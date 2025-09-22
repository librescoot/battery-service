package battery

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"battery-service/nfc/hal"
)

func (r *BatteryReader) deinitializeNFC() {
	if r.hal != nil {
		r.hal.Deinitialize()
	}
	r.service.logger.Printf("Battery %d: NFC deinitialized", r.index)
}

func (r *BatteryReader) startDiscovery() {
	pollPeriod := uint(100)
	if r.seatboxLockClosed {
		pollPeriod = 2500
	}

	r.service.logger.Printf("Battery %d: Starting discovery with poll period %d ms", r.index, pollPeriod)
	if err := r.hal.StartDiscovery(pollPeriod); err != nil {
		r.service.logger.Printf("Battery %d: Failed to start discovery: %v", r.index, err)
		r.handleNFCError(err)
		return
	}
	r.service.logger.Printf("Battery %d: Discovery started successfully", r.index)
}

func (r *BatteryReader) stopDiscovery() {
	if err := r.hal.StopDiscovery(); err != nil {
		if strings.Contains(err.Error(), "invalid state for stopping discovery") {
				return
		}
		r.service.logger.Printf("Battery %d: Failed to stop discovery: %v", r.index, err)
	}
}

func (r *BatteryReader) checkForTags() {
	tags, err := r.hal.DetectTags()

	if err != nil {
		if r.previousTagPresent {
			r.service.logger.Printf("Battery %d: Tag departed (detect failed: %v)", r.index, err)
			if r.isIn(StateTagPresent) {
				r.handleDeparture()
				r.transitionTo(StateDiscoverTag)
			}
			r.previousTagPresent = false
		}
		return
	}

	tagPresent := len(tags) > 0

	if tagPresent && !r.previousTagPresent {
		r.service.logger.Printf("Battery %d: Tag arrived: %x", r.index, tags[0].ID)
		r.service.logger.Printf("Battery %d: Processing tag arrival", r.index)
		if r.isIn(StateDiscoverTag) {
			r.justInserted = true
			r.transitionTo(StateTagPresent)
		}
	}

	if !tagPresent && r.previousTagPresent {
		r.service.logger.Printf("Battery %d: Tag departed", r.index)
		if r.isIn(StateTagPresent) {
			r.handleDeparture()
			r.transitionTo(StateDiscoverTag)
		}
	}

	r.previousTagPresent = tagPresent
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
					r.service.logger.Printf("Battery %d: Read verification mismatch at 0x%04X, retry %d", r.index, address, retry)
				}
			}
			checkBuffer = make([]byte, len(data))
			copy(checkBuffer, data)
			check = true
			continue
		}
		lastErr = err
		if nfcErr, ok := err.(*hal.Error); ok && nfcErr.Code == hal.ErrTagDeparted {
			return nil, err
		}
		if r.service.debug {
			r.service.logger.Printf("Battery %d: Read error at 0x%04X: %v (retry %d)", r.index, address, err, retry)
		}
	}

	if lastErr != nil {
		return nil, fmt.Errorf("read failed after %d retries: %w", maxRetries, lastErr)
	}
	return nil, fmt.Errorf("read failed after %d retries", maxRetries)
}

func (r *BatteryReader) readStatus() bool {
	status0, err := r.readWithVerification(0x0300)
	if err != nil {
		r.service.logger.Printf("Battery %d: Failed to read status0: %v", r.index, err)
		r.handleNFCError(err)
		return false
	}

	status1, err := r.readWithVerification(0x0310)
	if err != nil {
		r.service.logger.Printf("Battery %d: Failed to read status1: %v", r.index, err)
		r.handleNFCError(err)
		return false
	}

	status2, err := r.readWithVerification(0x0320)
	if err != nil {
		r.service.logger.Printf("Battery %d: Failed to read status2: %v", r.index, err)
		r.handleNFCError(err)
		return false
	}

	if r.service.debug {
		r.service.logger.Printf("Battery %d: STATUS0 raw BEFORE parsing: %x", r.index, status0)
		r.service.logger.Printf("Battery %d: STATUS1 raw BEFORE parsing: %x", r.index, status1)
		r.service.logger.Printf("Battery %d: STATUS2 raw BEFORE parsing: %x", r.index, status2)
		if len(status1) >= 4 {
			state := uint32(status1[0]) | uint32(status1[1])<<8 | uint32(status1[2])<<16 | uint32(status1[3])<<24
			r.service.logger.Printf("Battery %d: Raw state bytes STATUS1[0-3]: %02x %02x %02x %02x = 0x%08x (%s)",
				r.index, status1[0], status1[1], status1[2], status1[3], state, BMSState(state))
		}
	}

	r.parseStatusData(status0, status1, status2)

	r.service.logger.Printf("Battery %d: state=%s, voltage=%dmV, current=%dmA, charge=%d%%, temp=[%d,%d,%d,%d]°C",
		r.index, r.data.State.String(), r.data.Voltage, r.data.Current, r.data.Charge,
		r.data.Temperature[0], r.data.Temperature[1], r.data.Temperature[2], r.data.Temperature[3])

	r.setNFCFault(BMSFaultBMSCommsError, false)
	r.setNFCFault(BMSFaultBMSZeroData, false)

	r.sendStatusUpdate()

	return true
}

func (r *BatteryReader) writeCommand(cmd BMSCommand) {
	r.takeInhibitor()
	defer r.releaseInhibitor()

	cmdBytes := make([]byte, 4)
	cmdBytes[0] = byte(cmd)
	cmdBytes[1] = byte(cmd >> 8)
	cmdBytes[2] = byte(cmd >> 16)
	cmdBytes[3] = byte(cmd >> 24)

	if err := r.hal.WriteBinary(0x0330, cmdBytes); err != nil {
		r.service.logger.Printf("Battery %d: Failed to write command %s: %v", r.index, cmd, err)
		r.handleNFCError(err)
	} else {
		r.updateLastCmdTime()
		r.service.logger.Printf("Battery %d: Sent command %s", r.index, cmd)
	}

	r.stopDiscovery()
}

func (r *BatteryReader) writeCommandProtected(cmd BMSCommand) {
	r.writeCommand(cmd)
}

func (r *BatteryReader) handleNFCError(err error) {
	r.service.logger.Printf("Battery %d: NFC communication error: %v", r.index, err)

	if err != nil && (strings.Contains(err.Error(), "aborted") && strings.Contains(err.Error(), "after HAL reinitialization")) {
		r.service.logger.Printf("Battery %d: HAL reinitialization completed, restarting state machine from discovery", r.index)
		r.setNFCFault(BMSFaultBMSCommsError, false)
		time.Sleep(100 * time.Millisecond)
		r.transitionTo(StateDiscoverTag)
		return
	}

	if nfcErr, ok := err.(*hal.Error); ok && nfcErr.Code == hal.ErrTagDeparted {
		r.service.logger.Printf("Battery %d: Tag departure detected (HAL error code %d)", r.index, nfcErr.Code)
		if r.isIn(StateTagPresent) {
			r.handleDeparture()
			r.transitionTo(StateDiscoverTag)
		}
		r.previousTagPresent = false
		return
	}

	if err != nil && strings.Contains(err.Error(), "tag departed") {
		r.service.logger.Printf("Battery %d: Tag departure detected from error message", r.index)
		if r.isIn(StateTagPresent) {
			r.handleDeparture()
			r.transitionTo(StateDiscoverTag)
		}
		r.previousTagPresent = false
		return
	}

	r.setNFCFault(BMSFaultBMSCommsError, true)

	if isHALError(err) {
		r.service.logger.Printf("Battery %d: HAL-level error detected, setting fault but not reinitializing", r.index)
	}
}

func isHALError(err error) bool {
	// For now we assume any error is a HAL error
	return true
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
	effectivePresent := r.data.Present && !r.hasCriticalFaults()

	hashKey := fmt.Sprintf("battery:%d", r.index)

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
		r.service.logger.Printf("Battery %d: Publishing state=%s, present=%v, voltage=%d, charge=%d",
			r.index, r.data.State.String(), effectivePresent, r.data.Voltage, r.data.Charge)
	}

	if err := r.redis.HMSet(context.TODO(), hashKey, fields).Err(); err != nil {
		r.service.logger.Printf("Battery %d: Failed to update status hash: %v", r.index, err)
		return
	}

	r.updateFaultSet()

	channel := fmt.Sprintf("battery:%d", r.index)

	r.redis.Publish(context.TODO(), channel, "present")
	r.redis.Publish(context.TODO(), channel, "state")
	r.redis.Publish(context.TODO(), channel, "charge")
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

func (r *BatteryReader) updateFaultSet() {
	faultKey := fmt.Sprintf("battery:%d:fault", r.index)

	r.redis.Del(context.TODO(), faultKey)

	for fault, state := range r.faultStates {
		if state.Present {
			r.redis.SAdd(context.TODO(), faultKey, fmt.Sprintf("%d", fault))
		}
	}

	faultChannel := fmt.Sprintf("battery:%d fault", r.index)
	r.redis.Publish(context.TODO(), faultChannel, "fault")
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
	r.service.logger.Printf("Battery %d: Tag departed", r.index)
	r.data.Present = false
	r.sendStatusUpdate()
}

func (r *BatteryReader) retryZeroDataOrEmptyBattery() {
	// Always continue heartbeat to retry, never give up
	r.setupHeartbeatTimer()

	if r.data.EmptyOr0Data == BMSMaxZeroRetryHeartbeat+1 {
		r.service.logger.Printf("Battery %d: Zero data retry threshold exceeded, but continuing attempts", r.index)
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

	r.clearHeartbeatTimer()

	r.heartbeatTimer = time.NewTimer(heartbeatInterval)

	if !r.heartbeatRunning {
		r.heartbeatRunning = true
		go r.heartbeatMonitor()
	}
}

func (r *BatteryReader) clearHeartbeatTimer() {
	if r.heartbeatTimer != nil {
		r.heartbeatTimer.Stop()
		r.heartbeatTimer = nil
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
			if !r.checkStateCorrect(false) {
				r.handleDeparture()
				r.transitionTo(StateDiscoverTag)
			} else {
				r.triggerHeartbeatTimeout()
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

func (r *BatteryReader) triggerHeartbeatTimeout() {
	if r.isIn(StateHeartbeat) {
		r.transitionTo(StateHeartbeatActions)
	}
}

func (r *BatteryReader) takeInhibitor() {
}

func (r *BatteryReader) releaseInhibitor() {
}

func (r *BatteryReader) setNFCFault(fault BMSFault, present bool) {
	r.setFault(fault, present)
}

func (r *BatteryReader) setStateFault(fault BMSFault, present bool) {
	r.setFault(fault, present)
}
