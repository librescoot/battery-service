package battery

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"battery-service/battery/fsm"
	"battery-service/nfc/hal"
)

func NewBatteryReader(index int, role BatteryRole, deviceName string, logLevel int, service *Service) (*BatteryReader, error) {
	reader := &BatteryReader{
		index:      index,
		role:       role,
		deviceName: deviceName,
		logLevel:   logLevel,
		service:    service,
		ctx:        service.ctx,

		stopChan:         make(chan struct{}),
		restartChan:      make(chan struct{}, 1),
		vehicleStateChan: make(chan VehicleState, 1),
		seatboxLockChan:  make(chan bool, 1),
		enabledChan:      make(chan bool, 1),

		data: BMSData{
			Present: false,
		},

		faultDebounceTimers: make(map[BMSFault]*time.Timer),
		faultStates:         make(map[BMSFault]*FaultState),

		lastCmdTime: time.Now(),

		enabled: (role == BatteryRoleActive),
	}

	reader.logger = slog.New(NewFSMHandler(service.stdLogger.Writer(), LogLevel(logLevel), index))

	var err error
	reader.hal, err = hal.NewPN7150(deviceName, reader.makeLogCallback(), nil, true, false, service.debug)
	if err != nil {
		return nil, fmt.Errorf("failed to create NFC HAL for reader %d: %v", index, err)
	}

	reader.initializeFaultManagement()

	fsmLogger := slog.New(NewFSMHandler(service.stdLogger.Writer(), LogLevel(logLevel), index))

	reader.fsmCtx, reader.fsmCancel = context.WithCancel(reader.ctx)
	reader.fsm = fsm.New(reader, fsmLogger)

	// Do NOT use async tag event channel
	// reader.tagEventChan = reader.hal.GetTagEventChannel()

	return reader, nil
}

func (r *BatteryReader) Start() error {
	r.logger.Info(fmt.Sprintf("Starting battery reader on device %s", r.deviceName))

	go r.fsm.Run(r.fsmCtx)

	go r.run()

	return nil
}

func (r *BatteryReader) Stop() {
	r.logger.Info(fmt.Sprintf("Stopping battery reader %d", r.index))

	r.fsmCancel()

	close(r.stopChan)

	r.cleanupFaultManagement()

	if r.hal != nil {
		r.logger.Info(fmt.Sprintf("Deinitializing NFC reader"))
		r.hal.Deinitialize()
	}

	r.logger.Info(fmt.Sprintf("Battery reader %d stopped", r.index))
}

func (r *BatteryReader) run() {
	r.logger.Debug(fmt.Sprintf("Battery reader %d event loop started", r.index))

	r.sendStatusUpdate()

	r.fetchInitialRedisState()

	r.checkInitComplete()

	go func() {
		time.Sleep(5 * time.Second)
		if r.fsm.State() == fsm.StateInit {
			r.logger.Warn(fmt.Sprintf("Initialization timeout, forcing start with defaults"))
			r.initComplete.VehicleState = true
			r.initComplete.SeatboxLock = true
			r.vehicleState = VehicleStateStandby
			r.seatboxLockClosed = false
			r.checkInitComplete()
		}
	}()

	for {
		select {
		case <-r.stopChan:
			r.logger.Debug(fmt.Sprintf("Battery reader %d event loop stopping", r.index))
			return

		case <-r.restartChan:
			r.handleRestart()

		case vehicleState := <-r.vehicleStateChan:
			r.handleVehicleStateChange(vehicleState)

		case seatboxLockClosed := <-r.seatboxLockChan:
			r.handleSeatboxLockChange(seatboxLockClosed)

		case enabled := <-r.enabledChan:
			r.handleEnabledChange(enabled)
		}
	}
}

func (r *BatteryReader) handleRestart() {
	currentState := r.fsm.State()
	r.logger.Debug(fmt.Sprintf("Restart requested - currentState=%s",currentState))

	if r.isInHierarchy(fsm.StateTagPresent) && !r.isInHierarchy(fsm.StateCheckPresence) {
		r.logger.Debug(fmt.Sprintf("Sending restart event"))
		r.fsm.SendEvent(fsm.RestartEvent{})
	} else {
		r.logger.Debug(fmt.Sprintf("Restart skipped - not in valid state"))
	}
}

func (r *BatteryReader) handleVehicleStateChange(newState VehicleState) {
	r.initComplete.VehicleState = true
	oldVehicleState := r.vehicleState
	r.vehicleState = newState

	if oldVehicleState == VehicleStateReadyToDrive &&
		r.vehicleState != VehicleStateReadyToDrive &&
		!r.seatboxLockClosed &&
		r.latchedSeatboxLockClosed {
		r.latchedSeatboxLockClosed = false
		if r.isInHierarchy(fsm.StateTagPresent) {
			r.triggerRestart()
		}
	}

	if oldVehicleState != VehicleStateReadyToDrive && newState != VehicleStateReadyToDrive {
		r.data.EmptyOr0Data = 0
	}

	r.checkInitComplete()
}

func (r *BatteryReader) handleSeatboxLockChange(closed bool) {
	r.initComplete.SeatboxLock = true
	oldSeatboxLockClosed := r.seatboxLockClosed
	r.seatboxLockClosed = closed

	r.logger.Debug(fmt.Sprintf("Seatbox %s - role=%s, state=%s, enabled=%t",
		map[bool]string{true: "closed", false: "opened"}[closed], r.role, r.fsm.State(), r.enabled))

	if r.role == BatteryRoleActive {
		var newEnabled bool
		if r.service.config.DangerouslyIgnoreSeatbox {
			newEnabled = true
			if !closed {
				r.logger.Warn(fmt.Sprintf("Seatbox opened but battery staying active (--dangerously-ignore-seatbox)"))
			}
		} else {
			newEnabled = closed
		}
		if r.enabled != newEnabled {
			r.logger.Debug(fmt.Sprintf("Active battery enabled state changing from %t to %t",r.enabled, newEnabled))
			r.enabled = newEnabled
			if r.isInHierarchy(fsm.StateTagPresent) {
				r.logger.Debug(fmt.Sprintf("Triggering restart due to enabled state change"))
				r.triggerRestart()
			}
		}
	}

	oldLatch := r.latchedSeatboxLockClosed
	if r.vehicleState == VehicleStateReadyToDrive {
		r.latchedSeatboxLockClosed = closed
	} else if closed {
		r.latchedSeatboxLockClosed = true
	} else {
		r.latchedSeatboxLockClosed = false
	}

	if r.latchedSeatboxLockClosed != oldLatch && r.isInHierarchy(fsm.StateTagPresent) {
		r.logger.Debug(fmt.Sprintf("Latch changed (%t -> %t) and in StateTagPresent - triggering restart",
			oldLatch, r.latchedSeatboxLockClosed))
		r.triggerRestart()
	}

	if r.vehicleState != VehicleStateReadyToDrive || !r.latchedSeatboxLockClosed {
		r.data.EmptyOr0Data = 0
	}

	if !closed && oldSeatboxLockClosed {
		r.fsm.SendEvent(fsm.SeatboxOpenedEvent{})
	} else if closed && !oldSeatboxLockClosed {
		r.fsm.SendEvent(fsm.SeatboxClosedEvent{})
	}

	r.checkInitComplete()
}

func (r *BatteryReader) handleEnabledChange(enabled bool) {
	if r.enabled != enabled {
		r.enabled = enabled
		if r.isInHierarchy(fsm.StateTagPresent) {
			r.triggerRestart()
		}
	}
}

func (r *BatteryReader) checkInitComplete() {
	if r.initComplete.VehicleState &&
		r.initComplete.SeatboxLock &&
		r.fsm.State() == fsm.StateInit {
		r.logger.Info(fmt.Sprintf("Initialization complete, starting NFC operations"))
		r.fsm.SendEvent(fsm.InitCompleteEvent{})
	}
}

func (r *BatteryReader) triggerRestart() {
	select {
	case r.restartChan <- struct{}{}:
	default:
	}
}

func (r *BatteryReader) isInHierarchy(target fsm.State) bool {
	current := r.fsm.State()
	for current != fsm.StateRoot {
		if current == target {
			return true
		}
		current = current.Parent()
	}
	return target == fsm.StateRoot
}

func (r *BatteryReader) SetEnabled(enabled bool) {
	tryUpdateChannel(r.enabledChan, enabled)
}

func (r *BatteryReader) SendVehicleStateChange(state VehicleState) {
	tryUpdateChannel(r.vehicleStateChan, state)
}

func (r *BatteryReader) SendSeatboxLockChange(closed bool) {
	tryUpdateChannel(r.seatboxLockChan, closed)
}

func (r *BatteryReader) fetchInitialRedisState() {
	r.logger.Debug(fmt.Sprintf("Fetching initial Redis state from hashes"))

	vehicleState, err := r.service.redis.HGet(r.ctx, "vehicle", "state").Result()
	if err == nil {
		r.logger.Debug(fmt.Sprintf("Found vehicle state: %s",vehicleState))
		r.handleVehicleStateChange(VehicleState(vehicleState))
	} else {
		r.logger.Warn(fmt.Sprintf("No vehicle state in Redis hash: %v",err))
	}

	seatboxLock, err := r.service.redis.HGet(r.ctx, "vehicle", "seatbox:lock").Result()
	if err == nil {
		closed := (seatboxLock == "closed" || seatboxLock == "true" || seatboxLock == "1")
		r.logger.Debug(fmt.Sprintf("Found seatbox lock state: %s (closed=%t)",seatboxLock, closed))
		r.handleSeatboxLockChange(closed)
	} else {
		r.logger.Warn(fmt.Sprintf("No seatbox lock state in Redis hash: %v",err))
	}
}

func (r *BatteryReader) makeLogCallback() hal.LogCallback {
	return func(level hal.LogLevel, message string) {
		if int(level) > r.logLevel {
			return
		}

		var levelPrefix string
		switch level {
		case hal.LogLevelError:
			levelPrefix = "ERROR: "
		case hal.LogLevelWarning:
			levelPrefix = "WARN: "
		case hal.LogLevelInfo:
			levelPrefix = ""
		case hal.LogLevelDebug:
			levelPrefix = "DEBUG: "
		}

		msg := fmt.Sprintf("Battery %d: NFC: %s%s", r.index, levelPrefix, message)
		r.service.stdLogger.Printf("%s", msg)
	}
}

func (r *BatteryReader) handleDeparture() {
	r.data.Present = false

	// Cancel any pending fault timers to prevent activation after departure
	for _, state := range r.faultStates {
		if state.SetTimer != nil {
			state.SetTimer.Stop()
			state.SetTimer = nil
			state.PendingSet = false
		}
	}

	r.sendStatusUpdate()
}

func (r *BatteryReader) handleTagEvent(event hal.TagEvent) {
	if event.Type == hal.TagArrival {
		r.logger.Debug(fmt.Sprintf("Tag arrival event from async reader"))
		r.tagsDiscovered = true
		r.previousTagPresent = true

		if !r.isInHierarchy(fsm.StateTagPresent) {
			r.fsm.SendEvent(fsm.TagArrivedEvent{})
		}
	} else if event.Type == hal.TagDeparture {
		r.logger.Debug(fmt.Sprintf("Tag departure event from async reader"))
		r.tagsDiscovered = false
		r.previousTagPresent = false

		if r.isInHierarchy(fsm.StateTagPresent) {
			r.handleDeparture()
			r.fsm.SendEvent(fsm.TagDepartedEvent{})
		}
	}
}
