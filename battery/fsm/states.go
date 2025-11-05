package fsm

import (
	"context"
	"time"
)

const (
	timeCmd                      = 400 * time.Millisecond
	timeReinit                   = 2 * time.Second
	timeDeparture                = 500 * time.Millisecond
	timeCheckReader              = 10 * time.Second
	timeCheckPresence            = 10 * time.Second
	timeHeartbeatIntervalScooter = 30 * time.Second
	timeMaintPollInterval        = 5 * time.Minute
)

func (sm *StateMachine) enterState(ctx context.Context, state State) {
	sm.log.Debug("entering state", "state", state.String())

	switch state {
	case StateInit:
		sm.onEnterInit(ctx)
	case StateNFCReaderOff:
		sm.onEnterNFCReaderOff(ctx)
	case StateNFCReaderOn:
		sm.onEnterNFCReaderOn(ctx)
	case StateDiscoverTag:
		sm.onEnterDiscoverTag(ctx)
	case StateWaitArrival:
		sm.onEnterWaitArrival(ctx)
	case StateTagAbsent:
		sm.onEnterTagAbsent(ctx)
	case StateTagPresent:
		sm.onEnterTagPresent(ctx)
	case StateCondCheckPresence:
		sm.onEnterCondCheckPresence(ctx)
	case StateCheckPresence:
		sm.onEnterCheckPresence(ctx)
	case StateWaitLastCmd:
		sm.onEnterWaitLastCmd(ctx)
	case StateCondSeatboxLock:
		sm.onEnterCondSeatboxLock(ctx)
	case StateHeartbeat:
		sm.onEnterHeartbeat(ctx)
	case StateHeartbeatActions:
		sm.onEnterHeartbeatActions(ctx)
	case StateSendClosed:
		sm.onEnterSendClosed(ctx)
	case StateSendOnOff:
		sm.onEnterSendOnOff(ctx)
	case StateCondStateOK:
		sm.onEnterCondStateOK(ctx)
	case StateWaitUpdate:
		sm.onEnterWaitUpdate(ctx)
	case StateSendInsertedClosed:
		sm.onEnterSendInsertedClosed(ctx)
	case StateCondJustInserted:
		sm.onEnterCondJustInserted(ctx)
	case StateCondOff:
		sm.onEnterCondOff(ctx)
	case StateSendOff:
		sm.onEnterSendOff(ctx)
	case StateSendOpened:
		sm.onEnterSendOpened(ctx)
	case StateSendInsertedOpen:
		sm.onEnterSendInsertedOpen(ctx)
	}
}

func (sm *StateMachine) exitState(ctx context.Context, state State) {
	sm.log.Debug("exiting state", "state", state.String())

	switch state {
	case StateWaitArrival:
		sm.onExitWaitArrival(ctx)
	case StateTagAbsent:
		sm.onExitTagAbsent(ctx)
	case StateCondCheckPresence:
		sm.onExitCondCheckPresence(ctx)
	case StateHeartbeat:
		sm.onExitHeartbeat(ctx)
	case StateHeartbeatActions:
		sm.onExitHeartbeatActions(ctx)
	}
}

func (sm *StateMachine) onEnterInit(ctx context.Context) {
	sm.SendEvent(InitCompleteEvent{})
}

func (sm *StateMachine) onEnterNFCReaderOff(ctx context.Context) {
	sm.actions.Deinitialize()
	sm.startTimer("reinit", timeReinit, func() {
		sm.SendEvent(ReinitEvent{})
	})
}

func (sm *StateMachine) onEnterNFCReaderOn(ctx context.Context) {
	if err := sm.actions.Initialize(); err != nil {
		sm.log.Error("failed to initialize NFC", "error", err)
		sm.SendEvent(ReinitEvent{})
		return
	}
}

func (sm *StateMachine) onEnterDiscoverTag(ctx context.Context) {
	// Default child StateWaitArrival will be entered automatically
}

func (sm *StateMachine) onEnterWaitArrival(ctx context.Context) {
	sm.actions.TakeInhibitor()
	if err := sm.actions.StartDiscovery(); err != nil {
		// I2C bus may be stuck after physical tag removal - this is expected
		// The departure timer will handle transition to tag_absent
		sm.log.Warn("failed to start discovery in wait_arrival (may be transient after tag removal)", "error", err)
		// Don't trigger reinit here - let the system recover naturally
	}

	sm.startTimer("departure", timeDeparture, func() {
		sm.SendEvent(DepartureTimeoutEvent{})
	})
}

func (sm *StateMachine) onExitWaitArrival(ctx context.Context) {
	sm.stopTimer("departure")
}

func (sm *StateMachine) onEnterTagAbsent(ctx context.Context) {
	if err := sm.actions.StartDiscovery(); err != nil {
		sm.log.Error("failed to start discovery", "error", err)
		sm.SendEvent(ReinitEvent{})
		return
	}

	// Create cancellable context for polling goroutine
	pollCtx, cancel := context.WithCancel(ctx)
	sm.mu.Lock()
	sm.tagAbsentCancel = cancel
	sm.mu.Unlock()

	// Start goroutine to poll for tag arrivals
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-pollCtx.Done():
				return
			case <-ticker.C:
				sm.actions.PollForTagArrival()
			}
		}
	}()

	sm.startTimer("check_reader", timeCheckReader, func() {
		sm.SendEvent(CheckReaderTimeoutEvent{})
	})
}

func (sm *StateMachine) onExitTagAbsent(ctx context.Context) {
	sm.stopTimer("check_reader")

	sm.mu.Lock()
	if sm.tagAbsentCancel != nil {
		sm.tagAbsentCancel()
		sm.tagAbsentCancel = nil
	}
	sm.mu.Unlock()
}

func (sm *StateMachine) onEnterTagPresent(ctx context.Context) {
	// Default child StateCondCheckPresence will be entered automatically
}

func (sm *StateMachine) onEnterCondCheckPresence(ctx context.Context) {
	sm.actions.TakeInhibitor()
	sm.actions.ZeroRetryCounters()
	sm.lastOperationSuccess = (sm.actions.ReadStatus() == nil)
	// Jump evaluation will happen automatically
}

func (sm *StateMachine) onExitCondCheckPresence(ctx context.Context) {
	sm.actions.ReleaseInhibitor()
}

func (sm *StateMachine) onEnterCheckPresence(ctx context.Context) {
	sm.actions.WriteCommand(BMSCmdInsertedInScooter)

	sm.startTimer("check_presence", timeCheckPresence, func() {
		sm.SendEvent(CheckPresenceTimeoutEvent{})
	})
}

func (sm *StateMachine) onEnterWaitLastCmd(ctx context.Context) {
	sm.actions.ClearHeartbeatTimer()

	remaining := sm.actions.GetRemainingCmdTime()
	sm.startTimer("last_cmd", remaining, func() {
		sm.SendEvent(LastCmdTimeoutEvent{})
	})
}

func (sm *StateMachine) onEnterCondSeatboxLock(ctx context.Context) {
	sm.latchedSeatboxClosed = sm.actions.GetSeatboxLockClosed()
	if !sm.latchedSeatboxClosed {
		sm.actions.ReleaseInhibitor()
	}
	// Jump evaluation will happen automatically
}

func (sm *StateMachine) onEnterHeartbeat(ctx context.Context) {
	sm.actions.StartHeartbeatTimer()
	// Default child StateHeartbeatActions will be entered automatically
}

func (sm *StateMachine) onExitHeartbeat(ctx context.Context) {
	sm.actions.ClearHeartbeatTimer()
}

func (sm *StateMachine) onEnterHeartbeatActions(ctx context.Context) {
	sm.actions.TakeInhibitor()
	// Default child StateSendClosed will be entered automatically
}

func (sm *StateMachine) onExitHeartbeatActions(ctx context.Context) {
	sm.actions.ReleaseInhibitor()
}

func (sm *StateMachine) onEnterSendClosed(ctx context.Context) {
	sm.actions.WriteCommand(BMSCmdSeatboxClosed)

	sm.startTimer("closed", timeCmd, func() {
		sm.SendEvent(ClosedTimeoutEvent{})
	})
}

func (sm *StateMachine) onEnterSendOnOff(ctx context.Context) {
	var cmd BMSCommand
	if sm.actions.GetEnabled() {
		cmd = BMSCmdOn
	} else {
		cmd = BMSCmdOff
	}

	sm.actions.WriteCommand(cmd)

	sm.startTimer("on_off", timeCmd, func() {
		if err := sm.actions.ReadStatus(); err != nil {
			sm.log.Error("failed to read status after ON/OFF", "error", err)
		}
		sm.SendEvent(OnOffTimeoutEvent{})
	})
}

func (sm *StateMachine) onEnterCondStateOK(ctx context.Context) {
	if sm.actions.CheckStateCorrect() {
		sm.actions.StopDiscovery()
	}
	// Jump evaluation will happen automatically
}

func (sm *StateMachine) onEnterWaitUpdate(ctx context.Context) {
	sm.actions.StartHeartbeatTimer()
	sm.actions.StopTimerIfBatteryEmpty()
	sm.actions.ReleaseInhibitor()
}

func (sm *StateMachine) onEnterSendInsertedClosed(ctx context.Context) {
	sm.actions.WriteCommand(BMSCmdInsertedInScooter)

	sm.startTimer("inserted_closed", timeCmd, func() {
		sm.SendEvent(InsertedClosedTimeoutEvent{})
	})
}

func (sm *StateMachine) onEnterCondJustInserted(ctx context.Context) {
	// Jump evaluation will happen automatically
}

func (sm *StateMachine) onEnterCondOff(ctx context.Context) {
	// Jump evaluation will happen automatically
}

func (sm *StateMachine) onEnterSendOff(ctx context.Context) {
	sm.actions.WriteCommand(BMSCmdOff)

	sm.startTimer("off", timeCmd, func() {
		if err := sm.actions.ReadStatus(); err != nil {
			sm.log.Error("failed to read status after OFF", "error", err)
		}
		sm.SendEvent(OffTimeoutEvent{})
	})
}

func (sm *StateMachine) onEnterSendOpened(ctx context.Context) {
	sm.actions.WriteCommand(BMSCmdSeatboxOpened)

	openedTime := sm.actions.GetOpenedTime()
	sm.startTimer("opened", openedTime, func() {
		sm.justOpened = false
		sm.SendEvent(OpenedTimeoutEvent{})
	})
}

func (sm *StateMachine) onEnterSendInsertedOpen(ctx context.Context) {
	sm.actions.WriteCommand(BMSCmdInsertedInScooter)

	insertedTime := sm.actions.GetInsertedTime()
	sm.startTimer("inserted_open", insertedTime, func() {
		if err := sm.actions.ReadStatus(); err != nil {
			sm.log.Error("failed to read status after INSERTED", "error", err)
		}
		sm.justInserted = false
		sm.SendEvent(InsertedOpenTimeoutEvent{})
	})
}
