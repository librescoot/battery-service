package battery

import (
	"fmt"
	"strings"
	"time"

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

		state: StateRoot,
		data: BMSData{
			Present: false,
		},

		faultDebounceTimers: make(map[BMSFault]*time.Timer),
		faultStates:         make(map[BMSFault]*FaultState),

		lastCmdTime: time.Now(),

		enabled: (role == BatteryRoleActive),
	}

	var err error
	reader.hal, err = hal.NewPN7150(deviceName, reader.makeLogCallback(), nil, true, false, service.debug)
	if err != nil {
		return nil, fmt.Errorf("failed to create NFC HAL for reader %d: %v", index, err)
	}

	reader.tagEventChan = reader.hal.GetTagEventChannel()
	reader.hal.SetTagEventReaderEnabled(true)

	reader.initializeFaultManagement()

	return reader, nil
}

func (r *BatteryReader) Start() error {
	r.service.logger.Infof("Starting battery reader %d on device %s", r.index, r.deviceName)

	r.service.logger.Infof("Battery %d: Initializing NFC reader on %s", r.index, r.deviceName)
	if err := r.hal.Initialize(); err != nil {
		// Check if this is a cold boot semantic error
		if strings.Contains(err.Error(), "status: 06") || strings.Contains(err.Error(), "TOTAL_DURATION") {
			r.service.logger.Warnf("Battery %d: Initial initialization failed (likely cold boot), attempting reinitialization", r.index)
			// Try a full reinitialization with power cycle
			if reinitErr := r.hal.FullReinitialize(); reinitErr != nil {
				return fmt.Errorf("failed to reinitialize NFC HAL for reader %d after cold boot: %v", r.index, reinitErr)
			}
			r.service.logger.Infof("Battery %d: NFC reader reinitialized successfully after cold boot", r.index)
		} else {
			return fmt.Errorf("failed to initialize NFC HAL for reader %d: %v", r.index, err)
		}
	} else {
		r.service.logger.Infof("Battery %d: NFC reader initialized successfully", r.index)
	}

	// Clear NFC reader error fault on successful initialization
	r.setFault(BMSFaultNFCReaderError, false)

	go r.run()

	return nil
}

func (r *BatteryReader) Stop() {
	r.service.logger.Infof("Stopping battery reader %d", r.index)
	close(r.stopChan)

	if r.stateTimer != nil {
		r.stateTimer.Stop()
	}
	r.clearHeartbeatTimer()

	r.cleanupFaultManagement()

	if r.hal != nil {
		r.service.logger.Infof("Battery %d: Deinitializing NFC reader", r.index)
		r.hal.Deinitialize()
	}

	r.service.logger.Infof("Battery reader %d stopped", r.index)
}

func (r *BatteryReader) run() {
	r.service.logger.Debugf("Battery reader %d event loop started", r.index)

	r.sendStatusUpdate()

	r.transitionTo(StateInit)

	r.fetchInitialRedisState()

	r.checkInitComplete()

	go func() {
		time.Sleep(5 * time.Second)
		if r.isIn(StateInit) {
			r.service.logger.Warnf("Battery %d: Initialization timeout, forcing start with defaults", r.index)
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
			r.service.logger.Debugf("Battery reader %d event loop stopping", r.index)
			return

		case <-r.restartChan:
			r.handleRestart()

		case <-r.getStateTimer():
			r.handleTimeout()

		case vehicleState := <-r.vehicleStateChan:
			r.handleVehicleStateChange(vehicleState)

		case seatboxLockClosed := <-r.seatboxLockChan:
			r.handleSeatboxLockChange(seatboxLockClosed)

		case enabled := <-r.enabledChan:
			r.handleEnabledChange(enabled)

		case event := <-r.tagEventChan:
			r.handleTagEvent(event)

		}
	}
}

func (r *BatteryReader) handleRestart() {
	inTagPresent := r.isIn(StateTagPresent)
	inCheckPresence := r.isIn(StateCheckPresence)
	r.service.logger.Debugf("Battery %d: Restart requested - currentState=%s, inStateTagPresent=%t, inStateCheckPresence=%t",
		r.index, r.state, inTagPresent, inCheckPresence)

	if inTagPresent {
		r.service.logger.Debugf("Battery %d: Restarting state machine from %s", r.index, r.state)
		r.transitionTo(StateTagPresent)
	} else {
		r.service.logger.Debugf("Battery %d: Restart skipped - not in StateTagPresent (current: %s)", r.index, r.state)
	}
}

func (r *BatteryReader) handleTagEvent(event hal.TagEvent) {
	switch event.Type {
	case hal.TagArrival:
		if event.Tag != nil {
			r.service.logger.Infof("Battery %d: Tag arrived: %X", r.index, event.Tag.ID)
		} else {
			r.service.logger.Infof("Battery %d: Tag arrived", r.index)
		}
		r.service.logger.Debugf("Battery %d: Processing tag arrival", r.index)
		if r.isIn(StateDiscoverTag) {
			r.justInserted = true
			r.previousTagPresent = true
			r.transitionTo(StateTagPresent)
		}

	case hal.TagDeparture:
		if event.Tag != nil {
			r.service.logger.Infof("Battery %d: Tag departed: %X", r.index, event.Tag.ID)
		} else {
			r.service.logger.Infof("Battery %d: Tag departed", r.index)
		}
		if r.isIn(StateTagPresent) {
			r.handleDeparture()
			r.transitionTo(StateDiscoverTag)
		}
		r.previousTagPresent = false
	}
}

func (r *BatteryReader) handleTimeout() {
	switch r.state {
	case StateNFCReaderOff:

	case StateWaitArrival:
		r.handleDeparture()
		r.transitionTo(StateTagAbsent)

	case StateTagAbsent:
		r.setStateTimer(BMSTimeCheckReader)

	case StateCheckPresence:
		r.transitionTo(StateCondCheckPresence)

	case StateWaitLastCmd:
		r.transitionTo(StateCondSeatboxLock)

	case StateSendOff:
		r.readStatus()
		r.transitionTo(StateCondOff)

	case StateSendOpened:
		r.justOpened = false
		r.transitionTo(StateSendInsertedOpen)

	case StateSendInsertedOpen:
		r.justInserted = false
		if r.readStatus() {
			r.transitionTo(StateSendOpened)
		} else {
			r.transitionTo(StateCheckPresence)
		}

	case StateSendClosed:
		r.transitionTo(StateSendOnOff)

	case StateSendOnOff:
		if r.readStatus() {
			r.transitionTo(StateCondStateOK)
		} else {
			r.transitionTo(StateCheckPresence)
		}

	case StateSendInsertedClosed:
		r.transitionTo(StateSendClosed)

	case StateWaitUpdate:
		r.transitionTo(StateHeartbeatActions)

	default:
		r.service.logger.Warnf("Battery %d: Unhandled timeout in state %s", r.index, r.state)
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
		if r.isIn(StateTagPresent) {
			r.triggerRestart()
		}
	}

	// Reset recovery counter on vehicle state change (except in/out of ready-to-drive)
	if oldVehicleState != VehicleStateReadyToDrive && newState != VehicleStateReadyToDrive {
		r.data.EmptyOr0Data = 0
	}

	r.checkInitComplete()
}

func (r *BatteryReader) handleSeatboxLockChange(closed bool) {
	r.initComplete.SeatboxLock = true
	r.seatboxLockClosed = closed

	// Log state machine info on seatbox change
	r.service.logger.Debugf("Battery %d: Seatbox %s - role=%s, state=%s, enabled=%t, inStateTagPresent=%t",
		r.index, map[bool]string{true: "closed", false: "opened"}[closed], r.role, r.state, r.enabled, r.isIn(StateTagPresent))

	if r.role == BatteryRoleActive {
		newEnabled := closed
		if r.enabled != newEnabled {
			r.service.logger.Debugf("Battery %d: Active battery enabled state changing from %t to %t", r.index, r.enabled, newEnabled)
			r.enabled = newEnabled
			if r.isIn(StateTagPresent) {
				r.service.logger.Debugf("Battery %d: Triggering restart due to enabled state change", r.index)
				r.triggerRestart()
			} else {
				r.service.logger.Debugf("Battery %d: Not triggering restart - not in StateTagPresent (current state: %s)", r.index, r.state)
			}
		} else {
			r.service.logger.Debugf("Battery %d: Active battery enabled state unchanged (enabled=%t)", r.index, r.enabled)
		}
	} else {
		r.service.logger.Debugf("Battery %d: Inactive battery - skipping enabled state logic", r.index)
	}

	oldLatch := r.latchedSeatboxLockClosed
	r.service.logger.Debugf("Battery %d: Latch logic - vehicleState=%s, oldLatch=%t, seatboxClosed=%t",
		r.index, r.vehicleState, oldLatch, closed)
	if r.vehicleState == VehicleStateReadyToDrive {
		r.latchedSeatboxLockClosed = closed
	} else if closed {
		r.latchedSeatboxLockClosed = true
	} else {
		r.latchedSeatboxLockClosed = false
	}
	r.service.logger.Debugf("Battery %d: Latch updated from %t to %t", r.index, oldLatch, r.latchedSeatboxLockClosed)

	if r.latchedSeatboxLockClosed != oldLatch && r.isIn(StateTagPresent) {
		r.service.logger.Debugf("Battery %d: Latch changed (%t -> %t) and in StateTagPresent - triggering restart",
			r.index, oldLatch, r.latchedSeatboxLockClosed)
		r.triggerRestart()
	} else if r.latchedSeatboxLockClosed != oldLatch {
		r.service.logger.Debugf("Battery %d: Latch changed (%t -> %t) but NOT in StateTagPresent (current state: %s) - restart skipped",
			r.index, oldLatch, r.latchedSeatboxLockClosed, r.state)
	} else {
		r.service.logger.Debugf("Battery %d: Latch unchanged (%t) - no restart needed", r.index, oldLatch)
	}

	// Reset recovery counter on seatbox change (if not latched in ready-to-drive)
	if r.vehicleState != VehicleStateReadyToDrive || !r.latchedSeatboxLockClosed {
		r.data.EmptyOr0Data = 0
	}

	r.checkInitComplete()
}

func (r *BatteryReader) handleEnabledChange(enabled bool) {
	if r.enabled != enabled {
		r.enabled = enabled
		if r.isIn(StateTagPresent) {
			r.triggerRestart()
		}
	}
}

func (r *BatteryReader) checkInitComplete() {
	if r.initComplete.VehicleState &&
		r.initComplete.SeatboxLock &&
		r.isIn(StateInit) {
		r.service.logger.Infof("Battery %d: Initialization complete, starting NFC operations", r.index)
		r.transitionTo(StateNFCReaderOn)
	} else {
		r.service.logger.Debugf("Battery %d: Initialization pending - VehicleState: %t, SeatboxLock: %t",
			r.index, r.initComplete.VehicleState, r.initComplete.SeatboxLock)
	}
}

func (r *BatteryReader) triggerRestart() {
	select {
	case r.restartChan <- struct{}{}:
	default:
	}
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
	r.service.logger.Debugf("Battery %d: Fetching initial Redis state from hashes", r.index)

	vehicleState, err := r.service.redis.HGet(r.ctx, "vehicle", "state").Result()
	if err == nil {
		r.service.logger.Debugf("Battery %d: Found vehicle state: %s", r.index, vehicleState)
		r.handleVehicleStateChange(VehicleState(vehicleState))
	} else {
		r.service.logger.Warnf("Battery %d: No vehicle state in Redis hash: %v", r.index, err)
	}

	seatboxLock, err := r.service.redis.HGet(r.ctx, "vehicle", "seatbox:lock").Result()
	if err == nil {
		closed := (seatboxLock == "closed" || seatboxLock == "true" || seatboxLock == "1")
		r.service.logger.Debugf("Battery %d: Found seatbox lock state: %s (closed=%t)", r.index, seatboxLock, closed)
		r.handleSeatboxLockChange(closed)
	} else {
		r.service.logger.Warnf("Battery %d: No seatbox lock state in Redis hash: %v", r.index, err)
	}
}

func (r *BatteryReader) makeLogCallback() hal.LogCallback {
	return func(level hal.LogLevel, message string) {
		if int(level) > r.logLevel {
			return
		}

		msg := fmt.Sprintf("Battery %d NFC: %s", r.index, message)

		switch level {
		case hal.LogLevelError:
			r.service.stdLogger.Printf("ERROR: %s", msg)
		case hal.LogLevelWarning:
			r.service.stdLogger.Printf("WARN: %s", msg)
		case hal.LogLevelInfo:
			r.service.stdLogger.Printf("%s", msg)
		case hal.LogLevelDebug:
			r.service.stdLogger.Printf("DEBUG: %s", msg)
		}
	}
}
