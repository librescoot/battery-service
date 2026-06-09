package battery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"battery-service/battery/fsm"
	"github.com/librescoot/pn7150"
	"github.com/redis/go-redis/v9"
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

		faultStates: make(map[BMSFault]*FaultState),

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

	return reader, nil
}

func (r *BatteryReader) Start() error {
	r.logger.Info(fmt.Sprintf("Starting battery reader on device %s", r.deviceName))

	// Start the FSM synchronously so it is guaranteed to be in StateInit
	// before the event-loop / Redis-fetch goroutine starts pushing events.
	// Spawning fsm.Run as a goroutine in parallel with r.run() races: r.run()
	// can call checkInitComplete() before the FSM has entered StateInit, the
	// EvInitComplete event is then skipped, and the 5s timeout fallback fires
	// with broken defaults (notably seatboxLockClosed=false).
	if err := r.fsm.Start(r.fsmCtx); err != nil {
		return fmt.Errorf("start fsm: %w", err)
	}

	go r.run()

	return nil
}

func (r *BatteryReader) Stop() {
	r.logger.Info(fmt.Sprintf("Stopping battery reader %d", r.index))

	r.fsmCancel()

	close(r.stopChan)

	r.cleanupFaultManagement()

	if r.hal != nil {
		r.logger.Info("Deinitializing NFC reader")
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
		if r.initCompleteSent {
			return
		}
		// Only fill in defaults for state we never received. If Redis already
		// gave us a real value (initComplete.* already true), keep it — the
		// previous behaviour of unconditionally setting seatboxLockClosed=false
		// turned the FSM down the seatbox-open path and left an active
		// battery stuck in idle. See bean librescoot-5u06.
		if !r.initComplete.VehicleState {
			r.vehicleState = VehicleStateStandby
			r.initComplete.VehicleState = true
			r.logger.Warn("Initialization timeout, defaulting vehicle state to stand-by")
		}
		if !r.initComplete.SeatboxLock {
			// Conservative default: assume closed (the dominant resting state)
			// rather than the historical 'open', so a missing seatbox key
			// doesn't dump a freshly inserted battery down the idle path.
			r.seatboxLockClosed = true
			r.initComplete.SeatboxLock = true
			r.logger.Warn("Initialization timeout, defaulting seatbox lock to closed")
		}
		r.checkInitComplete()
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
	r.logger.Debug(fmt.Sprintf("Restart requested - currentState=%s", currentState))

	if r.fsm.IsInState(fsm.StateTagPresent) && !r.fsm.IsInState(fsm.StateCheckPresence) {
		r.logger.Debug("Sending restart event")
		r.fsm.SendEvent(fsm.EvRestart)
	} else {
		r.logger.Debug("Restart skipped - not in valid state")
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
		if r.fsm.IsInState(fsm.StateTagPresent) {
			r.triggerRestart()
		}
	}

	if oldVehicleState != VehicleStateReadyToDrive && newState != VehicleStateReadyToDrive {
		r.data.EmptyOr0Data = 0
	}

	r.checkInitComplete()
}

// nextLatchedSeatboxClosed returns the new latched seatbox-closed value used to
// gate battery power. While ready-to-drive an opening report is ignored (the
// latch holds its previous value) so a latch-sensor bounce on a rough road
// can't cut 48V to the motor mid-ride; only a closing report updates it.
// Leaving ready-to-drive with the seatbox physically open unwinds the latch in
// handleVehicleStateChange. Outside ready-to-drive the latch tracks the raw
// value, so this only changes the ready-to-drive case.
func nextLatchedSeatboxClosed(vehicleState VehicleState, currentLatch, rawClosed bool) bool {
	if vehicleState != VehicleStateReadyToDrive || (rawClosed && !currentLatch) {
		return rawClosed
	}
	return currentLatch
}

func (r *BatteryReader) handleSeatboxLockChange(closed bool) {
	r.initComplete.SeatboxLock = true
	r.seatboxLockClosed = closed

	oldLatch := r.latchedSeatboxLockClosed
	r.latchedSeatboxLockClosed = nextLatchedSeatboxClosed(r.vehicleState, oldLatch, closed)
	latchClosed := r.latchedSeatboxLockClosed

	r.logger.Debug(fmt.Sprintf("Seatbox %s (latched %s) - role=%s, state=%s, enabled=%t",
		map[bool]string{true: "closed", false: "opened"}[closed],
		map[bool]string{true: "closed", false: "opened"}[latchClosed],
		r.role, r.fsm.State(), r.enabled))

	if r.role == BatteryRoleActive {
		var newEnabled bool
		switch {
		case r.voltageDeltaBlocked:
			newEnabled = false
		case r.service.config.EffectiveKeepActiveOnSeatboxOpen():
			newEnabled = true
			if !latchClosed {
				r.logger.Info("Seatbox opened but battery staying active (keep-active-on-seatbox-open)")
			}
		default:
			newEnabled = latchClosed
		}
		if r.enabled != newEnabled {
			r.logger.Debug(fmt.Sprintf("Active battery enabled state changing from %t to %t", r.enabled, newEnabled))
			r.enabled = newEnabled
			if r.fsm.IsInState(fsm.StateTagPresent) {
				r.logger.Debug("Triggering restart due to enabled state change")
				r.triggerRestart()
			}
		}
	}

	// With keep-active-on-seatbox-open, don't restart a running battery on
	// latch change; the FSM handles seatbox events directly and a restart
	// would walk the battery through StateSendOff and briefly power it down.
	if latchClosed != oldLatch && r.fsm.IsInState(fsm.StateTagPresent) && !r.service.config.EffectiveKeepActiveOnSeatboxOpen() {
		r.logger.Debug(fmt.Sprintf("Latch changed (%t -> %t) and in StateTagPresent - triggering restart",
			oldLatch, latchClosed))
		r.triggerRestart()
	}

	if r.vehicleState != VehicleStateReadyToDrive || !latchClosed {
		r.data.EmptyOr0Data = 0
	}

	if !latchClosed && oldLatch {
		r.fsm.SendEvent(fsm.EvSeatboxOpened)
	} else if latchClosed && !oldLatch {
		r.fsm.SendEvent(fsm.EvSeatboxClosed)
	}

	r.checkInitComplete()
}

func (r *BatteryReader) handleEnabledChange(enabled bool) {
	if r.enabled != enabled {
		r.enabled = enabled
		if r.fsm.IsInState(fsm.StateTagPresent) {
			r.triggerRestart()
		}
	}
}

func (r *BatteryReader) checkInitComplete() {
	if r.initCompleteSent {
		return
	}
	if r.initComplete.VehicleState && r.initComplete.SeatboxLock {
		r.initCompleteSent = true
		r.fsm.SendEvent(fsm.EvInitComplete)
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
	r.logger.Debug("Fetching initial Redis state from hashes")

	vehicleState, err := r.service.redis.HGet(r.ctx, "vehicle", "state").Result()
	switch {
	case err == nil:
		r.logger.Debug(fmt.Sprintf("Found vehicle state: %s", vehicleState))
		r.handleVehicleStateChange(VehicleState(vehicleState))
	case errors.Is(err, redis.Nil):
		// vehicle-service hasn't written state yet — expected on cold boot.
		r.logger.Debug("No vehicle state in Redis hash yet")
	default:
		r.logger.Warn(fmt.Sprintf("Failed to read vehicle state from Redis: %v", err))
	}

	seatboxLock, err := r.service.redis.HGet(r.ctx, "vehicle", "seatbox:lock").Result()
	switch {
	case err == nil:
		closed := (seatboxLock == "closed")
		r.logger.Debug(fmt.Sprintf("Found seatbox lock state: %s (closed=%t)", seatboxLock, closed))
		r.handleSeatboxLockChange(closed)
	case errors.Is(err, redis.Nil):
		r.logger.Debug("No seatbox lock state in Redis hash yet")
	default:
		r.logger.Warn(fmt.Sprintf("Failed to read seatbox lock state from Redis: %v", err))
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
	r.logger.Debug(fmt.Sprintf("Tag departure on reader %d, last serial=%s", r.index, r.data.SerialNumber))
	r.data = BMSData{Present: false}

	// The pack is gone, so clear every pack-reported fault below the
	// reader-error fault, whether already active (remove it from Redis) or
	// still debouncing (cancel the pending set). The reader-error fault tracks
	// the reader itself and is managed by init/deinit, not by tag departure.
	r.faultMu.Lock()
	for fault := range r.faultStates {
		if fault < BMSFaultNFCReaderError {
			r.setFaultLocked(fault, false)
		}
	}
	r.faultMu.Unlock()

	r.sendStatusUpdate()
}
