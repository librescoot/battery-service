package battery

import (
	"context"
	"log"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"battery-service/battery/fsm"
	"github.com/librescoot/pn7150"

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

// Sensors 0 and 1 move together and have been observed pegged at exactly these
// two values while sensors 2 and 3 read ambient, in frames that were otherwise
// intact and that recovered within seconds. Sensors 2 and 3 have never been
// seen at either value. Both readings therefore mean "no measurement", not a
// temperature, and only the 0/1 pair is checked for them.
const (
	sensors01ExtremeHot  = 105
	sensors01ExtremeCold = -40
)

// An extreme on sensors 0 and 1 is believed only when the previous sample was
// already near it. A pack this size cannot move ~25 K within one poll interval,
// so a jump straight to an extreme is the artifact rather than a measurement.
// A pack that really reaches these temperatures also sets its own latching
// fault bits, which this service parses separately.
const (
	sensors01HotCredibleFrom = 80
	sensors01ColdCredibleTo  = -20
)

// A previous sample older than this says nothing about whether an extreme is
// real, so it counts as absent. Covers the active pack's heartbeat interval
// with margin; a slot polled more slowly than this never accepts an extreme,
// which is the safe direction.
const sensors01PreviousMaxAge = 60 * time.Second

// How long after a tag change to ask for an early read if the pack has not
// been seen writing by then. The pack takes 3.0 to 3.5 s on this hardware, so a
// read requested at this point lands just after it. Without it the next read
// could be a whole poll interval away: 40 s on the active slot, half an hour on
// an idle one.
const tagConfirmAfter = 3 * time.Second

// tagFreshness tracks whether the pack has been seen writing to this slot's tag
// since the tag last changed.
//
// A pack carries a tag on each side and writes its status to the one facing the
// reader, so the tag on the other side keeps whatever was last written to it,
// however long ago. Reading a pack through a tag it has not written to recently
// yields a complete, self-consistent frame of the right pack that is simply
// old: correct serial, stale charge, voltage and temperatures. Re-reading does
// not separate the two, since the tag serves the same bytes until the pack
// overwrites them. What does is the pack writing, which shows up as the frame
// changing.
//
// Frames are published throughout, so a slot never looks empty while this is
// pending. Only the dual-battery voltage delta check waits on it, because that
// one latches a decision about whether the second pack may activate.
type tagFreshness struct {
	unconfirmed bool
	haveFrame   bool
	since       time.Time
	frame       BMSData
	timer       *time.Timer
}

// confirm marks the tag as written to and cancels any pending early read.
func (f *tagFreshness) confirm() {
	f.unconfirmed = false
	f.stopTimer()
}

// stopTimer cancels the early-read timer if one is pending.
func (f *tagFreshness) stopTimer() {
	if f.timer != nil {
		f.timer.Stop()
		f.timer = nil
	}
}

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

// sensors01Sample is the last sample whose sensor 0/1 readings were believed.
// Voltage and current are kept alongside because they have been seen wrong in
// the same frames: the frame that pegged both sensors at the hot extreme also
// reported a voltage around a third of the pack's true voltage, with charge,
// serial and the other two sensors all correct.
type sensors01Sample struct {
	valid       bool
	temperature [2]int
	voltage     uint
	current     int
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
	ManufacturingDate string              `json:"manufacturing_date"`
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
	BMSMinSOC                = 0
)

// Dual battery protection
const DefaultMaxVoltageDeltaMV = 1000 // Default maximum voltage delta (mV) for dual battery activation

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

// ServiceConfig holds runtime configuration. Fields that can be reloaded
// live (via Redis setting pub/sub) are atomic so reader goroutines see
// consistent values without a lock.
type ServiceConfig struct {
	RedisServerAddress      string
	RedisServerPort         uint16
	HeartbeatTimeout        time.Duration
	OffUpdateTime           time.Duration
	KeepActiveOnSeatboxOpen atomic.Bool
	MaxVoltageDelta         atomic.Uint64 // mV, 0 = disabled

	// Aux-battery low-voltage override for KeepActiveOnSeatboxOpen.
	// While aux voltage is below Enter (mV), the override engages and the
	// effective keep-active flag is forced true. It disengages when aux
	// voltage rises to at-or-above Exit (mV). AuxLowKeepActive holds the
	// current latched override state.
	AuxLowKeepActiveEnterMv atomic.Uint64
	AuxLowKeepActiveExitMv  atomic.Uint64
	AuxLowKeepActive        atomic.Bool
}

// EffectiveKeepActiveOnSeatboxOpen returns the effective keep-active flag,
// combining the user setting with the aux-low override.
func (c *ServiceConfig) EffectiveKeepActiveOnSeatboxOpen() bool {
	return c.KeepActiveOnSeatboxOpen.Load() || c.AuxLowKeepActive.Load()
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
	fsm            *fsmStateMachine
	fsmCtx         context.Context
	fsmCancel      context.CancelFunc
	data           BMSData
	previousData   BMSData
	previousFields map[string]any

	// Event loop control
	stopChan    chan struct{}
	restartChan chan struct{} // Preemption mechanism

	// Timer management
	heartbeatTimer   *time.Timer
	heartbeatRunning bool

	// Event channels
	vehicleStateChan chan VehicleState
	seatboxLockChan  chan bool
	enabledChan      chan bool

	// State tracking
	enabled                  bool
	voltageDeltaBlocked      bool // blocks activation when voltage delta is too large
	voltageDeltaChecked      bool // latched once delta has been evaluated since last insertion
	vehicleState             VehicleState
	seatboxLockClosed        bool
	latchedSeatboxLockClosed bool
	lastCmdTime              time.Time
	initComplete             InitComplete
	initCompleteSent         bool // EvInitComplete already dispatched; suppresses duplicates
	previousTagPresent       bool
	tagsDiscovered           bool
	// UID of the tag currently selected on this reader. A pack carries a tag
	// on each side and only the one facing this slot's reader is ever seen, so
	// the UID identifies the side as well as the pack.
	currentTagUID []byte

	// Tracks whether the pack has been seen writing to this slot's tag since
	// the tag changed.
	fresh tagFreshness

	// Last sample whose sensor 0/1 readings were believed. Used both to judge
	// whether a new extreme is real and to stand in for the fields that go
	// wrong alongside it.
	lastGoodSensors01     sensors01Sample
	lastGoodSensors01Time time.Time
	sensors01Artifacts    uint64

	// Fault management
	faultMu              sync.Mutex
	faultStates          map[BMSFault]*FaultState
	nfcReaderErrorRaised bool // NFC reader fault armed for the current error episode (FSM goroutine only)

	// Recovery tracking
	commFailureCount   int
	lastSuccessfulComm time.Time
	commsFaultCount    int

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
