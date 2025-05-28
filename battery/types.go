package battery

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"battery-service/nfc/hal"

	"github.com/redis/go-redis/v9"
)

// ErrHALRecreatedRetryRead signals that the HAL was recreated and the read operation should be retried.
var errHALRecreatedRetryRead = fmt.Errorf("HAL was recreated, retry read operation")

// BatteryReader represents a single battery reader instance
type BatteryReader struct {
	sync.Mutex
	index                            int
	role                             BatteryRole          // Role of this battery (active or inactive)
	hal                              hal.HAL
	data                             BatteryData
	enabled                          bool
	deviceName                       string               // NFC device path
	logLevel                         int                  // Log level for this reader
	service                          *Service
	lastCmd                          time.Time
	justInserted                     bool
	readyToScoot                     bool                 // Flag indicating battery responded with ReadyToScoot
	stopChan                         chan struct{}        // Channel to signal goroutine shutdown
	nfcMutex                         sync.Mutex           // Serializes access to NFC HAL operations
	lastPublishedData                BatteryData          // Stores the state as of the last successful Redis update with PUBLISH
	lastBattery1MaintPollTime        time.Time            // Tracks last maintenance poll for battery 1
	lastIdleAsleepPollTime           time.Time            // Tracks last poll time for battery 0 when idle/asleep in stand-by
	lastReinitialization             time.Time            // Track when HAL was last reinitialized
	isPoweredDown                    bool                 // Indicates reader is temporarily powered down during low-frequency polling
	stateMachine                     *BatteryStateMachine // State machine for managing battery transitions
	
	// Operation cancellation context - cancelled when tag departs to abort pending operations
	operationCtx    context.Context
	operationCancel context.CancelFunc
}

// getOperationContext returns the operation context if available, otherwise the service context
func (r *BatteryReader) getOperationContext() context.Context {
	r.Lock()
	defer r.Unlock()
	if r.operationCtx != nil {
		return r.operationCtx
	}
	return r.service.ctx
}

// Service represents the battery service that manages multiple readers
type Service struct {
	sync.Mutex
	config           *ServiceConfig
	batteryConfig    *BatteryConfiguration
	readers          []*BatteryReader
	logger           *log.Logger
	redis            *redis.Client
	ctx              context.Context
	cancel           context.CancelFunc
	debug            bool // Add debug flag here
	seatboxOpen      bool // Track current seatbox state

	// Vehicle state tracking
	vehicleState          string

}

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
	// Commands to Battery Management Software
	BatteryCommandOn                BatteryCommand = 0x50505050 // Turn on the high-current path, Enters BatteryActive mode.
	BatteryCommandOff               BatteryCommand = 0xCAFEF00D // Turn off the high-current path, Enters BatteryIdle mode.
	BatteryCommandSleepNow          BatteryCommand = 0x39845983 // Force Battery to enter BatteryAsleep mode immediately.
	BatteryCommandInsertedInCharger BatteryCommand = 0x4D415856 // Charger tells Battery that it is now in the Charger. ("MAXV")
	BatteryCommandInsertedInScooter BatteryCommand = 0x44414E41 // Scooter tells Battery that it is now in the Scooter. ("ANAD")
	BatteryCommandChargerHeartbeat  BatteryCommand = 0x4755494C // This signal indicates that the Battery is in the Charger. ("GUIL")
	BatteryCommandScooterHeartbeat  BatteryCommand = 0x534E4A41 // This signal indicates that the Battery is in the Scooter. ("SNJA")
	BatteryCommandBatteryRemoved    BatteryCommand = 0x4753534F // Battery Management System tells LED Ring that Battery removed from Charger or from Scooter. ("GSSO")
	BatteryCommandSocUpdate         BatteryCommand = 0xFE4C4958 // Battery Management System tells LED Ring the SOC percentage, set data[0] = SOC %. ("þLIX")
	BatteryCommandUserOpenedSeatbox BatteryCommand = 0x48525259 // Scooter sends this command to BMS, which forwards command to LED Ring. ("HRRY")
	BatteryCommandUserClosedSeatbox BatteryCommand = 0x4D4B4D4B // Scooter sends this command to BMS, which forwards command to LED Ring. ("MKMK")
	BatteryCommandErrorDetected     BatteryCommand = 0x54484D53 // Battery Management System sends this command to LED Ring when an error is detected. ("THMS")
	BatteryCommandLedPassthrough    BatteryCommand = 0x52AAABEB // Entire BatteryControlMessage is forwarded to LED Ring.

	// Responses from Battery Management Software (written to command register)
	BatteryCommandReadyToCharge BatteryCommand = 0x4D485249 // Battery writes this to indicate ready to charge. ("MHRI")
	BatteryCommandReadyToScoot  BatteryCommand = 0x4D484D54 // Battery writes this to indicate ready to scoot. ("MHMT")

	BatteryCommandNone BatteryCommand = 0 // Keep for default/unknown
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
	BatteryFWVersionLen         = 7 // "255.255"
	BatterySerialNumberLen      = 16
	BatteryManufacturingDateLen = 10 // "2020-01-01"
	BatteryNumTemperatures      = 4
)

// BatteryFaults represents specific fault conditions reported by the battery
type BatteryFaults struct {
	ChargeTempOverHigh    bool
	ChargeTempOverLow     bool
	DischargeTempOverHigh bool
	DischargeTempOverLow  bool
	SignalWireBroken      bool
	SecondLevelOverTemp   bool
	PackVoltageHigh       bool
	MOSTempOverHigh       bool
	CellVoltageHigh       bool
	PackVoltageLow        bool
	CellVoltageLow        bool
	ChargeOverCurrent     bool
	DischargeOverCurrent  bool
	ShortCircuit          bool
	NotFollowingCommand   bool // Battery state not changing as expected after command
	ZeroData              bool // Received all zero data from battery
	CommunicationError    bool // Error during NFC read/write
	ReaderError           bool // Underlying NFC reader hardware error
}

// BatteryData represents the data read from a battery
type BatteryData struct {
	Present           bool
	Voltage           uint16
	Current           int16
	FWVersion         string
	Charge            uint8
	FaultCode         uint16        // Keep raw fault code for reference
	Faults            BatteryFaults // Detailed fault breakdown
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
}

// ServiceConfig represents the configuration for the battery service
type ServiceConfig struct {
	RedisServerAddress string
	RedisServerPort    uint16
	TestMainPower      bool
}
