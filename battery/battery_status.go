package battery

import (
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

func (r *BatteryReader) parseStatusData(status0, status1, status2 []byte) bool {
	if len(status0) == 0 || len(status1) == 0 || len(status2) == 0 {
		r.data.EmptyOr0Data++
		r.setFault(BMSFaultBMSZeroData, true)
		return false
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
		return false
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
			r.data.ManufacturingDate = fmt.Sprintf("%c%c%c%c-%c%c-%c%c",
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

	r.updateTemperatureState(r.applySensors01Guard(time.Now()))

	r.updateFaultsFromBatteryData()

	return true
}

// sensors01Suspect reports whether temperature sensors 0 and 1 are both sitting
// on the same extreme with nothing to make that credible.
//
// The two always move together, so both landing on the same extreme in one
// sample, while sensors 2 and 3 carry on reading normally, is the shape of the
// artifact rather than of a pack getting hot or cold. A real excursion arrives
// gradually and leaves a previous sample near the extreme.
func sensors01Suspect(t0, t1 int, prev sensors01Sample, prevAge time.Duration) bool {
	atHot := t0 == sensors01ExtremeHot && t1 == sensors01ExtremeHot
	atCold := t0 == sensors01ExtremeCold && t1 == sensors01ExtremeCold
	if !atHot && !atCold {
		return false
	}

	// Nothing to judge against, so there is no way to tell the artifact from a
	// real excursion. Substitute rather than accept.
	if !prev.valid || prevAge > sensors01PreviousMaxAge {
		return true
	}

	if atHot {
		return prev.temperature[0] < sensors01HotCredibleFrom && prev.temperature[1] < sensors01HotCredibleFrom
	}
	return prev.temperature[0] > sensors01ColdCredibleTo && prev.temperature[1] > sensors01ColdCredibleTo
}

// applySensors01Guard stands the last believed values in for the fields that go
// wrong together with sensors 0 and 1, and records this sample as the new
// reference when its readings are believed.
//
// It returns whether sensors 0 and 1 may be used to decide the temperature
// state: true when this sample is believed, and also when the last believed
// sample stood in for it. False only when there is nothing to substitute, in
// which case the state has to be decided on sensors 2 and 3.
func (r *BatteryReader) applySensors01Guard(now time.Time) bool {
	if !sensors01Suspect(r.data.Temperature[0], r.data.Temperature[1], r.lastGoodSensors01, now.Sub(r.lastGoodSensors01Time)) {
		r.lastGoodSensors01 = sensors01Sample{
			valid:       true,
			temperature: [2]int{r.data.Temperature[0], r.data.Temperature[1]},
			voltage:     r.data.Voltage,
			current:     r.data.Current,
		}
		r.lastGoodSensors01Time = now
		return true
	}

	r.sensors01Artifacts++
	r.logger.Warn(fmt.Sprintf(
		"Temperature sensors 0/1 both reading exactly %d with no comparable previous sample, holding last known good temperature, voltage and current (occurrence %d)",
		r.data.Temperature[0], r.sensors01Artifacts))

	if !r.lastGoodSensors01.valid {
		// Nothing to substitute. Leave the raw values in place so the reading
		// stays visible, and let the caller decide the state on sensors 2 and 3.
		return false
	}

	r.data.Temperature[0] = r.lastGoodSensors01.temperature[0]
	r.data.Temperature[1] = r.lastGoodSensors01.temperature[1]
	r.data.Voltage = r.lastGoodSensors01.voltage
	r.data.Current = r.lastGoodSensors01.current
	return true
}

// updateTemperatureState decides the pack's temperature state. useSensors01 is
// false when sensors 0 and 1 are sitting on an extreme that cannot be believed
// and there is no earlier sample to stand in, leaving sensors 2 and 3 as the
// only usable measurements.
func (r *BatteryReader) updateTemperatureState(useSensors01 bool) {
	if len(r.data.Temperature) == 0 {
		r.data.TemperatureState = BMSTemperatureStateUnknown
		return
	}

	channels := r.data.Temperature[:]
	if !useSensors01 {
		channels = r.data.Temperature[2:]
	}

	// A single sensor out of range gates the whole pack: any sensor at or
	// below the cold limit is cold, any sensor at or above the hot limit is
	// hot. The first out-of-range sensor in index order decides. Taking only
	// the hottest sensor would miss a single cold cell, which downstream
	// consumers use to gate recuperation/charging.
	for _, temp := range channels {
		if temp <= BMSTemperatureStateColdLimit {
			r.data.TemperatureState = BMSTemperatureStateCold
			return
		}
		if temp >= BMSTemperatureStateHotLimit {
			r.data.TemperatureState = BMSTemperatureStateHot
			return
		}
	}
	r.data.TemperatureState = BMSTemperatureStateIdeal
}

// statusFrameChanged reports whether anything the pack measures or counts has
// moved between two frames. A tag the pack has written to since the last read
// shows movement; one still serving an older snapshot returns the same bytes
// however often it is read.
func statusFrameChanged(a, b BMSData) bool {
	return a.Voltage != b.Voltage ||
		a.Current != b.Current ||
		a.Charge != b.Charge ||
		a.State != b.State ||
		a.Temperature != b.Temperature ||
		a.CycleCount != b.CycleCount
}

// noteFrameFreshness records the first frame after a tag change and marks the
// tag confirmed once a later frame differs, which is the pack having written to
// it. Frames are published regardless; this only decides whether the
// dual-battery voltage delta check may act on them.
func (r *BatteryReader) noteFrameFreshness() {
	if !r.fresh.unconfirmed {
		return
	}

	if !r.fresh.haveFrame {
		r.fresh.frame = r.data
		r.fresh.haveFrame = true
		return
	}

	if statusFrameChanged(r.data, r.fresh.frame) {
		r.fresh.confirm()
		return
	}

	// Backstop for the early read: past the bound, act on what we have rather
	// than leaving the delta check waiting indefinitely on a pack whose
	// readings never move.
	if time.Since(r.fresh.since) >= tagConfirmAfter {
		r.logger.Warn(fmt.Sprintf(
			"Tag data unchanged %s after the tag changed, treating it as current", tagConfirmAfter))
		r.fresh.confirm()
	}
}

func (r *BatteryReader) sendStatusUpdate() {
	r.noteFrameFreshness()

	// Voltage delta protection: one-shot gate at battery 1's first activation.
	// Retry while not yet evaluated to cover the startup race where battery 0's
	// voltage isn't yet in Redis when battery 1 first appears. Once the check
	// has run, latch — voltage drift after activation must not disable a
	// running battery. The not-present -> present edge resets the latch so a
	// fresh insertion gets a new evaluation.
	if r.index > 0 && r.data.Present && !r.previousData.Present {
		r.voltageDeltaChecked = false
		r.voltageDeltaBlocked = false
	}

	// Wait for the tag to be confirmed before latching this decision. A tag the
	// pack has not written to recently reports a voltage from the pack's
	// previous life, which would block or permit activation on the wrong number.
	maxDelta := r.service.config.MaxVoltageDelta.Load()
	if r.index > 0 && maxDelta > 0 && r.role == BatteryRoleActive &&
		!r.voltageDeltaChecked && !r.fresh.unconfirmed && r.data.Present && r.data.Voltage > 0 {
		v0, err := r.service.redis.HGet(r.ctx, "battery:0", "voltage").Uint64()
		if err == nil && v0 > 0 {
			v1 := uint64(r.data.Voltage)
			var delta uint64
			if v0 > v1 {
				delta = v0 - v1
			} else {
				delta = v1 - v0
			}
			r.voltageDeltaChecked = true
			if delta > maxDelta {
				r.logger.Warn(fmt.Sprintf("Voltage delta too large (%dmV > %dmV, battery0=%dmV, battery1=%dmV) - blocking battery 1 activation",
					delta, maxDelta, v0, v1))
				r.voltageDeltaBlocked = true
				if r.enabled {
					r.enabled = false
					r.triggerRestart()
				}
			}
		}
	}

	effectivePresent := r.data.Present

	hashKey := fmt.Sprintf("battery:%d", r.index)
	channel := fmt.Sprintf("battery:%d", r.index)

	// Build fields map for all data
	fields := map[string]any{
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
		"manufacturing-date": r.data.ManufacturingDate,
		"fw-version":         r.data.FwVersion,
	}

	if r.service.debug {
		r.logger.Debug(fmt.Sprintf("Publishing state=%s, present=%v, voltage=%d, charge=%d",
			r.data.State.String(), effectivePresent, r.data.Voltage, r.data.Charge))
	}

	// Use Redis transaction for atomic updates
	pipe := r.service.redis.TxPipeline()

	// Update all fields in Redis hash
	pipe.HMSet(r.ctx, hashKey, fields)

	// Update fault set within transaction
	changedFaults, faultChanges := r.updateFaultSetInTransaction(pipe)

	// Publish notifications for all changed fields
	for field, value := range fields {
		if value != r.previousFields[field] {
			pipe.Publish(r.ctx, channel, field)
		}
	}

	// Execute the transaction
	if _, err := pipe.Exec(r.ctx); err != nil {
		r.logger.Error(fmt.Sprintf("Failed to execute Redis transaction: %v", err))
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
	r.previousFields = fields
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
				pipe.SAdd(r.ctx, faultKey, fmt.Sprintf("%d", fault))
			} else {
				// Remove fault from set
				pipe.SRem(r.ctx, faultKey, fmt.Sprintf("%d", fault))
			}
			changedFaults = append(changedFaults, fault)
			anyChanges = true
		}
	}

	// Only publish fault notification if there were changes
	if anyChanges {
		faultChannel := fmt.Sprintf("battery:%d", r.index)
		pipe.Publish(r.ctx, faultChannel, "fault")
	}

	return changedFaults, anyChanges
}
