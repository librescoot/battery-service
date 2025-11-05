package battery

import (
	"context"
	"log"
	"log/slog"
	"sync"
	"time"

	"battery-service/battery/fsm"
	"battery-service/nfc/hal"

	"github.com/redis/go-redis/v9"
)

type fsmStateMachine = fsm.StateMachine

type BMSState uint32

const (
	BMSStateUnknown BMSState = 0
	BMSStateAsleep  BMSState = 0xA4983474
	BMSStateIdle    BMSState = 0xB9164828
	BMSStateActive  BMSState = 0xC6583518
)

func (s BMSState) String() string {
	switch s {
	case BMSStateUnknown:
		return "unknown"
	case BMSStateAsleep:
		return "asleep"
	case BMSStateIdle:
		return "idle"
	case BMSStateActive:
		return "active"
	default:
		return "unknown"
	}
}

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

type VehicleState string

const (
	VehicleStateStandby                    VehicleState = "stand-by"
	VehicleStateParked                     VehicleState = "parked"
	VehicleStateReadyToDrive               VehicleState = "ready-to-drive"
	VehicleStateWaitingSeatbox             VehicleState = "waiting-seatbox"
	VehicleStateShuttingDown               VehicleState = "shutting-down"
	VehicleStateUpdating                   VehicleState = "updating"
	VehicleStateWaitingHibernation         VehicleState = "waiting-hibernation"
	VehicleStateWaitingHibernationAdvanced VehicleState = "waiting-hibernation-advanced"
	VehicleStateWaitingHibernationSeatbox  VehicleState = "waiting-hibernation-seatbox"
	VehicleStateWaitingHibernationConfirm  VehicleState = "waiting-hibernation-confirm"
	VehicleStateOther                      VehicleState = "other"
)

type BMSTemperatureState int

const (
	BMSTemperatureStateUnknown BMSTemperatureState = iota
	BMSTemperatureStateCold
	BMSTemperatureStateHot
	BMSTemperatureStateIdeal
)

const (
	BMSTemperatureStateColdLimit = 2
	BMSTemperatureStateHotLimit  = 43
)

type BMSFault int

const (
	BMSFaultNone BMSFault = 0

	BMSFaultChgTempOverHighProt BMSFault = 1
	BMSFaultChgTempOverLowProt  BMSFault = 2
	BMSFaultDsgTempOverHighProt BMSFault = 3
	BMSFaultDsgTempOverLowProt  BMSFault = 4
	BMSFaultSignalWireBrokeProt BMSFault = 5
	BMSFaultSecondLvlOverTemp   BMSFault = 6
	BMSFaultPackVoltHighProt    BMSFault = 7
	BMSFaultMosTempOverHighProt BMSFault = 8
	BMSFaultCellVoltHighProt    BMSFault = 9
	BMSFaultPackVoltLowProt     BMSFault = 10
	BMSFaultCellVoltLowProt     BMSFault = 11
	BMSFaultCrgOverCurrentProt  BMSFault = 12
	BMSFaultDsgOverCurrentProt  BMSFault = 13
	BMSFaultShortCircuitProt    BMSFault = 14
	BMSFaultReserved            BMSFault = 15
	BMSFaultReserved2           BMSFault = 16

	BMSFaultBMSNotFollowingCmd BMSFault = 32
	BMSFaultBMSZeroData        BMSFault = 33
	BMSFaultBMSCommsError      BMSFault = 34
	BMSFaultNFCReaderError     BMSFault = 35

	BMSFaultNum BMSFault = 64
)

func (f BMSFault) IsCritical() bool {
	return f >= BMSFaultBMSZeroData
}

type BMSData struct {
	Present           bool                `json:"present"`
	Voltage           uint                `json:"voltage"`
	Current           int                 `json:"current"`
	FwVersion         string              `json:"fw_version"`
	Charge            uint                `json:"charge"`
	FaultCode         uint                `json:"fault_code"`
	Temperature       [4]int              `json:"temperature"`
	TemperatureState  BMSTemperatureState `json:"temperature_state"`
	StateOfHealth     uint8               `json:"state_of_health"`
	LowSOC            bool                `json:"low_soc"`
	State             BMSState            `json:"state"`
	SerialNumber      string              `json:"serial_number"`
	ManuDate          string              `json:"manu_date"`
	CycleCount        uint                `json:"cycle_count"`
	RemainingCapacity uint                `json:"remaining_capacity"`
	FullCapacity      uint                `json:"full_capacity"`
	EmptyOr0Data      int                 `json:"empty_or_0_data"`
}

const (
	BMSTimeReinit               = 2 * time.Second
	BMSTimeDeparture            = 500 * time.Millisecond
	BMSTimeCheckReader          = 10 * time.Second
	BMSTimeReadable             = 250 * time.Millisecond
	BMSTimeCmd                  = 400 * time.Millisecond
	BMSTimeCmdSlow              = 1 * time.Second
	BMSTimeCmdFirstOpenedAwake  = 2 * time.Second
	BMSTimeCmdFirstOpenedAsleep = 3 * time.Second
	BMSTimeUpdateOn             = 10 * time.Second
	BMSTimePresence             = 10 * time.Second
)

// Retry limits
const (
	BMSMaxZeroRetryHeartbeat = 10
	BMSMaxRetryTakeInhibitor = 10
	BMSMinSOC                = 0
)

// Heartbeat intervals
const (
	HeartbeatIntervalActiveStandby = 40 * time.Second
	HeartbeatIntervalInactive      = 30 * time.Minute
)

// Discovery polling intervals (milliseconds)
const (
	DiscoveryPollFast = 100  // seatbox open
	DiscoveryPollSlow = 2500 // seatbox closed
)

type BatteryRole string

const (
	BatteryRoleActive   BatteryRole = "active"
	BatteryRoleInactive BatteryRole = "inactive"
)

// Configuration types
type ServiceConfig struct {
	RedisServerAddress       string
	RedisServerPort          uint16
	TestMainPower            bool
	HeartbeatTimeout         time.Duration
	OffUpdateTime            time.Duration
	DangerouslyIgnoreSeatbox bool
}

type BatteryReaderConfig struct {
	Index      int
	Role       BatteryRole
	Enabled    bool
	DeviceName string
	LogLevel   int
}

type BatteryConfiguration struct {
	Readers []BatteryReaderConfig
}

// Initialization completion tracking
type InitComplete struct {
	VehicleState bool
	SeatboxLock  bool
}

// Service represents the main battery service
type Service struct {
	config        *ServiceConfig
	batteryConfig *BatteryConfiguration
	logger        *slog.Logger
	stdLogger     *log.Logger
	ctx           context.Context
	cancel        context.CancelFunc
	debug         bool
	redis         *redis.Client
	vehicleState  VehicleState
	readers       []*BatteryReader
}

// BatteryReader represents a single battery reader with its own event loop
type BatteryReader struct {
	// Basic configuration
	index      int
	role       BatteryRole
	deviceName string
	logLevel   int
	logger     *slog.Logger
	service    *Service
	ctx        context.Context

	// NFC HAL - owned exclusively by this reader's goroutine
	hal *hal.PN7150

	// Serializes NFC operations to prevent concurrent access
	nfcMu sync.Mutex

	// State machine (FSM-based)
	fsm          *fsmStateMachine
	fsmCtx       context.Context
	fsmCancel    context.CancelFunc
	data         BMSData
	previousData BMSData

	// Event loop control
	stopChan    chan struct{}
	restartChan chan struct{} // Preemption mechanism

	// Timer management
	stateTimer       *time.Timer
	heartbeatTimer   *time.Timer
	heartbeatRunning bool

	// Event channels
	vehicleStateChan chan VehicleState
	seatboxLockChan  chan bool
	enabledChan      chan bool
	tagEventChan     <-chan hal.TagEvent

	// State tracking
	enabled                  bool
	vehicleState             VehicleState
	seatboxLockClosed        bool
	latchedSeatboxLockClosed bool
	justInserted             bool
	justOpened               bool
	lastCmdTime              time.Time
	initComplete             InitComplete
	previousTagPresent       bool
	tagsDiscovered           bool

	// Fault management
	faultDebounceTimers map[BMSFault]*time.Timer
	faultStates         map[BMSFault]*FaultState

	// Recovery tracking
	commFailureCount   int
	lastSuccessfulComm time.Time

	// Power management
	suspendInhibitor *SuspendInhibitor
}

type FaultState struct {
	Present          bool
	PendingSet       bool
	PendingReset     bool
	SetTimer         *time.Timer
	ResetTimer       *time.Timer
	PublishedToRedis bool
}
