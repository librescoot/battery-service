package fsm

type BMSCommand uint32

const (
	BMSCmdNone              BMSCommand = 0
	BMSCmdOn                BMSCommand = 0x50505050
	BMSCmdOff               BMSCommand = 0xCAFEF00D
	BMSCmdInsertedInScooter BMSCommand = 0x44414E41
	BMSCmdSeatboxOpened     BMSCommand = 0x48525259
	BMSCmdSeatboxClosed     BMSCommand = 0x4D4B4D4B
	BMSCmdHeartbeatScooter  BMSCommand = 0x534E4A41
	BMSCmdInsertedInCharger BMSCommand = 0x4D415856
	BMSCmdHeartbeatCharger  BMSCommand = 0x4755494C
	BMSCmdReadyToCharge     BMSCommand = 0x4D485249
	BMSCmdReadyToScoot      BMSCommand = 0x4D484D54
)

func (c BMSCommand) String() string {
	switch c {
	case BMSCmdNone:
		return "NONE"
	case BMSCmdOn:
		return "ON"
	case BMSCmdOff:
		return "OFF"
	case BMSCmdInsertedInScooter:
		return "INSERTED_IN_SCOOTER"
	case BMSCmdSeatboxOpened:
		return "SEATBOX_OPENED"
	case BMSCmdSeatboxClosed:
		return "SEATBOX_CLOSED"
	case BMSCmdHeartbeatScooter:
		return "HEARTBEAT_SCOOTER"
	case BMSCmdInsertedInCharger:
		return "INSERTED_IN_CHARGER"
	case BMSCmdHeartbeatCharger:
		return "HEARTBEAT_CHARGER"
	case BMSCmdReadyToCharge:
		return "READY_TO_CHARGE"
	case BMSCmdReadyToScoot:
		return "READY_TO_SCOOT"
	default:
		return "UNKNOWN"
	}
}
