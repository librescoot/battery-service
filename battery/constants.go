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
	timeHeartbeatIntervalScooter    = 5 * time.Second        // Interval for ScooterHeartbeat when present
	timeHeartbeatTimeoutScooter     = 800 * time.Millisecond // Timeout for battery's internal safety check
	timeReadyToScootTimeout         = 350 * time.Millisecond // Max time to wait for BatteryReadyToScoot response
	timePeriodicOnInterval          = 3 * time.Second        // Interval to send BatteryOn when seatbox closed
	timeCmd                         = 200 * time.Millisecond
	timeCmdSlow                     = 800 * time.Millisecond
	timeCmdFirstOpened              = 500 * time.Millisecond // Used after SeatboxClosed/Opened?
	timePresence                    = 10 * time.Second
	timeCheckReader                 = 10 * time.Second
	timeDeparture                   = 500 * time.Millisecond
	timeStateVerify                 = 1000 * time.Millisecond // Time to wait before verifying state change
	timeActivationRetry             = 2 * time.Second         // Time between activation retry attempts
	timeHALTimeout                  = 7 * time.Second         // Timeout for individual HAL operations (Increased from 5s)
	timeActiveStatusPoll            = 10 * time.Second        // Interval to poll status when battery is active
	timeBattery1MaintPollInterval   = 5 * time.Minute         // Polling interval for battery 1 during maintenance
	timeCbBatteryPollInterval       = 10 * time.Second        // Interval to poll cb-battery charge when vehicle is in stand-by
	timeMaintPollIdleAsleepInterval = 2 * time.Minute         // Polling interval for battery 0 when expected idle/asleep in stand-by

	// Constants for temperature limits
	temperatureStateColdLimit = -10 // Celsius
	temperatureStateHotLimit  = 60  // Celsius

	// Retry constants
	maxReadRetries       = 3 // Increased from 2
	maxWriteRetries      = 3 // Increased from 2
	maxActivationRetries = 3 // Maximum number of times to retry activation sequence

	// CB-Battery charge thresholds
	cbBatteryActivationThreshold   = 50 // Percent
	cbBatteryDeactivationThreshold = 90 // Percent

	// Aux-Battery voltage thresholds
	auxBatteryActivationThreshold   = 11000 // millivolts (11V)
	auxBatteryDeactivationThreshold = 12600 // millivolts (12.6V)
)

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
