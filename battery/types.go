package battery

// BatteryState represents the state of the battery
type BatteryState uint32

const (
	BatteryStateUnknown BatteryState = 0
	BatteryStateAsleep  BatteryState = 0xA4983474
	BatteryStateIdle    BatteryState = 0xB9164828
	BatteryStateActive  BatteryState = 0xC6583518
)

// String returns a string representation of the battery state
func (s BatteryState) String() string {
	switch s {
	case BatteryStateAsleep:
		return "asleep"
	case BatteryStateIdle:
		return "idle"
	case BatteryStateActive:
		return "active"
	default:
		return "unknown"
	}
}

// BatteryCommand represents commands that can be sent to the battery
type BatteryCommand uint32

const (
	BatteryCommandNone              BatteryCommand = 0
	BatteryCommandOn               BatteryCommand = 0x50505050
	BatteryCommandOff              BatteryCommand = 0xCAFEF00D
	BatteryCommandInsertedInCharger BatteryCommand = 0x4D415856 // "MAXV"
	BatteryCommandInsertedInScooter BatteryCommand = 0x44414E41 // "ANAD"
	BatteryCommandHeartbeatCharger  BatteryCommand = 0x4755494C // "GUIL"
	BatteryCommandHeartbeatScooter  BatteryCommand = 0x534E4A41 // "SNJA"
	BatteryCommandSeatboxOpened     BatteryCommand = 0x48525259 // "HRRY"
	BatteryCommandSeatboxClosed     BatteryCommand = 0x4D4B4D4B // "MKMK"
	BatteryCommandReadyToCharge     BatteryCommand = 0x4D485249 // "MHRI"
	BatteryCommandReadyToScoot      BatteryCommand = 0x4D484D54 // "MHMT"
)

// BatteryTemperatureState represents the temperature state of the battery
type BatteryTemperatureState int

const (
	BatteryTemperatureStateUnknown BatteryTemperatureState = iota
	BatteryTemperatureStateCold
	BatteryTemperatureStateHot
	BatteryTemperatureStateIdeal
)

// String returns a string representation of the temperature state
func (s BatteryTemperatureState) String() string {
	switch s {
	case BatteryTemperatureStateCold:
		return "cold"
	case BatteryTemperatureStateHot:
		return "hot"
	case BatteryTemperatureStateIdeal:
		return "ideal"
	default:
		return "unknown"
	}
}

// Constants for battery data
const (
	BatteryFWVersionLen        = 7  // "255.255"
	BatterySerialNumberLen     = 16
	BatteryManufacturingDateLen = 10 // "2020-01-01"
	BatteryNumTemperatures     = 4
)

// BatteryData represents the data read from a battery
type BatteryData struct {
	Present            bool
	Voltage           uint16
	Current           int16
	FWVersion         string
	Charge            uint8
	FaultCode         uint16
	Temperature       [BatteryNumTemperatures]int8
	TemperatureState  BatteryTemperatureState
	StateOfHealth     uint8
	LowSOC            bool
	State             BatteryState
	SerialNumber      [BatterySerialNumberLen]byte
	ManufacturingDate string
	CycleCount        uint16
	RemainingCapacity uint16
	FullCapacity      uint16
	EmptyOr0Data      int
}

// BatteryConfig represents the configuration for a battery reader
type BatteryConfig struct {
	DeviceName string
	LogLevel   int
}

// ServiceConfig represents the configuration for the battery service
type ServiceConfig struct {
	RedisServerAddress string
	RedisServerPort    uint16
	OffUpdateTime      uint
	TestMainPower      bool
	HeartbeatTimeout   uint16
	Batteries          [2]BatteryConfig
} 