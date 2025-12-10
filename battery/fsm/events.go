package fsm

import "github.com/librescoot/librefsm"

// EventID constants
const (
	EvInitComplete          librefsm.EventID = "init_complete"
	EvReinit                librefsm.EventID = "reinit"
	EvTagArrived            librefsm.EventID = "tag_arrived"
	EvTagDeparted           librefsm.EventID = "tag_departed"
	EvRestart               librefsm.EventID = "restart"
	EvHeartbeatTimeout      librefsm.EventID = "heartbeat_timeout"
	EvSeatboxOpened         librefsm.EventID = "seatbox_opened"
	EvSeatboxClosed         librefsm.EventID = "seatbox_closed"
	EvVehicleStateChanged   librefsm.EventID = "vehicle_state_changed"
	EvDepartureTimeout      librefsm.EventID = "departure_timeout"
	EvCheckPresenceTimeout  librefsm.EventID = "check_presence_timeout"
	EvLastCmdTimeout        librefsm.EventID = "last_cmd_timeout"
	EvClosedTimeout         librefsm.EventID = "closed_timeout"
	EvOnOffTimeout          librefsm.EventID = "on_off_timeout"
	EvInsertedClosedTimeout librefsm.EventID = "inserted_closed_timeout"
	EvOffTimeout            librefsm.EventID = "off_timeout"
	EvOpenedTimeout         librefsm.EventID = "opened_timeout"
	EvInsertedOpenTimeout   librefsm.EventID = "inserted_open_timeout"
	EvCheckReaderTimeout    librefsm.EventID = "check_reader_timeout"
)
