package battery

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"battery-service/battery/fsm"
	"github.com/librescoot/pn7150"
)

func (r *BatteryReader) deinitializeNFC() {
	if r.hal != nil {
		r.hal.Deinitialize()
	}
	r.logger.Info("NFC deinitialized")
}

func (r *BatteryReader) startDiscovery() bool {
	pollPeriod := uint(DiscoveryPollFast)
	if r.seatboxLockClosed {
		pollPeriod = DiscoveryPollSlow
	}

	r.logger.Debug(fmt.Sprintf("Starting discovery with poll period %d ms", pollPeriod))
	if err := r.hal.StartDiscovery(pollPeriod); err != nil {
		// Check for semantic error (status 0x06) which indicates cold boot condition
		if strings.Contains(err.Error(), "status: 06") {
			r.logger.Warn("Discovery failed with semantic error (cold boot condition), attempting reinitialization")
			// Attempt full reinitialization once
			if reinitErr := r.hal.FullReinitialize(); reinitErr != nil {
				r.logger.Error(fmt.Sprintf("Reinitialization failed: %v", reinitErr))
				r.handleNFCError(reinitErr)
				return false
			}
			// Try starting discovery again after reinitialization
			if err := r.hal.StartDiscovery(pollPeriod); err != nil {
				r.logger.Error(fmt.Sprintf("Failed to start discovery after reinitialization: %v", err))
				r.handleNFCError(err)
				return false
			}
			r.logger.Info("Discovery started successfully after reinitialization")
			return true
		}
		r.logger.Error(fmt.Sprintf("Failed to start discovery: %v", err))
		r.handleNFCError(err)
		return false
	}
	r.logger.Debug("Discovery started successfully")
	return true
}

func (r *BatteryReader) stopDiscovery() {
	r.logger.Debug("Stopping discovery")
	if err := r.hal.StopDiscovery(); err != nil {
		if strings.Contains(err.Error(), "invalid state for stopping discovery") {
			r.logger.Debug("Discovery already stopped")
			r.tagsDiscovered = false
			return
		}
		// Handle HAL errors from stop_discovery (triggers reinit if I2C stuck)
		r.logger.Warn(fmt.Sprintf("Failed to stop discovery: %v", err))
		r.handleNFCError(err)
	}
	r.logger.Debug("Discovery stopped successfully")
	r.tagsDiscovered = false
}

func (r *BatteryReader) discoverBatteryTag() bool {
	if r.tagsDiscovered {
		return true
	}

	if !r.startDiscovery() {
		return false
	}

	// Synchronous blocking wait for tag
	tags, err := r.hal.DetectTags()
	if err != nil {
		r.logger.Warn(fmt.Sprintf("Failed to detect tags: %v", err))
		r.handleNFCError(err)
		return false
	}

	if len(tags) == 0 {
		r.logger.Warn("DetectTags returned no tags")
		// If we previously had a tag present and now detect no tags, treat as tag departure
		if r.previousTagPresent {
			r.logger.Info("Tag departed (no tags detected)")
			r.previousTagPresent = false
			r.tagsDiscovered = false
			if r.fsm.IsInState(fsm.StateTagPresent) {
				r.handleDeparture()
				r.fsm.SendEvent(fsm.EvTagDeparted)
			}
		}
		return false
	}

	r.tagsDiscovered = true
	r.previousTagPresent = true
	r.logger.Debug(fmt.Sprintf("Tag arrived: %X", tags[0].ID))

	return true
}

func (r *BatteryReader) pollForTagArrival() {
	r.nfcMu.Lock()
	defer r.nfcMu.Unlock()

	// Poll for tag arrival in tag_absent state
	tags, err := r.hal.DetectTags()
	if err == nil && len(tags) > 0 {
		r.tagsDiscovered = true
		r.previousTagPresent = true
		r.logger.Debug(fmt.Sprintf("Tag arrived: %X", tags[0].ID))
		r.fsm.SendEvent(fsm.EvTagArrived)
	}
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
					r.logger.Debug(fmt.Sprintf("Read verification mismatch at 0x%04X, retry %d", address, retry))
				}
			}
			checkBuffer = make([]byte, len(data))
			copy(checkBuffer, data)
			check = true
			continue
		}
		lastErr = err

		// Tag departed - don't retry
		if isTagDepartedError(err) {
			return nil, err
		}

		// Arbiter busy - retry with SelectTag(0)
		if isArbiterBusyError(err) {
			if r.service.debug {
				r.logger.Debug(fmt.Sprintf("Arbiter busy at 0x%04X, calling SelectTag(0) (retry %d)", address, retry))
			}
			if err := r.hal.SelectTag(0); err != nil {
				if isTagDepartedError(err) {
					return nil, err
				}
				r.logger.Warn(fmt.Sprintf("SelectTag failed: %v, continuing retry", err))
			}
			check = false
			continue
		}

		if r.service.debug {
			r.logger.Debug(fmt.Sprintf("Read error at 0x%04X: %v (retry %d)", address, err, retry))
		}
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("read failed after %d retries", maxRetries)
}

func (r *BatteryReader) readStatus() bool {
	r.nfcMu.Lock()
	defer r.nfcMu.Unlock()

	// Only start discovery if not already discovered
	if !r.tagsDiscovered {
		if !r.discoverBatteryTag() {
			return false
		}
	}
	// If already discovered, proceed directly to read

	status0, err := r.readWithVerification(0x0300)
	if err != nil {
		r.logger.Warn(fmt.Sprintf("Failed to read status0: %v", err))
		r.handleNFCError(err)
		return false
	}

	status1, err := r.readWithVerification(0x0310)
	if err != nil {
		r.logger.Warn(fmt.Sprintf("Failed to read status1: %v", err))
		r.handleNFCError(err)
		return false
	}

	status2, err := r.readWithVerification(0x0320)
	if err != nil {
		r.logger.Warn(fmt.Sprintf("Failed to read status2: %v", err))
		r.handleNFCError(err)
		return false
	}

	if r.service.debug {
		r.logger.Debug(fmt.Sprintf("STATUS0 raw: %x", status0))
		r.logger.Debug(fmt.Sprintf("STATUS1 raw: %x", status1))
		r.logger.Debug(fmt.Sprintf("STATUS2 raw: %x", status2))
		if len(status1) >= 4 {
			state := uint32(status1[0]) | uint32(status1[1])<<8 | uint32(status1[2])<<16 | uint32(status1[3])<<24
			r.logger.Debug(fmt.Sprintf("Raw state bytes STATUS1[0-3]: %02x %02x %02x %02x = 0x%08x (%s)",
				status1[0], status1[1], status1[2], status1[3], state, BMSState(state)))
		}
	}

	r.parseStatusData(status0, status1, status2)

	r.logger.Info(fmt.Sprintf("Status: state=%s, voltage=%dmV, current=%dmA, charge=%d%%, temp=[%d,%d,%d,%d]°C (%s), soh=%d%%, cycles=%d, sn=%s, fw=%s",
		r.data.State.String(), r.data.Voltage, r.data.Current, r.data.Charge,
		r.data.Temperature[0], r.data.Temperature[1], r.data.Temperature[2], r.data.Temperature[3],
		r.temperatureStateString(), r.data.StateOfHealth, r.data.CycleCount, r.data.SerialNumber, r.data.FwVersion))

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

func (r *BatteryReader) WriteCommand(cmd fsm.BMSCommand) {
	r.nfcMu.Lock()
	defer r.nfcMu.Unlock()

	// Only start discovery if not already discovered
	if !r.tagsDiscovered {
		if !r.discoverBatteryTag() {
			r.stopDiscovery()
			return
		}
	}
	// If already discovered, proceed directly to write

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
			r.logger.Info(fmt.Sprintf("Sent command: %s", cmd))
			r.stopDiscovery()
			return
		}

		lastErr = err

		// Arbiter busy - retry with SelectTag(0)
		if isArbiterBusyError(err) {
			if r.service.debug {
				r.logger.Debug(fmt.Sprintf("Arbiter busy writing command %s, calling SelectTag(0) (retry %d)", cmd, retry))
			}
			if err := r.hal.SelectTag(0); err != nil {
				if isTagDepartedError(err) {
					lastErr = err
					break
				}
				r.logger.Warn(fmt.Sprintf("SelectTag failed: %v, continuing retry", err))
			}
			continue
		}

		break
	}

	r.logger.Error(fmt.Sprintf("Failed to write command %s after %d retries: %v", cmd, maxRetries, lastErr))
	r.handleNFCError(lastErr)
	r.stopDiscovery()
}

func (r *BatteryReader) handleNFCError(err error) {
	r.logger.Error(fmt.Sprintf("NFC communication error: %v (type: %T)", err, err))
	r.logger.Warn(fmt.Sprintf("Error type: isHALError=%t, isI2CError=%t, isNCIError=%t, isTransient=%t, isTagDeparted=%t",
		isHALError(err), hal.IsI2CError(err), hal.IsNCIError(err), isTransientError(err), isTagDepartedError(err)))

	// Handle tag departure - no fault, clean transition
	if isTagDepartedError(err) {
		r.logger.Info("Tag departure detected")
		if r.fsm.IsInState(fsm.StateTagPresent) {
			r.handleDeparture()
			r.fsm.SendEvent(fsm.EvTagDeparted)
		}
		r.previousTagPresent = false
		return
	}

	if isMultipleTagsError(err) {
		r.logger.Warn("Multiple tags detected - retrying")
		return
	}

	if isHALError(err) {
		r.commFailureCount++
		r.logger.Warn(fmt.Sprintf("Communication failure %d: %v", r.commFailureCount, err))

		// Only set fault if we expected the battery to be present (in StateTagPresent hierarchy)
		// Don't set fault during normal removal, absence, or reinit recovery
		if r.fsm.IsInState(fsm.StateTagPresent) {
			r.setFault(BMSFaultBMSCommsError, true)
			// Only trigger reinit if we expected battery to be present
			r.fsm.SendEvent(fsm.EvReinit)
		}
	} else {
		r.logger.Debug(fmt.Sprintf("Transient error, retrying: %v", err))
	}
}

func isTagDepartedError(err error) bool {
	return hal.IsTagDepartedError(err)
}

func isMultipleTagsError(err error) bool {
	return hal.IsMultipleTagsError(err)
}

func isArbiterBusyError(err error) bool {
	return hal.IsArbiterBusyError(err)
}

func isTransientError(err error) bool {
	return hal.IsTransientError(err)
}

func isHALError(err error) bool {
	return hal.IsHALError(err)
}
