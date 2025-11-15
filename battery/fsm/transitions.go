package fsm

import "context"

type TransitionAction func(ctx context.Context)

type Transition struct {
	NextState State
	Action    TransitionAction
}

func (sm *StateMachine) getTransition(event Event) Transition {
	sm.mu.RLock()
	currentState := sm.state
	sm.mu.RUnlock()

	noAction := Transition{NextState: currentState, Action: nil}
	tr := func(state State) Transition {
		return Transition{NextState: state, Action: nil}
	}
	trWithAction := func(state State, action TransitionAction) Transition {
		return Transition{NextState: state, Action: action}
	}

	switch currentState {
	case StateInit:
		if _, ok := event.(InitCompleteEvent); ok {
			return tr(StateNFCReaderOn)
		}

	case StateNFCReaderOff:
		if _, ok := event.(ReinitEvent); ok {
			return tr(StateNFCReaderOn)
		}

	case StateNFCReaderOn:
		if _, ok := event.(ReinitEvent); ok {
			return tr(StateNFCReaderOff)
		}
		if _, ok := event.(TagArrivedEvent); ok {
			return tr(StateDiscoverTag)
		}

	case StateDiscoverTag:
		if _, ok := event.(ReinitEvent); ok {
			return tr(StateNFCReaderOff)
		}
		if _, ok := event.(TagArrivedEvent); ok {
			return tr(StateTagPresent)
		}

	case StateWaitArrival:
		if _, ok := event.(ReinitEvent); ok {
			return tr(StateNFCReaderOff)
		}
		if _, ok := event.(TagArrivedEvent); ok {
			return tr(StateTagPresent)
		}
		if _, ok := event.(DepartureTimeoutEvent); ok {
			return tr(StateTagAbsent)
		}

	case StateTagAbsent:
		if _, ok := event.(ReinitEvent); ok {
			return tr(StateNFCReaderOff)
		}
		if _, ok := event.(TagArrivedEvent); ok {
			return tr(StateTagPresent)
		}
		if _, ok := event.(RestartEvent); ok {
			return trWithAction(StateTagAbsent, func(ctx context.Context) {
				if err := sm.actions.StartDiscovery(); err != nil {
					sm.log.Error("failed to restart discovery", "error", err)
					sm.SendEvent(ReinitEvent{})
				}
			})
		}

	case StateCheckPresence:
		if _, ok := event.(ReinitEvent); ok {
			return tr(StateNFCReaderOff)
		}
		if _, ok := event.(TagDepartedEvent); ok {
			return tr(StateDiscoverTag)
		}
		if _, ok := event.(CheckPresenceTimeoutEvent); ok {
			return tr(StateCondCheckPresence)
		}

	case StateWaitLastCmd:
		if _, ok := event.(ReinitEvent); ok {
			return tr(StateNFCReaderOff)
		}
		if _, ok := event.(TagDepartedEvent); ok {
			return tr(StateDiscoverTag)
		}
		if e, ok := event.(RestartEvent); ok && !sm.isInHierarchy(StateCheckPresence) {
			_ = e
			return tr(StateTagPresent)
		}
		if _, ok := event.(LastCmdTimeoutEvent); ok {
			return tr(StateCondIgnoreSeatbox)
		}

	case StateHeartbeat:
		if _, ok := event.(ReinitEvent); ok {
			return tr(StateNFCReaderOff)
		}
		if _, ok := event.(TagDepartedEvent); ok {
			return tr(StateDiscoverTag)
		}
		if e, ok := event.(RestartEvent); ok && !sm.isInHierarchy(StateCheckPresence) {
			_ = e
			return tr(StateTagPresent)
		}
		if _, ok := event.(SeatboxOpenedEvent); ok {
			return tr(StateCondJustInserted)
		}

	case StateHeartbeatActions:
		if _, ok := event.(ReinitEvent); ok {
			return tr(StateNFCReaderOff)
		}
		if _, ok := event.(TagDepartedEvent); ok {
			return tr(StateDiscoverTag)
		}
		if e, ok := event.(RestartEvent); ok && !sm.isInHierarchy(StateCheckPresence) {
			_ = e
			return tr(StateTagPresent)
		}
		if _, ok := event.(SeatboxOpenedEvent); ok {
			return tr(StateCondJustInserted)
		}
		if _, ok := event.(HeartbeatTimeoutEvent); ok {
			return tr(StateHeartbeatActions)
		}

	case StateSendClosed:
		if _, ok := event.(ReinitEvent); ok {
			return tr(StateNFCReaderOff)
		}
		if _, ok := event.(TagDepartedEvent); ok {
			return tr(StateDiscoverTag)
		}
		if e, ok := event.(RestartEvent); ok && !sm.isInHierarchy(StateCheckPresence) {
			_ = e
			return tr(StateTagPresent)
		}
		if _, ok := event.(SeatboxOpenedEvent); ok {
			return tr(StateCondJustInserted)
		}
		if _, ok := event.(ClosedTimeoutEvent); ok {
			return tr(StateSendOnOff)
		}

	case StateSendOnOff:
		if _, ok := event.(ReinitEvent); ok {
			return tr(StateNFCReaderOff)
		}
		if _, ok := event.(TagDepartedEvent); ok {
			return tr(StateDiscoverTag)
		}
		if e, ok := event.(RestartEvent); ok && !sm.isInHierarchy(StateCheckPresence) {
			_ = e
			return tr(StateTagPresent)
		}
		if _, ok := event.(SeatboxOpenedEvent); ok {
			return tr(StateCondJustInserted)
		}
		if _, ok := event.(OnOffTimeoutEvent); ok {
			return tr(StateCondStateOK)
		}

	case StateWaitUpdate:
		if _, ok := event.(ReinitEvent); ok {
			return tr(StateNFCReaderOff)
		}
		if _, ok := event.(TagDepartedEvent); ok {
			return tr(StateDiscoverTag)
		}
		if e, ok := event.(RestartEvent); ok && !sm.isInHierarchy(StateCheckPresence) {
			_ = e
			return tr(StateTagPresent)
		}
		if _, ok := event.(SeatboxOpenedEvent); ok {
			return tr(StateCondJustInserted)
		}
		if _, ok := event.(HeartbeatTimeoutEvent); ok {
			return tr(StateHeartbeatActions)
		}

	case StateSendInsertedClosed:
		if _, ok := event.(ReinitEvent); ok {
			return tr(StateNFCReaderOff)
		}
		if _, ok := event.(TagDepartedEvent); ok {
			return tr(StateDiscoverTag)
		}
		if e, ok := event.(RestartEvent); ok && !sm.isInHierarchy(StateCheckPresence) {
			_ = e
			return tr(StateTagPresent)
		}
		if _, ok := event.(SeatboxOpenedEvent); ok {
			return tr(StateCondJustInserted)
		}
		if _, ok := event.(InsertedClosedTimeoutEvent); ok {
			return tr(StateSendClosed)
		}

	case StateCondJustInserted:
		if _, ok := event.(ReinitEvent); ok {
			return tr(StateNFCReaderOff)
		}
		if _, ok := event.(TagDepartedEvent); ok {
			return tr(StateDiscoverTag)
		}
		if e, ok := event.(RestartEvent); ok && !sm.isInHierarchy(StateCheckPresence) {
			_ = e
			return tr(StateTagPresent)
		}

	case StateCondOff:
		if _, ok := event.(ReinitEvent); ok {
			return tr(StateNFCReaderOff)
		}
		if _, ok := event.(TagDepartedEvent); ok {
			return tr(StateDiscoverTag)
		}
		if e, ok := event.(RestartEvent); ok && !sm.isInHierarchy(StateCheckPresence) {
			_ = e
			return tr(StateTagPresent)
		}

	case StateSendOff:
		if _, ok := event.(ReinitEvent); ok {
			return tr(StateNFCReaderOff)
		}
		if _, ok := event.(TagDepartedEvent); ok {
			return tr(StateDiscoverTag)
		}
		if e, ok := event.(RestartEvent); ok && !sm.isInHierarchy(StateCheckPresence) {
			_ = e
			return tr(StateTagPresent)
		}
		if _, ok := event.(OffTimeoutEvent); ok {
			return tr(StateCondOff)
		}

	case StateSendOpened:
		if _, ok := event.(ReinitEvent); ok {
			return tr(StateNFCReaderOff)
		}
		if _, ok := event.(TagDepartedEvent); ok {
			return tr(StateDiscoverTag)
		}
		if e, ok := event.(RestartEvent); ok && !sm.isInHierarchy(StateCheckPresence) {
			_ = e
			return tr(StateTagPresent)
		}
		if _, ok := event.(OpenedTimeoutEvent); ok {
			return tr(StateSendInsertedOpen)
		}

	case StateSendInsertedOpen:
		if _, ok := event.(ReinitEvent); ok {
			return tr(StateNFCReaderOff)
		}
		if _, ok := event.(TagDepartedEvent); ok {
			return tr(StateDiscoverTag)
		}
		if e, ok := event.(RestartEvent); ok && !sm.isInHierarchy(StateCheckPresence) {
			_ = e
			return tr(StateTagPresent)
		}
		if _, ok := event.(InsertedOpenTimeoutEvent); ok {
			return tr(StateSendOpened)
		}
	}

	return noAction
}
