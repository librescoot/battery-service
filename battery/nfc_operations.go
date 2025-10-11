package battery

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"battery-service/nfc/hal"
)

func (r *BatteryReader) deinitializeNFC() {
	if r.hal != nil {
		r.hal.Deinitialize()
	}
	r.service.logger.Infof("Battery %d: NFC deinitialized", r.index)
}

func (r *BatteryReader) startDiscovery() bool {
	pollPeriod := uint(DiscoveryPollFast)
	if r.seatboxLockClosed {
		pollPeriod = DiscoveryPollSlow
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

	r.service.logger.Infof("Battery %d: state=%s, voltage=%dmV, current=%dmA, charge=%d%%, temp=[%d,%d,%d,%d]°C (%s), soh=%d%%, cycles=%d, sn=%s, fw=%s",
		r.index, r.data.State.String(), r.data.Voltage, r.data.Current, r.data.Charge,
		r.data.Temperature[0], r.data.Temperature[1], r.data.Temperature[2], r.data.Temperature[3],
		r.temperatureStateString(), r.data.StateOfHealth, r.data.CycleCount, r.data.SerialNumber, r.data.FwVersion)

	r.setFault(BMSFaultBMSCommsError, false)
	r.setFault(BMSFaultBMSZeroData, false)

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
		r.setFault(BMSFaultBMSCommsError, true)
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

