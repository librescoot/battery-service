package fsm

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
	StateHeartbeat
	StateHeartbeatActions
	StateSendClosed
	StateSendOnOff
	StateCondStateOK
	StateWaitUpdate
	StateSendInsertedClosed
	StateCondJustInserted
	StateCondOff
	StateSendOff
	StateSendOpened
	StateSendInsertedOpen
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
		StateCondSeatboxLock, StateHeartbeat,
		StateCondJustInserted, StateCondOff, StateSendOff,
		StateSendOpened, StateSendInsertedOpen:
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

func (s State) IsCondition() bool {
	switch s {
	case StateCondCheckPresence, StateCondSeatboxLock,
		StateCondStateOK, StateCondJustInserted, StateCondOff:
		return true
	default:
		return false
	}
}

type JumpEvaluator interface {
	EvaluateJump(state State) State
}

func (s State) HasJump() bool {
	return s.IsCondition()
}

func (s State) DefaultChild() State {
	switch s {
	case StateNFCReaderOn:
		return StateDiscoverTag
	case StateDiscoverTag:
		return StateWaitArrival
	case StateTagPresent:
		return StateCondCheckPresence
	case StateHeartbeat:
		return StateHeartbeatActions
	case StateHeartbeatActions:
		return StateSendClosed
	default:
		return StateRoot
	}
}
