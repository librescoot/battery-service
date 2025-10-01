package battery

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"battery-service/nfc/hal"

	"github.com/redis/go-redis/v9"
)

func (r *BatteryReader) deinitializeNFC() {
	if r.hal != nil {
		r.hal.Deinitialize()
	}
	r.service.logger.Infof("Battery %d: NFC deinitialized", r.index)
}

func (r *BatteryReader) startDiscovery() bool {
	pollPeriod := uint(100)
	if r.seatboxLockClosed {
		pollPeriod = 2500
	}

	r.service.logger.Debugf("Battery %d: Starting discovery with poll period %d ms", r.index, pollPeriod)
	if err := r.hal.StartDiscovery(pollPeriod); err != nil {
		r.service.logger.Errorf("Battery %d: Failed to start discovery: %v", r.index, err)
		r.handleNFCError(err)
		return false
	}
	r.service.logger.Debugf("Battery %d: Discovery started successfully", r.index)
	return true
}

func (r *BatteryReader) stopDiscovery() {
	if err := r.hal.StopDiscovery(); err != nil {
		if strings.Contains(err.Error(), "invalid state for stopping discovery") {
			r.tagsDiscovered = false
			return
		}
		r.service.logger.Warnf("Battery %d: Failed to stop discovery: %v", r.index, err)
	}
	r.tagsDiscovered = false
}

func (r *BatteryReader) discoverBatteryTag() bool {
	if r.tagsDiscovered {
		return true
	}

	if !r.startDiscovery() {
		return false
	}

	timeout := time.Now().Add(3 * time.Second)
	for time.Now().Before(timeout) {
		tags, err := r.hal.DetectTags()
		if err != nil {
			r.service.logger.Warnf("Battery %d: Tag detection error during discovery: %v", r.index, err)
			r.handleNFCError(err)
			return false
		}
		if len(tags) > 0 {
			r.service.logger.Debugf("Battery %d: Battery tag discovered", r.index)
			r.tagsDiscovered = true
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}

	r.service.logger.Warnf("Battery %d: Tag discovery timeout", r.index)
	return false
}

func (r *BatteryReader) readWithVerification(address uint16) ([]byte, error) {
	const maxRetries = 4
	var checkBuffer []byte
	check := false
	var lastErr error

	for retry := 0; retry < maxRetries; retry++ {
		data, err := r.hal.ReadBinary(address)
		if err == nil {
			if check {
				if len(data) == len(checkBuffer) && bytes.Equal(data, checkBuffer) {
					return data, nil
				}
				if r.service.debug {
					r.service.logger.Debugf("Battery %d: Read verification mismatch at 0x%04X, retry %d", r.index, address, retry)
				}
			}
			checkBuffer = make([]byte, len(data))
			copy(checkBuffer, data)
			check = true
			continue
		}
		lastErr = err

		// Tag departed - don't retry
		if nfcErr, ok := err.(*hal.Error); ok && nfcErr.Code == hal.ErrTagDeparted {
			return nil, err
		}

		// Arbiter busy - retry with SelectTag(0)
		if nfcErr, ok := err.(*hal.Error); ok && nfcErr.Code == hal.ErrArbiterBusy {
			if r.service.debug {
				r.service.logger.Debugf("Battery %d: Arbiter busy at 0x%04X, calling SelectTag(0) (retry %d)", r.index, address, retry)
			}
			if err := r.hal.SelectTag(0); err != nil {
				// Only treat as tag departed if SelectTag explicitly returns ErrTagDeparted
				if nfcErr, ok := err.(*hal.Error); ok && nfcErr.Code == hal.ErrTagDeparted {
					return nil, err
				}
				// Other SelectTag errors (e.g., timeout) - log and continue retry
				r.service.logger.Warnf("Battery %d: SelectTag failed: %v, continuing retry", r.index, err)
				// Don't continue - this retry failed, try the whole read again
			}
			// Reset verification state on arbiter busy
			check = false
			continue
		}

		if r.service.debug {
			r.service.logger.Debugf("Battery %d: Read error at 0x%04X: %v (retry %d)", r.index, address, err, retry)
		}
	}

	if lastErr != nil {
		return nil, fmt.Errorf("read failed after %d retries: %w", maxRetries, lastErr)
	}
	return nil, fmt.Errorf("read failed after %d retries", maxRetries)
}

func (r *BatteryReader) readStatus() bool {
	if !r.discoverBatteryTag() {
		r.service.logger.Warnf("Battery %d: Failed to discover tag before reading status", r.index)
		return false
	}

	status0, err := r.readWithVerification(0x0300)
	if err != nil {
		r.service.logger.Warnf("Battery %d: Failed to read status0: %v", r.index, err)
		r.handleNFCError(err)
		return false
	}

	status1, err := r.readWithVerification(0x0310)
	if err != nil {
		r.service.logger.Warnf("Battery %d: Failed to read status1: %v", r.index, err)
		r.handleNFCError(err)
		return false
	}

	status2, err := r.readWithVerification(0x0320)
	if err != nil {
		r.service.logger.Warnf("Battery %d: Failed to read status2: %v", r.index, err)
		r.handleNFCError(err)
		return false
	}

	if r.service.debug {
		r.service.logger.Debugf("Battery %d: STATUS0 raw BEFORE parsing: %x", r.index, status0)
		r.service.logger.Debugf("Battery %d: STATUS1 raw BEFORE parsing: %x", r.index, status1)
		r.service.logger.Debugf("Battery %d: STATUS2 raw BEFORE parsing: %x", r.index, status2)
		if len(status1) >= 4 {
			state := uint32(status1[0]) | uint32(status1[1])<<8 | uint32(status1[2])<<16 | uint32(status1[3])<<24
			r.service.logger.Debugf("Battery %d: Raw state bytes STATUS1[0-3]: %02x %02x %02x %02x = 0x%08x (%s)",
				r.index, status1[0], status1[1], status1[2], status1[3], state, BMSState(state))
		}
	}

	r.parseStatusData(status0, status1, status2)

	r.service.logger.Infof("Battery %d: state=%s, voltage=%dmV, current=%dmA, charge=%d%%, temp=[%d,%d,%d,%d]°C",
		r.index, r.data.State.String(), r.data.Voltage, r.data.Current, r.data.Charge,
		r.data.Temperature[0], r.data.Temperature[1], r.data.Temperature[2], r.data.Temperature[3])

	r.setNFCFault(BMSFaultBMSCommsError, false)
	r.setNFCFault(BMSFaultBMSZeroData, false)

	// Reset communication failure counter on successful read
	r.commFailureCount = 0
	r.lastSuccessfulComm = time.Now()

	// Reset recovery counter on successful read
	r.data.EmptyOr0Data = 0

	r.sendStatusUpdate()

	return true
}

func (r *BatteryReader) writeCommand(cmd BMSCommand) {
	if !r.discoverBatteryTag() {
		r.service.logger.Warnf("Battery %d: Failed to discover tag before writing command", r.index)
		return
	}

	r.takeInhibitor()
	defer r.releaseInhibitor()

	cmdBytes := make([]byte, 4)
	cmdBytes[0] = byte(cmd)
	cmdBytes[1] = byte(cmd >> 8)
	cmdBytes[2] = byte(cmd >> 16)
	cmdBytes[3] = byte(cmd >> 24)

	// Retry logic for arbiter busy
	const maxRetries = 3
	var lastErr error
	for retry := 0; retry < maxRetries; retry++ {
		err := r.hal.WriteBinary(0x0330, cmdBytes)
		if err == nil {
			r.updateLastCmdTime()
			r.commFailureCount = 0
			r.lastSuccessfulComm = time.Now()
			r.service.logger.Infof("Battery %d: Sent command %s", r.index, cmd)
			r.stopDiscovery()
			return
		}

		lastErr = err

		// Arbiter busy - retry with SelectTag(0)
		if nfcErr, ok := err.(*hal.Error); ok && nfcErr.Code == hal.ErrArbiterBusy {
			if r.service.debug {
				r.service.logger.Debugf("Battery %d: Arbiter busy writing command %s, calling SelectTag(0) (retry %d)", r.index, cmd, retry)
			}
			if err := r.hal.SelectTag(0); err != nil {
				// Only treat as tag departed if SelectTag explicitly returns ErrTagDeparted
				if nfcErr, ok := err.(*hal.Error); ok && nfcErr.Code == hal.ErrTagDeparted {
					lastErr = err
					break
				}
				// Other SelectTag errors (e.g., timeout) - log and continue retry
				r.service.logger.Warnf("Battery %d: SelectTag failed: %v, continuing retry", r.index, err)
			}
			continue
		}

		// Other errors - don't retry
		break
	}

	r.service.logger.Errorf("Battery %d: Failed to write command %s after %d retries: %v", r.index, cmd, maxRetries, lastErr)
	r.handleNFCError(lastErr)
	r.stopDiscovery()
}

func (r *BatteryReader) writeCommandProtected(cmd BMSCommand) {
	r.writeCommand(cmd)
}

func (r *BatteryReader) handleNFCError(err error) {
	r.service.logger.Errorf("Battery %d: NFC communication error: %v", r.index, err)

	// Handle tag departure - no fault, clean transition
	if nfcErr, ok := err.(*hal.Error); ok && nfcErr.Code == hal.ErrTagDeparted {
		r.service.logger.Infof("Battery %d: Tag departure detected (HAL error code %d)", r.index, nfcErr.Code)
		if r.isIn(StateTagPresent) {
			r.handleDeparture()
			r.transitionTo(StateDiscoverTag)
		}
		r.previousTagPresent = false
		return
	}

	if err != nil && strings.Contains(err.Error(), "tag departed") {
		r.service.logger.Infof("Battery %d: Tag departure detected from error message", r.index)
		if r.isIn(StateTagPresent) {
			r.handleDeparture()
			r.transitionTo(StateDiscoverTag)
		}
		r.previousTagPresent = false
		return
	}

	// Handle multiple tags detected
	if nfcErr, ok := err.(*hal.Error); ok && nfcErr.Code == hal.ErrMultipleTags {
		r.service.logger.Warnf("Battery %d: Multiple tags detected - retrying", r.index)
		// Treat as transient - will retry on next discovery
		return
	}

	// Only set BMSCommsError and count failures for actual HAL errors
	if isHALError(err) {
		r.commFailureCount++
		r.service.logger.Warnf("Battery %d: Communication failure %d: %v", r.index, r.commFailureCount, err)
		r.setNFCFault(BMSFaultBMSCommsError, true)
	} else {
		// Transient errors - no fault, will be retried by caller
		r.service.logger.Debugf("Battery %d: Transient error, retrying: %v", r.index, err)
	}
}

func isHALError(err error) bool {
	if err == nil {
		return false
	}

	// Tag departure is not a HAL error
	if nfcErr, ok := err.(*hal.Error); ok && nfcErr.Code == hal.ErrTagDeparted {
		return false
	}
	if strings.Contains(err.Error(), "tag departed") {
		return false
	}

	// Multiple tags is not a HAL error
	if nfcErr, ok := err.(*hal.Error); ok && nfcErr.Code == hal.ErrMultipleTags {
		return false
	}

	// Transient errors are not HAL errors
	if isTransientError(err) {
		return false
	}

	// Everything else is considered a HAL error
	return true
}

func isTransientError(err error) bool {
	// Check for arbiter busy error code
	if nfcErr, ok := err.(*hal.Error); ok && nfcErr.Code == hal.ErrArbiterBusy {
		return true
	}

	return false
}

func (r *BatteryReader) parseStatusData(status0, status1, status2 []byte) {
	if len(status0) == 0 || len(status1) == 0 || len(status2) == 0 {
		r.data.EmptyOr0Data++
		r.setFault(BMSFaultBMSZeroData, true)
		return
	}

	allZero := true
	for _, block := range [][]byte{status0, status1, status2} {
		for _, b := range block {
			if b != 0 {
				allZero = false
				break
			}
		}
		if !allZero {
			break
		}
	}

	if allZero {
		r.data.EmptyOr0Data++
		r.setFault(BMSFaultBMSZeroData, true)
		return
	}

	r.data.Present = true
	r.data.EmptyOr0Data = 0
	r.setFault(BMSFaultBMSZeroData, false)

	if len(status0) >= 16 {
		r.data.Voltage = uint(status0[0]) | uint(status0[1])<<8

		current := int16(uint16(status0[2]) | uint16(status0[3])<<8)
		r.data.Current = int(current)

		r.data.FwVersion = fmt.Sprintf("%d.%d", status0[4], status0[5])

		r.data.RemainingCapacity = uint(status0[6]) | uint(status0[7])<<8

		r.data.FullCapacity = uint(status0[8]) | uint(status0[9])<<8

		if r.data.FullCapacity == 0 {
			r.data.Charge = 0
		} else {
			r.data.Charge = (r.data.RemainingCapacity*100 + r.data.FullCapacity/2) / r.data.FullCapacity
		}

		r.data.FaultCode = uint(status0[10]) | uint(status0[11])<<8

		r.data.Temperature[0] = int(int8(status0[12]))
		r.data.Temperature[1] = int(int8(status0[13]))

		r.data.StateOfHealth = status0[14]

		r.data.LowSOC = status0[15] != 0
	}

	if len(status1) >= 16 {
		state := uint32(status1[0]) | uint32(status1[1])<<8 | uint32(status1[2])<<16 | uint32(status1[3])<<24
		r.data.State = BMSState(state)

		if len(status1) >= 16 {
			r.data.SerialNumber = string(status1[4:16])
		}
	}

	if len(status2) >= 16 {
		if len(r.data.SerialNumber) >= 12 {
			r.data.SerialNumber += string(status2[0:4])
		}

		if len(status2) >= 12 {
			r.data.ManuDate = fmt.Sprintf("%c%c%c%c-%c%c-%c%c",
				status2[4], status2[5], status2[6], status2[7],
				status2[8], status2[9], status2[10], status2[11])
		}

		if len(status2) >= 14 {
			r.data.CycleCount = uint(status2[12]) | uint(status2[13])<<8
		}

		if len(status2) >= 16 {
			r.data.Temperature[2] = int(int8(status2[14]))
			r.data.Temperature[3] = int(int8(status2[15]))
		}
	}

	r.updateTemperatureState()

	r.data.LowSOC = r.data.Charge <= BMSMinSOC

	r.parseHardwareFaults(r.data.FaultCode)

	r.updateFaultsFromBatteryData()
}

func (r *BatteryReader) updateTemperatureState() {
	if len(r.data.Temperature) == 0 {
		r.data.TemperatureState = BMSTemperatureStateUnknown
		return
	}

	maxTemp := r.data.Temperature[0]
	for _, temp := range r.data.Temperature[1:] {
		if temp > maxTemp {
			maxTemp = temp
		}
	}

	if maxTemp < BMSTemperatureStateColdLimit {
		r.data.TemperatureState = BMSTemperatureStateCold
	} else if maxTemp > BMSTemperatureStateHotLimit {
		r.data.TemperatureState = BMSTemperatureStateHot
	} else {
		r.data.TemperatureState = BMSTemperatureStateIdeal
	}
}

func (r *BatteryReader) sendStatusUpdate() {
	effectivePresent := r.data.Present
	previousEffectivePresent := r.previousData.Present

	hashKey := fmt.Sprintf("battery:%d", r.index)
	channel := fmt.Sprintf("battery:%d", r.index)

	// Build fields map for all data
	fields := map[string]interface{}{
		"present":            fmt.Sprintf("%v", effectivePresent),
		"state":              r.data.State.String(),
		"voltage":            fmt.Sprintf("%d", r.data.Voltage),
		"current":            fmt.Sprintf("%d", r.data.Current),
		"charge":             fmt.Sprintf("%d", r.data.Charge),
		"temperature:0":      fmt.Sprintf("%d", r.data.Temperature[0]),
		"temperature:1":      fmt.Sprintf("%d", r.data.Temperature[1]),
		"temperature:2":      fmt.Sprintf("%d", r.data.Temperature[2]),
		"temperature:3":      fmt.Sprintf("%d", r.data.Temperature[3]),
		"temperature-state":  r.temperatureStateString(),
		"cycle-count":        fmt.Sprintf("%d", r.data.CycleCount),
		"state-of-health":    fmt.Sprintf("%d", r.data.StateOfHealth),
		"serial-number":      r.data.SerialNumber,
		"manufacturing-date": r.data.ManuDate,
		"fw-version":         r.data.FwVersion,
	}

	if r.service.debug {
		r.service.logger.Debugf("Battery %d: Publishing state=%s, present=%v, voltage=%d, charge=%d",
			r.index, r.data.State.String(), effectivePresent, r.data.Voltage, r.data.Charge)
	}

	// Use Redis transaction for atomic updates
	pipe := r.redis.TxPipeline()

	// Update all fields in Redis hash
	pipe.HMSet(context.TODO(), hashKey, fields)

	// Update fault set within transaction
	changedFaults, faultChanges := r.updateFaultSetInTransaction(pipe)

	// Publish notifications only for changed fields
	if effectivePresent != previousEffectivePresent {
		pipe.Publish(context.TODO(), channel, "present")
	}
	if r.data.State != r.previousData.State {
		pipe.Publish(context.TODO(), channel, "state")
	}
	if r.data.Charge != r.previousData.Charge {
		pipe.Publish(context.TODO(), channel, "charge")
	}
	if r.data.TemperatureState != r.previousData.TemperatureState {
		pipe.Publish(context.TODO(), channel, "temperature-state")
	}

	// Execute the transaction
	if _, err := pipe.Exec(context.TODO()); err != nil {
		r.service.logger.Errorf("Battery %d: Failed to execute Redis transaction: %v", r.index, err)
		return
	}

	// Update fault tracking flags only after successful transaction
	if faultChanges {
		for _, fault := range changedFaults {
			if state, exists := r.faultStates[fault]; exists {
				state.PublishedToRedis = state.Present
			}
		}
	}

	// Update previous data for next comparison
	r.previousData = r.data
}

func (r *BatteryReader) temperatureStateString() string {
	switch r.data.TemperatureState {
	case BMSTemperatureStateCold:
		return "cold"
	case BMSTemperatureStateHot:
		return "hot"
	case BMSTemperatureStateIdeal:
		return "ideal"
	default:
		return "unknown"
	}
}

func (r *BatteryReader) updateFaultSetInTransaction(pipe redis.Pipeliner) ([]BMSFault, bool) {
	faultKey := fmt.Sprintf("battery:%d:fault", r.index)
	var changedFaults []BMSFault
	anyChanges := false

	for fault, state := range r.faultStates {
		// Only update Redis if the fault state changed
		if state.Present != state.PublishedToRedis {
			if state.Present {
				// Add fault to set
				pipe.SAdd(context.TODO(), faultKey, fmt.Sprintf("%d", fault))
			} else {
				// Remove fault from set
				pipe.SRem(context.TODO(), faultKey, fmt.Sprintf("%d", fault))
			}
			changedFaults = append(changedFaults, fault)
			anyChanges = true
		}
	}

	// Only publish fault notification if there were changes
	if anyChanges {
		faultChannel := fmt.Sprintf("battery:%d", r.index)
		pipe.Publish(context.TODO(), faultChannel, "fault")
	}

	return changedFaults, anyChanges
}

func (r *BatteryReader) updateLastCmdTime() {
	r.lastCmdTime = time.Now()
}

func (r *BatteryReader) getRemainingCmdTime() time.Duration {
	elapsed := time.Since(r.lastCmdTime)
	required := BMSTimeCmd
	if elapsed >= required {
		return 0
	}
	return required - elapsed
}

func (r *BatteryReader) getOpenedTime() time.Duration {
	if r.justInserted {
		return BMSTimeCmd
	}
	if r.justOpened {
		if r.data.State == BMSStateAsleep {
			return BMSTimeCmdFirstOpenedAsleep
		} else {
			return BMSTimeCmdFirstOpenedAwake
		}
	}
	return BMSTimeCmdSlow
}

func (r *BatteryReader) getInsertedTime() time.Duration {
	if r.justInserted {
		return BMSTimeCmd
	}
	return BMSTimeCmdSlow
}

func (r *BatteryReader) sendOnOff() {
	var cmd BMSCommand
	if r.enabled && !r.batteryEmpty() {
		cmd = BMSCmdOn
	} else {
		cmd = BMSCmdOff
	}
	r.writeCommand(cmd)
}

func (r *BatteryReader) batteryEmpty() bool {
	return r.data.LowSOC && r.data.Charge <= BMSMinSOC
}

func (r *BatteryReader) checkInactive() bool {
	correct := (r.data.State == BMSStateAsleep) || (r.data.State == BMSStateIdle)
	r.setStateFault(BMSFaultBMSNotFollowingCmd, !correct)
	return correct
}

func (r *BatteryReader) checkStateCorrect(raiseFault bool) bool {
	var correct bool
	if r.enabled && !r.batteryEmpty() {
		correct = (r.data.State == BMSStateActive)
	} else {
		correct = (r.data.State == BMSStateAsleep) || (r.data.State == BMSStateIdle)
	}

	if raiseFault {
		tryingToSwitchOnWithLowSOCOrFault := r.enabled &&
			(r.data.LowSOC || r.data.FaultCode != 0)

		faultCriteria := !correct &&
			!tryingToSwitchOnWithLowSOCOrFault &&
			r.data.EmptyOr0Data == 0

		r.setStateFault(BMSFaultBMSNotFollowingCmd, faultCriteria)
	}

	return correct
}

func (r *BatteryReader) handleDeparture() {
	r.service.logger.Infof("Battery %d: Tag departed", r.index)
	r.data = BMSData{}
	r.sendStatusUpdate()
}

func (r *BatteryReader) retryZeroDataOrEmptyBattery() {
	if r.data.EmptyOr0Data <= BMSMaxZeroRetryHeartbeat {
		// Continue heartbeat to retry
		r.setupHeartbeatTimer()
	} else {
		// Exceeded threshold - stop heartbeat, will rely on passive discovery
		r.service.logger.Warnf("Battery %d: Giving up active recovery after %d attempts (~110s)", r.index, r.data.EmptyOr0Data)
		r.clearHeartbeatTimer()
	}
}

func (r *BatteryReader) stopTimerIfBatteryEmpty() {
	if r.data.LowSOC && r.data.Charge <= BMSMinSOC {
		r.clearHeartbeatTimer()
		r.data.State = BMSStateIdle
		r.sendStatusUpdate()
	}
}

func (r *BatteryReader) setupHeartbeatTimer() {
	var heartbeatInterval time.Duration

	if r.vehicleState == VehicleStateStandby {
		if r.enabled {
			heartbeatInterval = 40 * time.Second
		} else {
			heartbeatInterval = 30 * time.Minute
		}
	} else {
		if r.enabled {
			heartbeatInterval = BMSTimeUpdateOn // 10s active
		} else {
			heartbeatInterval = 30 * time.Minute
		}
	}

	r.service.logger.Debugf("Battery %d: Setting up heartbeat timer for %s (vehicleState=%s, enabled=%t, state=%s)",
		r.index, heartbeatInterval, r.vehicleState, r.enabled, r.state)

	if r.heartbeatTimer == nil {
		// First time setup
		r.heartbeatTimer = time.NewTimer(heartbeatInterval)
	} else {
		// Reset existing timer - this preserves the channel
		if !r.heartbeatTimer.Stop() {
			// Timer already fired, drain the channel
			select {
			case <-r.heartbeatTimer.C:
			default:
			}
		}
		r.heartbeatTimer.Reset(heartbeatInterval)
	}

	if !r.heartbeatRunning {
		r.heartbeatRunning = true
		r.service.logger.Debugf("Battery %d: Starting heartbeat monitor goroutine", r.index)
		go r.heartbeatMonitor()
	}
}

func (r *BatteryReader) clearHeartbeatTimer() {
	if r.heartbeatTimer != nil {
		// Stop the timer but don't set to nil - preserve the channel for heartbeat monitor
		r.heartbeatTimer.Stop()
	}
}

func (r *BatteryReader) getHeartbeatInterval() time.Duration {
	if r.vehicleState == VehicleStateStandby {
		if r.enabled {
			return r.service.config.HeartbeatTimeout
		} else {
			return r.service.config.OffUpdateTime
		}
	} else {
		if r.enabled {
			return BMSTimeUpdateOn
		} else {
			return r.service.config.OffUpdateTime
		}
	}
}

func (r *BatteryReader) heartbeatMonitor() {
	defer func() {
		r.heartbeatRunning = false
	}()

	for {
		select {
		case <-r.stopChan:
			return

		case <-r.getHeartbeatTimer():
			// Heartbeat timeout during any heartbeat-related state
			r.service.logger.Debugf("Battery %d: Heartbeat timer fired, state=%s, inHeartbeatTree=%t",
				r.index, r.state, r.isInHeartbeatTree())
			if r.isInHeartbeatTree() {
				stateCorrect := r.checkStateCorrect(false)
				r.service.logger.Debugf("Battery %d: State correct check: %t (current=%s, expected=%s)",
					r.index, stateCorrect, r.data.State, func() string {
						if r.enabled && !r.batteryEmpty() {
							return "active"
						}
						return "asleep/idle"
					}())
				if !stateCorrect {
					// Battery state wrong after timeout - force rediscovery
					r.service.logger.Warnf("Battery %d: Recovery timeout after %s - forcing rediscovery", r.index, r.getHeartbeatInterval())
					r.handleDeparture()
					r.transitionTo(StateDiscoverTag)
				} else {
					// State correct - restart heartbeat cycle
					r.service.logger.Warnf("Battery %d: Heartbeat timeout - restarting cycle", r.index)
					r.transitionTo(StateHeartbeat)
				}
			}
		}
	}
}

func (r *BatteryReader) getHeartbeatTimer() <-chan time.Time {
	if r.heartbeatTimer == nil {
		return nil
	}
	return r.heartbeatTimer.C
}

// Check if we're in any heartbeat-related state (including recovery states)
func (r *BatteryReader) isInHeartbeatTree() bool {
	switch r.state {
	case StateHeartbeat, StateHeartbeatActions, StateSendClosed,
		StateSendOnOff, StateCondStateOK, StateSendInsertedClosed, StateWaitUpdate:
		return true
	default:
		return false
	}
}

// takeInhibitor acquires a systemd suspend inhibitor lock to prevent system suspend
// during critical NFC operations.
//
// The function retries up to BMSMaxRetryTakeInhibitor times with a 1ms delay between
// attempts. If acquisition fails, the error is logged but the system continues to
// operate (degraded mode).
func (r *BatteryReader) takeInhibitor() {
	// If we already have an inhibitor, don't acquire another one
	if r.suspendInhibitor != nil && r.suspendInhibitor.IsActive() {
		r.service.logger.Debugf("Battery %d: Inhibitor already acquired", r.index)
		return
	}

	var lastErr error
	for attempt := 0; attempt < BMSMaxRetryTakeInhibitor; attempt++ {
		inhibitor, err := NewSuspendInhibitor(
			"BATTERY_NFC_TRANSACTION_INHIBITOR",
			"NFC_TRANSACTION",
			"block",
		)
		if err == nil {
			r.suspendInhibitor = inhibitor
			r.service.logger.Debugf("Battery %d: Inhibitor acquired", r.index)
			return
		}

		lastErr = err
		if attempt == 0 {
			r.service.logger.Debugf("Battery %d: Failed to acquire suspend inhibitor (attempt %d/%d): %v",
				r.index, attempt+1, BMSMaxRetryTakeInhibitor, err)
		} else {
			r.service.logger.Warnf("Battery %d: Failed to acquire suspend inhibitor (attempt %d/%d): %v",
				r.index, attempt+1, BMSMaxRetryTakeInhibitor, err)
		}

		if attempt < BMSMaxRetryTakeInhibitor-1 {
			time.Sleep(1 * time.Millisecond)
		}
	}

	// If we exhausted all retries, log the final error
	r.service.logger.Errorf("Battery %d: Failed to acquire suspend inhibitor after %d attempts: %v",
		r.index, BMSMaxRetryTakeInhibitor, lastErr)
}

// releaseInhibitor releases the systemd suspend inhibitor lock, allowing the system
// to suspend again. This should be called after critical NFC operations complete.
func (r *BatteryReader) releaseInhibitor() {
	if r.suspendInhibitor == nil {
		return
	}

	r.service.logger.Debugf("Battery %d: Releasing inhibitor", r.index)

	if err := r.suspendInhibitor.Release(); err != nil {
		r.service.logger.Warnf("Battery %d: Failed to release inhibitor: %v", r.index, err)
	}

	r.suspendInhibitor = nil
}

func (r *BatteryReader) setNFCFault(fault BMSFault, present bool) {
	r.setFault(fault, present)
}

func (r *BatteryReader) setStateFault(fault BMSFault, present bool) {
	r.setFault(fault, present)
}
