package battery

import (
	"time"
)

const (
	// Memory addresses for battery data
	addrStatus0 = 0x0300
	addrStatus1 = 0x0310
	addrStatus2 = 0x0320
	addrCommand = 0x0330
	addrConfig  = 0x03A0
	addrSession = 0x03B0

	// Timing constants
	timeHeartbeatIntervalScooter    = 30 * time.Second       // Interval for ScooterHeartbeat when present
	timeCmd                         = 400 * time.Millisecond // Time to wait after sending a command
	timeDeparture                   = 500 * time.Millisecond // Tag departure confirmation timeout
	timeStateVerify                 = 200 * time.Millisecond // Time to verify battery state
	timeReinit                      = 2 * time.Second        // Time to wait before reinit
	timeHALTimeout                  = 7 * time.Second        // Timeout for individual HAL operations
	timeActiveStatusPoll            = 20 * time.Second       // Interval to poll status when battery is active
	timeBattery1MaintPollInterval   = 5 * time.Minute        // Polling interval for battery 1 during maintenance
	timeMaintPollIdleAsleepInterval = 2 * time.Minute        // Polling interval for battery 0 when expected idle/asleep in stand-by

	// Constants for temperature limits
	temperatureStateColdLimit = -10 // Celsius
	temperatureStateHotLimit  = 60  // Celsius

	// Retry constants
	maxReadRetries       = 3  // Increased from 2
	maxWriteRetries      = 3  // Increased from 2
	maxActivationRetries = 3  // Maximum number of times to retry activation sequence
	maxZeroDataRetries   = 10 // Maximum consecutive zero data responses before giving up

	// Critical fault threshold - faults with ID >= this value cause battery to be reported as not present
	criticalFaultThreshold = 33

	// Fault debouncing timing
	faultDebounceSetTime   = 5 * time.Second  // Time to confirm fault presence
	faultDebounceResetTime = 10 * time.Second // Time to confirm fault absence

	// LED maintenance timing
	timeCmdSlow            = 1 * time.Second // Slow command timing for LED operations
	timeCmdFirstOpened     = 2 * time.Second // First seatbox opened command timing
	timeCmdFirstOpenedLong = 3 * time.Second // First seatbox opened when battery asleep

)
