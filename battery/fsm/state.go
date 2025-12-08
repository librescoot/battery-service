package fsm

import "github.com/librescoot/librefsm"

// State represents a state in the battery FSM
type State = librefsm.StateID

// State constants
const (
	StateRoot               State = "root"
	StateInit               State = "init"
	StateNFCReaderOff       State = "nfc_reader_off"
	StateNFCReaderOn        State = "nfc_reader_on"
	StateDiscoverTag        State = "discover_tag"
	StateWaitArrival        State = "wait_arrival"
	StateTagAbsent          State = "tag_absent"
	StateTagPresent         State = "tag_present"
	StateCondCheckPresence  State = "cond_check_presence"
	StateCheckPresence      State = "check_presence"
	StateWaitLastCmd        State = "wait_last_cmd"
	StateCondIgnoreSeatbox  State = "cond_ignore_seatbox"
	StateCondSeatboxLock    State = "cond_seatbox_lock"
	StateHeartbeat          State = "heartbeat"
	StateHeartbeatActions   State = "heartbeat_actions"
	StateSendClosed         State = "send_closed"
	StateSendOnOff          State = "send_on_off"
	StateCondStateOK        State = "cond_state_ok"
	StateWaitUpdate         State = "wait_update"
	StateSendInsertedClosed State = "send_inserted_closed"
	StateCondJustInserted   State = "cond_just_inserted"
	StateCondOff            State = "cond_off"
	StateSendOff            State = "send_off"
	StateSendOpened         State = "send_opened"
	StateSendInsertedOpen   State = "send_inserted_open"
)
