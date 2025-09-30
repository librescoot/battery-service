package battery

import (
	"time"
)

type State int

const (
	StateRoot State = iota
	StateInit
	StateNFCReaderOff
	StateNFCReaderOn

	StateDiscoverTag
	StateWaitArrival
	StateTagAbsent

	StateTagPresent
	StateCondCheckPresence
	StateCheckPresence
	StateWaitLastCmd
	StateCondSeatboxLock
	StateCondJustInserted
	StateCondOff
	StateSendOff
	StateSendOpened
	StateSendInsertedOpen

	StateHeartbeat
	StateHeartbeatActions
	StateSendClosed
	StateSendOnOff
	StateCondStateOK
	StateWaitUpdate
	StateSendInsertedClosed
)

func (s State) String() string {
	switch s {
	case StateRoot:
		return "root"
	case StateInit:
		return "init"
	case StateNFCReaderOff:
		return "nfc_reader_off"
	case StateNFCReaderOn:
		return "nfc_reader_on"
	case StateDiscoverTag:
		return "discover_tag"
	case StateWaitArrival:
		return "wait_arrival"
	case StateTagAbsent:
		return "tag_absent"
	case StateTagPresent:
		return "tag_present"
	case StateCondCheckPresence:
		return "cond_check_presence"
	case StateCheckPresence:
		return "check_presence"
	case StateWaitLastCmd:
		return "wait_last_cmd"
	case StateCondSeatboxLock:
		return "cond_seatbox_lock"
	case StateCondJustInserted:
		return "cond_just_inserted"
	case StateCondOff:
		return "cond_off"
	case StateSendOff:
		return "send_off"
	case StateSendOpened:
		return "send_opened"
	case StateSendInsertedOpen:
		return "send_inserted_open"
	case StateHeartbeat:
		return "heartbeat"
	case StateHeartbeatActions:
		return "heartbeat_actions"
	case StateSendClosed:
		return "send_closed"
	case StateSendOnOff:
		return "send_on_off"
	case StateCondStateOK:
		return "cond_state_ok"
	case StateWaitUpdate:
		return "wait_update"
	case StateSendInsertedClosed:
		return "send_inserted_closed"
	default:
		return "unknown"
	}
}

func (s State) Parent() State {
	switch s {
	case StateInit, StateNFCReaderOff, StateNFCReaderOn:
		return StateRoot
	case StateDiscoverTag, StateTagPresent:
		return StateNFCReaderOn
	case StateWaitArrival, StateTagAbsent:
		return StateDiscoverTag
	case StateCondCheckPresence, StateCheckPresence, StateWaitLastCmd,
		StateCondSeatboxLock, StateCondJustInserted, StateCondOff,
		StateSendOff, StateSendOpened, StateSendInsertedOpen, StateHeartbeat:
		return StateTagPresent
	case StateHeartbeatActions:
		return StateHeartbeat
	case StateSendClosed, StateSendOnOff, StateCondStateOK,
		StateWaitUpdate, StateSendInsertedClosed:
		return StateHeartbeatActions
	default:
		return StateRoot
	}
}

func (r *BatteryReader) isIn(targetState State) bool {
	current := r.state
	for current != StateRoot {
		if current == targetState {
			return true
		}
		current = current.Parent()
	}
	return targetState == StateRoot
}

func (r *BatteryReader) enterState(newState State) {
	r.logStateTransition(r.state, newState)

	r.exitState()

	r.state = newState

	switch newState {
	case StateInit:

	case StateNFCReaderOff:
		r.deinitializeNFC()

	case StateNFCReaderOn:
		r.service.logger.Infof("Battery %d: NFC reader operational, starting discovery", r.index)
		r.transitionTo(StateDiscoverTag)

	case StateDiscoverTag:
		r.transitionTo(StateWaitArrival)

	case StateWaitArrival:
		r.takeInhibitor()
		r.startDiscovery()
		r.checkForTags()
		if r.state == StateWaitArrival {
			r.setStateTimer(BMSTimeDeparture)
		}

	case StateTagAbsent:
		r.startDiscovery()
		r.checkForTags()
		if r.state == StateTagAbsent {
			checkInterval := BMSTimeCheckReader
			if !r.seatboxLockClosed {
				checkInterval = 1 * time.Second
			}
			r.setStateTimer(checkInterval)
		}

	case StateTagPresent:
		r.transitionTo(StateCondCheckPresence)

	case StateCondCheckPresence:
		r.takeInhibitor()
		r.retryZeroDataOrEmptyBattery()
		if r.readStatus() {
			r.data.EmptyOr0Data = 0
			r.transitionTo(StateWaitLastCmd)
		} else {
			// Check threshold - if exceeded, give up active recovery
			if r.data.EmptyOr0Data > BMSMaxZeroRetryHeartbeat {
				r.service.logger.Warnf("Battery %d: Recovery threshold exceeded, entering passive discovery", r.index)
				r.transitionTo(StateTagAbsent)
			} else {
				r.transitionTo(StateCheckPresence)
			}
		}

	case StateCheckPresence:
		r.setStateTimer(BMSTimePresence)
		r.writeCommandProtected(BMSCmdInsertedInScooter)

	case StateWaitLastCmd:
		r.clearHeartbeatTimer()
		remaining := r.getRemainingCmdTime()
		if remaining > 0 {
			r.setStateTimer(remaining)
		} else {
			r.transitionTo(StateCondSeatboxLock)
		}

	case StateCondSeatboxLock:
		r.releaseInhibitor()
		if r.latchedSeatboxLockClosed {
			r.justInserted = false
			r.transitionTo(StateHeartbeat)
		} else {
			r.transitionTo(StateCondJustInserted)
		}

	case StateCondJustInserted:
		if r.justInserted {
			r.transitionTo(StateCondOff)
		} else {
			r.transitionTo(StateSendOff)
		}

	case StateCondOff:
		if r.checkInactive() {
			r.justOpened = true
			r.transitionTo(StateSendOpened)
		} else {
			r.transitionTo(StateSendOff)
		}

	case StateSendOff:
		r.setStateTimer(BMSTimeCmd)
		r.writeCommand(BMSCmdOff)

	case StateSendOpened:
		r.setStateTimer(r.getOpenedTime())
		r.writeCommand(BMSCmdSeatboxOpened)

	case StateSendInsertedOpen:
		r.setStateTimer(r.getInsertedTime())
		r.writeCommand(BMSCmdInsertedInScooter)

	case StateHeartbeat:
		r.setupHeartbeatTimer()
		r.transitionTo(StateHeartbeatActions)

	case StateHeartbeatActions:
		r.takeInhibitor()
		r.transitionTo(StateSendClosed)

	case StateSendClosed:
		r.setStateTimer(BMSTimeCmd)
		r.writeCommand(BMSCmdSeatboxClosed)

	case StateSendOnOff:
		r.setStateTimer(BMSTimeCmd)
		r.sendOnOff()

	case StateCondStateOK:
		if r.checkStateCorrect(true) {
			r.stopDiscovery()
			r.transitionTo(StateWaitUpdate)
		} else {
			r.transitionTo(StateSendInsertedClosed)
		}

	case StateWaitUpdate:
		r.stopTimerIfBatteryEmpty()
		r.releaseInhibitor()
		r.clearHeartbeatTimer() // Clear recovery heartbeat timer
		// Set state timer for next heartbeat interval
		r.setStateTimer(r.getHeartbeatInterval())

	case StateSendInsertedClosed:
		r.setStateTimer(BMSTimeCmd)
		r.writeCommand(BMSCmdInsertedInScooter)
	}
}

func (r *BatteryReader) exitState() {
	if r.stateTimer != nil {
		r.stateTimer.Stop()
		r.stateTimer = nil
	}

	switch r.state {
	case StateCondCheckPresence:
		r.releaseInhibitor()
	case StateHeartbeat:
		// Don't clear heartbeat timer - keep it running during recovery
		// r.clearHeartbeatTimer()
	case StateHeartbeatActions:
		r.releaseInhibitor()
	}
}

func (r *BatteryReader) transitionTo(newState State) {
	r.enterState(newState)
}

func (r *BatteryReader) setStateTimer(duration time.Duration) {
	if r.stateTimer != nil {
		r.stateTimer.Stop()
	}
	r.stateTimer = time.NewTimer(duration)
}

func (r *BatteryReader) getStateTimer() <-chan time.Time {
	if r.stateTimer == nil {
		return nil
	}
	return r.stateTimer.C
}

func (r *BatteryReader) logStateTransition(from, to State) {
	if from != to {
		r.service.logger.Debugf("Battery %d: %s -> %s", r.index, from, to)
	}
}
