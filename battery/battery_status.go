package battery

import (
	"fmt"

	"github.com/redis/go-redis/v9"
)

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
		"manufacturing-date": r.data.ManuDate,
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

	// Publish notifications only for changed fields
	if effectivePresent != previousEffectivePresent {
		pipe.Publish(r.ctx, channel, "present")
	}
	if r.data.State != r.previousData.State {
		pipe.Publish(r.ctx, channel, "state")
	}
	if r.data.Charge != r.previousData.Charge {
		pipe.Publish(r.ctx, channel, "charge")
	}
	if r.data.TemperatureState != r.previousData.TemperatureState {
		pipe.Publish(r.ctx, channel, "temperature-state")
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
