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
	timeHeartbeatIntervalScooter    = 5 * time.Second         // Reduced from 10s - unified polling interval
	timeCmd                         = 100 * time.Millisecond  // Reduced from 400ms for faster NFC operations
	timeDeparture                   = 250 * time.Millisecond  // Reduced from 500ms
	timeStateVerify                 = 100 * time.Millisecond  // Reduced from 200ms
	timeReinit                      = 2 * time.Second         // Time to wait before reinit (matches C's BMS_TIME_REINIT)
	timeHALTimeout                  = 5 * time.Second         // Reduced from 7s
	timeActiveStatusPoll            = 10 * time.Second        // Reduced from 20s for faster active monitoring
	timeBattery1MaintPollInterval   = 30 * time.Second       // Reduced from 5 minutes for better responsiveness
	timeMaintPollIdleAsleepInterval = 30 * time.Second       // Reduced from 2 minutes for better responsiveness

	// Constants for temperature limits
	temperatureStateColdLimit = -10 // Celsius
	temperatureStateHotLimit  = 60  // Celsius

	// Retry constants
	maxReadRetries       = 3 // Increased from 2
	maxWriteRetries      = 3 // Increased from 2
	maxActivationRetries = 3 // Maximum number of times to retry activation sequence

)
