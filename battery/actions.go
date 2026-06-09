package battery

import (
	"fmt"
	"time"

	"battery-service/battery/fsm"
)

func (r *BatteryReader) TakeInhibitor() {
	if err := r.takeInhibitor(); err != nil {
		r.logger.Error(fmt.Sprintf("Failed to acquire suspend inhibitor: %v", err))
	}
}

func (r *BatteryReader) ReleaseInhibitor() {
	r.releaseInhibitor()
}

func (r *BatteryReader) StartDiscovery() error {
	r.nfcMu.Lock()
	defer r.nfcMu.Unlock()

	if !r.startDiscovery() {
		return fmt.Errorf("battery %d: failed to start discovery", r.index)
	}
	return nil
}

func (r *BatteryReader) StopDiscovery() {
	r.nfcMu.Lock()
	defer r.nfcMu.Unlock()

	r.stopDiscovery()
}

func (r *BatteryReader) SelectTag() {
	r.nfcMu.Lock()
	defer r.nfcMu.Unlock()

	if err := r.hal.SelectTag(0); err != nil {
		r.logger.Warn(fmt.Sprintf("Failed to select tag: %v", err))
		r.handleNFCError(err)
	}
}

func (r *BatteryReader) PollForTagArrival() bool {
	return r.pollForTagArrival()
}

func (r *BatteryReader) Initialize() error {
	r.nfcMu.Lock()
	defer r.nfcMu.Unlock()

	r.logger.Info(fmt.Sprintf("Initializing NFC reader on %s", r.deviceName))
	if err := r.hal.Initialize(); err != nil {
		r.logger.Error(fmt.Sprintf("Initialization failed: %v", err))
		return err
	}

	// Do NOT enable async tag event reader
	// r.hal.SetTagEventReaderEnabled(true)

	r.logger.Info("NFC reader initialized successfully")
	r.clearNFCReaderError()
	return nil
}

func (r *BatteryReader) Deinitialize() {
	r.nfcMu.Lock()
	defer r.nfcMu.Unlock()

	r.deinitializeNFC()
	r.clearHeartbeatTimer()
	r.raiseNFCReaderError()
}

func (r *BatteryReader) ReadStatus() error {
	if !r.readStatus() {
		return fmt.Errorf("battery %d: failed to read status", r.index)
	}
	return nil
}

func (r *BatteryReader) SendCheckPresenceReady() {
	if r.fsm != nil {
		r.fsm.SendEvent(fsm.EvCheckPresenceReady)
	}
}

func (r *BatteryReader) GetEnabled() bool {
	return r.enabled
}

// GetSeatboxLockClosed returns the latched seatbox-closed value (not the raw
// sensor) so the FSM's seatbox gate holds across a latch-sensor bounce while
// ready-to-drive. See nextLatchedSeatboxClosed.
func (r *BatteryReader) GetSeatboxLockClosed() bool {
	return r.latchedSeatboxLockClosed
}

func (r *BatteryReader) GetVehicleActive() bool {
	return r.vehicleState == VehicleStateReadyToDrive
}

func (r *BatteryReader) CheckStateCorrect() bool {
	if r.enabled {
		if r.data.State != BMSStateActive {
			r.logger.Warn("State mismatch", "expected", BMSStateActive, "got", r.data.State)
			return false
		}
		return true
	}

	if r.data.State != BMSStateIdle && r.data.State != BMSStateAsleep {
		r.logger.Warn("State mismatch", "expected", "idle or asleep", "got", r.data.State)
		return false
	}
	return true
}

func (r *BatteryReader) GetRemainingCmdTime() time.Duration {
	elapsed := time.Since(r.lastCmdTime)
	if elapsed >= BMSTimeCmd {
		return 0
	}
	return BMSTimeCmd - elapsed
}

// GetOpenedTime returns the delay between the SEATBOX_OPENED and the following
// INSERTED_IN_SCOOTER command in the seatbox-open maintenance loop:
//   - just inserted: BMSTimeCmd, light the LED ring as fast as possible;
//   - else first open of an already-present pack: mirror the wake-up time, 3s
//     if the BMS is asleep (disabled), 2s otherwise;
//   - else steady-state: the slow maintenance rate.
//
// justInserted/justOpened are owned by the FSM and passed in; the previous
// version read a reader field that was never set, so the first-open delay
// never applied.
func (r *BatteryReader) GetOpenedTime(justInserted, justOpened bool) time.Duration {
	switch {
	case justInserted:
		return BMSTimeCmd
	case justOpened:
		if r.data.State == BMSStateAsleep {
			return BMSTimeCmdFirstOpenedAsleep
		}
		return BMSTimeCmdFirstOpenedAwake
	default:
		return BMSTimeCmdSlow
	}
}

// GetInsertedTime returns the delay after the INSERTED_IN_SCOOTER command:
// BMSTimeCmd on the first pass to re-light the ring quickly, the slow
// maintenance rate thereafter.
func (r *BatteryReader) GetInsertedTime(justInserted bool) time.Duration {
	if justInserted {
		return BMSTimeCmd
	}
	return BMSTimeCmdSlow
}

func (r *BatteryReader) IsInactive() bool {
	return r.data.State != BMSStateActive
}

func (r *BatteryReader) ZeroRetryCounters() {
	r.data.EmptyOr0Data = 0
}

func (r *BatteryReader) StopHeartbeatTimer() {
	r.heartbeatRunning = false
	if r.heartbeatTimer != nil {
		r.heartbeatTimer.Stop()
		r.heartbeatTimer = nil
	}
}

func (r *BatteryReader) StartHeartbeatTimer() {
	r.heartbeatRunning = true
	interval := r.GetHeartbeatInterval()

	if r.heartbeatTimer != nil {
		r.heartbeatTimer.Stop()
	}

	// Timer callback only sends event - state check happens in FSM goroutine
	// This avoids race conditions between timer goroutine and FSM/event loop
	r.heartbeatTimer = time.AfterFunc(interval, func() {
		if r.fsm != nil {
			r.fsm.SendEvent(fsm.EvHeartbeatTimeout)
		}
	})
}

func (r *BatteryReader) ClearHeartbeatTimer() {
	r.StopHeartbeatTimer()
}

func (r *BatteryReader) StopTimerIfBatteryEmpty() {
	if r.data.Charge == 0 {
		r.logger.Debug("Battery empty (charge=0), stopping heartbeat timer")
		r.StopHeartbeatTimer()
	}
}

func (r *BatteryReader) IsRoleInactive() bool {
	return r.role == BatteryRoleInactive
}

func (r *BatteryReader) GetHeartbeatInterval() time.Duration {
	if r.role == BatteryRoleInactive {
		return r.service.config.OffUpdateTime
	}
	return r.service.config.HeartbeatTimeout
}

func (r *BatteryReader) updateLastCmdTime() {
	r.lastCmdTime = time.Now()
}

func (r *BatteryReader) ShouldKeepActiveOnSeatboxOpen() bool {
	// Inactive-role slots never run the keep-active path: the wake-up cycle
	// lights the battery LED, which we don't want on a slot we're not driving.
	if r.role != BatteryRoleActive {
		return false
	}
	return r.service.config.EffectiveKeepActiveOnSeatboxOpen()
}
