package fsm

type Event interface {
	Type() string
}

type InitCompleteEvent struct{}

func (e InitCompleteEvent) Type() string { return "init_complete" }

type ReinitEvent struct{}

func (e ReinitEvent) Type() string { return "reinit" }

type TagArrivedEvent struct{}

func (e TagArrivedEvent) Type() string { return "tag_arrived" }

type TagDepartedEvent struct{}

func (e TagDepartedEvent) Type() string { return "tag_departed" }

type RestartEvent struct{}

func (e RestartEvent) Type() string { return "restart" }

type HeartbeatTimeoutEvent struct{}

func (e HeartbeatTimeoutEvent) Type() string { return "heartbeat_timeout" }

type SeatboxOpenedEvent struct{}

func (e SeatboxOpenedEvent) Type() string { return "seatbox_opened" }

type SeatboxClosedEvent struct{}

func (e SeatboxClosedEvent) Type() string { return "seatbox_closed" }

type VehicleStateChangedEvent struct {
	Active bool
}

func (e VehicleStateChangedEvent) Type() string { return "vehicle_state_changed" }

type DepartureTimeoutEvent struct{}

func (e DepartureTimeoutEvent) Type() string { return "departure_timeout" }

type CheckPresenceTimeoutEvent struct{}

func (e CheckPresenceTimeoutEvent) Type() string { return "check_presence_timeout" }

type LastCmdTimeoutEvent struct{}

func (e LastCmdTimeoutEvent) Type() string { return "last_cmd_timeout" }

type ClosedTimeoutEvent struct{}

func (e ClosedTimeoutEvent) Type() string { return "closed_timeout" }

type OnOffTimeoutEvent struct{}

func (e OnOffTimeoutEvent) Type() string { return "on_off_timeout" }

type InsertedClosedTimeoutEvent struct{}

func (e InsertedClosedTimeoutEvent) Type() string { return "inserted_closed_timeout" }

type OffTimeoutEvent struct{}

func (e OffTimeoutEvent) Type() string { return "off_timeout" }

type OpenedTimeoutEvent struct{}

func (e OpenedTimeoutEvent) Type() string { return "opened_timeout" }

type InsertedOpenTimeoutEvent struct{}

func (e InsertedOpenTimeoutEvent) Type() string { return "inserted_open_timeout" }

type CheckReaderTimeoutEvent struct{}

func (e CheckReaderTimeoutEvent) Type() string { return "check_reader_timeout" }
