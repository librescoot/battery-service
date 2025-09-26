package battery

import (
	"context"
	"fmt"
	"time"

	"battery-service/nfc/hal"

	"github.com/redis/go-redis/v9"
)

func NewBatteryReader(index int, role BatteryRole, deviceName string, logLevel int, service *Service) (*BatteryReader, error) {
	reader := &BatteryReader{
		index:      index,
		role:       role,
		deviceName: deviceName,
		logLevel:   logLevel,
		service:    service,

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

	reader.redis = redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("%s:%d", service.config.RedisServerAddress, service.config.RedisServerPort),
	})

	if err := reader.redis.Ping(context.TODO()).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis for reader %d: %v", index, err)
	}

	var err error
	reader.hal, err = hal.NewPN7150(deviceName, reader.makeLogCallback(), nil, true, false, service.debug)
	if err != nil {
		return nil, fmt.Errorf("failed to create NFC HAL for reader %d: %v", index, err)
	}

	reader.initializeFaultManagement()

	return reader, nil
}

func (r *BatteryReader) Start() error {
	r.service.logger.Printf("Starting battery reader %d on device %s", r.index, r.deviceName)

	r.service.logger.Printf("Battery %d: Initializing NFC reader on %s", r.index, r.deviceName)
	if err := r.hal.Initialize(); err != nil {
		return fmt.Errorf("failed to initialize NFC HAL for reader %d: %v", r.index, err)
	}
	r.service.logger.Printf("Battery %d: NFC reader initialized successfully", r.index)

	go r.run()

	return nil
}

func (r *BatteryReader) Stop() {
	r.service.logger.Printf("Stopping battery reader %d", r.index)
	close(r.stopChan)

	if r.stateTimer != nil {
		r.stateTimer.Stop()
	}
	r.clearHeartbeatTimer()

	r.cleanupFaultManagement()

	if r.redis != nil {
		r.redis.Close()
	}

	if r.hal != nil {
		r.service.logger.Printf("Battery %d: Deinitializing NFC reader", r.index)
		r.hal.Deinitialize()
	}

	r.service.logger.Printf("Battery reader %d stopped", r.index)
}

func (r *BatteryReader) run() {
	r.service.logger.Printf("Battery reader %d event loop started", r.index)

	r.sendStatusUpdate()

	r.transitionTo(StateInit)

	r.fetchInitialRedisState()

	r.checkInitComplete()

	go func() {
		time.Sleep(5 * time.Second)
		if r.isIn(StateInit) {
			r.service.logger.Printf("Battery %d: Initialization timeout, forcing start with defaults", r.index)
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
			r.service.logger.Printf("Battery reader %d event loop stopping", r.index)
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

		}
	}
}

func (r *BatteryReader) handleRestart() {
	inTagPresent := r.isIn(StateTagPresent)
	inCheckPresence := r.isIn(StateCheckPresence)
	r.service.logger.Printf("Battery %d: Restart requested - currentState=%s, inStateTagPresent=%t, inStateCheckPresence=%t",
		r.index, r.state, inTagPresent, inCheckPresence)

	if inTagPresent {
		r.service.logger.Printf("Battery %d: Restarting state machine from %s", r.index, r.state)
		r.transitionTo(StateTagPresent)
	} else {
		r.service.logger.Printf("Battery %d: Restart skipped - not in StateTagPresent (current: %s)", r.index, r.state)
	}
}

func (r *BatteryReader) handleTimeout() {
	switch r.state {
	case StateNFCReaderOff:

	case StateWaitArrival:
		r.handleDeparture()
		r.transitionTo(StateTagAbsent)

	case StateTagAbsent:
		r.checkForTags() // Discovery was started on state entry
		if r.state == StateTagAbsent {
			checkInterval := BMSTimeCheckReader // 10s default
			if !r.seatboxLockClosed {
				checkInterval = 1 * time.Second
			}
			r.setStateTimer(checkInterval)
		}

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
			r.checkForTags() // Check for tag departure during LED cycle
			r.transitionTo(StateSendOpened)
		}

	case StateSendClosed:
		r.transitionTo(StateSendOnOff)

	case StateSendOnOff:
		if r.readStatus() {
			r.checkForTags() // Check for tag departure during heartbeat
			r.transitionTo(StateCondStateOK)
		}

	case StateSendInsertedClosed:
		r.transitionTo(StateSendClosed)

	case StateWaitUpdate:
		r.transitionTo(StateHeartbeatActions)

	default:
		r.service.logger.Printf("Battery %d: Unhandled timeout in state %s", r.index, r.state)
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

	r.checkInitComplete()
}

func (r *BatteryReader) handleSeatboxLockChange(closed bool) {
	r.initComplete.SeatboxLock = true
	r.seatboxLockClosed = closed

	// Log state machine info on seatbox change
	r.service.logger.Printf("Battery %d: Seatbox %s - role=%s, state=%s, enabled=%t, inStateTagPresent=%t",
		r.index, map[bool]string{true: "closed", false: "opened"}[closed], r.role, r.state, r.enabled, r.isIn(StateTagPresent))

	if r.role == BatteryRoleActive {
		newEnabled := closed
		if r.enabled != newEnabled {
			r.service.logger.Printf("Battery %d: Active battery enabled state changing from %t to %t", r.index, r.enabled, newEnabled)
			r.enabled = newEnabled
			if r.isIn(StateTagPresent) {
				r.service.logger.Printf("Battery %d: Triggering restart due to enabled state change", r.index)
				r.triggerRestart()
			} else {
				r.service.logger.Printf("Battery %d: Not triggering restart - not in StateTagPresent (current state: %s)", r.index, r.state)
			}
		} else {
			r.service.logger.Printf("Battery %d: Active battery enabled state unchanged (enabled=%t)", r.index, r.enabled)
		}
	} else {
		r.service.logger.Printf("Battery %d: Inactive battery - skipping enabled state logic", r.index)
	}

	oldLatch := r.latchedSeatboxLockClosed
	r.service.logger.Printf("Battery %d: Latch logic - vehicleState=%s, oldLatch=%t, seatboxClosed=%t",
		r.index, r.vehicleState, oldLatch, closed)
	if r.vehicleState == VehicleStateReadyToDrive {
		r.latchedSeatboxLockClosed = closed
	} else if closed {
		r.latchedSeatboxLockClosed = true
	} else {
		r.latchedSeatboxLockClosed = false
	}
	r.service.logger.Printf("Battery %d: Latch updated from %t to %t", r.index, oldLatch, r.latchedSeatboxLockClosed)

	if r.latchedSeatboxLockClosed != oldLatch && r.isIn(StateTagPresent) {
		r.service.logger.Printf("Battery %d: Latch changed (%t -> %t) and in StateTagPresent - triggering restart",
			r.index, oldLatch, r.latchedSeatboxLockClosed)
		r.triggerRestart()
	} else if r.latchedSeatboxLockClosed != oldLatch {
		r.service.logger.Printf("Battery %d: Latch changed (%t -> %t) but NOT in StateTagPresent (current state: %s) - restart skipped",
			r.index, oldLatch, r.latchedSeatboxLockClosed, r.state)
	} else {
		r.service.logger.Printf("Battery %d: Latch unchanged (%t) - no restart needed", r.index, oldLatch)
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
		r.service.logger.Printf("Battery %d: Initialization complete, starting NFC operations", r.index)
		r.transitionTo(StateNFCReaderOn)
	} else {
		r.service.logger.Printf("Battery %d: Initialization pending - VehicleState: %t, SeatboxLock: %t",
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
	select {
	case r.enabledChan <- enabled:
	default:
		select {
		case <-r.enabledChan:
			r.enabledChan <- enabled
		default:
			r.enabledChan <- enabled
		}
	}
}

func (r *BatteryReader) SendVehicleStateChange(state VehicleState) {
	select {
	case r.vehicleStateChan <- state:
	default:
		select {
		case <-r.vehicleStateChan:
			r.vehicleStateChan <- state
		default:
			r.vehicleStateChan <- state
		}
	}
}

func (r *BatteryReader) SendSeatboxLockChange(closed bool) {
	select {
	case r.seatboxLockChan <- closed:
	default:
		select {
		case <-r.seatboxLockChan:
			r.seatboxLockChan <- closed
		default:
			r.seatboxLockChan <- closed
		}
	}
}

func (r *BatteryReader) fetchInitialRedisState() {
	r.service.logger.Printf("Battery %d: Fetching initial Redis state from hashes", r.index)

	vehicleState, err := r.redis.HGet(context.TODO(), "vehicle", "state").Result()
	if err == nil {
		r.service.logger.Printf("Battery %d: Found vehicle state: %s", r.index, vehicleState)
		r.handleVehicleStateChange(VehicleState(vehicleState))
	} else {
		r.service.logger.Printf("Battery %d: No vehicle state in Redis hash: %v", r.index, err)
	}

	seatboxLock, err := r.redis.HGet(context.TODO(), "vehicle", "seatbox:lock").Result()
	if err == nil {
		closed := (seatboxLock == "closed" || seatboxLock == "true" || seatboxLock == "1")
		r.service.logger.Printf("Battery %d: Found seatbox lock state: %s (closed=%t)", r.index, seatboxLock, closed)
		r.handleSeatboxLockChange(closed)
	} else {
		r.service.logger.Printf("Battery %d: No seatbox lock state in Redis hash: %v", r.index, err)
	}
}

func (r *BatteryReader) makeLogCallback() hal.LogCallback {
	return func(level hal.LogLevel, message string) {
		if int(level) <= r.logLevel {
			r.service.logger.Printf("Battery %d NFC: %s", r.index, message)
		}
	}
}
