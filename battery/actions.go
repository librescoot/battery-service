package battery

import (
	"fmt"
	"time"

	"battery-service/battery/fsm"
)

func (r *BatteryReader) TakeInhibitor() {
	r.takeInhibitor()
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
		r.logger.Warn(fmt.Sprintf("Failed to select tag: %v",err))
		r.handleNFCError(err)
	}
}

func (r *BatteryReader) PollForTagArrival() {
	r.pollForTagArrival()
}

func (r *BatteryReader) Initialize() error {
	r.nfcMu.Lock()
	defer r.nfcMu.Unlock()

	r.logger.Info(fmt.Sprintf("Initializing NFC reader on %s",r.deviceName))
	if err := r.hal.Initialize(); err != nil {
		r.logger.Error(fmt.Sprintf("Initialization failed: %v",err))
		return err
	}

	// Do NOT enable async tag event reader
	// r.hal.SetTagEventReaderEnabled(true)

	r.logger.Info(fmt.Sprintf("NFC reader initialized successfully"))
	return nil
}

func (r *BatteryReader) Deinitialize() {
	r.nfcMu.Lock()
	defer r.nfcMu.Unlock()

	r.deinitializeNFC()
	r.clearHeartbeatTimer()
}

func (r *BatteryReader) ReadStatus() error {
	if !r.readStatus() {
		return fmt.Errorf("battery %d: failed to read status", r.index)
	}
	return nil
}

func (r *BatteryReader) WriteCommand(cmd byte) error {
	var bmsCmd BMSCommand
	switch cmd {
	case 0x0A:
		bmsCmd = BMSCmdInsertedInScooter
	case 0x0B:
		bmsCmd = BMSCmdSeatboxOpened
	case 0x0C:
		bmsCmd = BMSCmdSeatboxClosed
	case 0x0D:
		bmsCmd = BMSCmdOn
	case 0x0E:
		bmsCmd = BMSCmdOff
	default:
		return fmt.Errorf("battery %d: unknown command byte 0x%02X",cmd)
	}

	r.writeCommand(bmsCmd)
	return nil
}

func (r *BatteryReader) GetEnabled() bool {
	return r.enabled
}

func (r *BatteryReader) GetSeatboxLockClosed() bool {
	return r.seatboxLockClosed
}

func (r *BatteryReader) GetVehicleActive() bool {
	return r.vehicleState == VehicleStateReadyToDrive
}

func (r *BatteryReader) CheckStateCorrect() bool {
	expectedState := BMSStateIdle
	if r.enabled {
		expectedState = BMSStateActive
	}

	if r.data.State != expectedState {
		r.logger.Warn(fmt.Sprintf("State mismatch - expected %s, got %s",
			r.index, expectedState, r.data.State))
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

func (r *BatteryReader) GetOpenedTime() time.Duration {
	if r.justOpened {
		if r.data.State == BMSStateAsleep {
			return BMSTimeCmdFirstOpenedAsleep
		}
		return BMSTimeCmdFirstOpenedAwake
	}
	return BMSTimeCmd
}

func (r *BatteryReader) GetInsertedTime() time.Duration {
	return BMSTimeCmd
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
	interval := r.getHeartbeatInterval()

	if r.heartbeatTimer != nil {
		r.heartbeatTimer.Stop()
	}

	r.heartbeatTimer = time.AfterFunc(interval, func() {
		if r.fsm != nil {
			if !r.CheckStateCorrect() {
				r.logger.Info(fmt.Sprintf("State mismatch detected during heartbeat - triggering departure"))
				r.fsm.SendEvent(fsm.TagDepartedEvent{})
			} else {
				r.fsm.SendEvent(fsm.HeartbeatTimeoutEvent{})
			}
		}
	})
}

func (r *BatteryReader) ClearHeartbeatTimer() {
	r.StopHeartbeatTimer()
}

func (r *BatteryReader) StopTimerIfBatteryEmpty() {
	if r.data.Charge == 0 {
		r.logger.Debug(fmt.Sprintf("Battery empty (charge=0), stopping heartbeat timer"))
		r.StopHeartbeatTimer()
	}
}

func (r *BatteryReader) IsRoleInactive() bool {
	return r.role == BatteryRoleInactive
}

func (r *BatteryReader) getHeartbeatInterval() time.Duration {
	if r.role == BatteryRoleInactive {
		return HeartbeatIntervalInactive
	}
	return HeartbeatIntervalActiveStandby
}

func (r *BatteryReader) updateLastCmdTime() {
	r.lastCmdTime = time.Now()
}
