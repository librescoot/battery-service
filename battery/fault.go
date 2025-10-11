package battery

import (
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type FaultConfig struct {
	Fault             BMSFault
	Description       string
	DebounceTimeSet   time.Duration // Time to confirm fault presence
	DebounceTimeReset time.Duration // Time to confirm fault absence
	IsCritical        bool          // Critical faults cause battery not present
}

var faultConfigs = map[BMSFault]FaultConfig{
	BMSFaultChgTempOverHighProt: {BMSFaultChgTempOverHighProt, "High temperature during charging", 0, 0, false},
	BMSFaultChgTempOverLowProt:  {BMSFaultChgTempOverLowProt, "Low temperature during charging", 0, 0, false},
	BMSFaultDsgTempOverHighProt: {BMSFaultDsgTempOverHighProt, "High temperature during discharge", 0, 0, false},
	BMSFaultDsgTempOverLowProt:  {BMSFaultDsgTempOverLowProt, "Low temperature during discharge", 0, 0, false},
	BMSFaultSignalWireBrokeProt: {BMSFaultSignalWireBrokeProt, "Signal wire disconnected", 0, 0, false},
	BMSFaultSecondLvlOverTemp:   {BMSFaultSecondLvlOverTemp, "Critical temperature level", 0, 0, false},
	BMSFaultPackVoltHighProt:    {BMSFaultPackVoltHighProt, "Battery pack overvoltage", 0, 0, false},
	BMSFaultMosTempOverHighProt: {BMSFaultMosTempOverHighProt, "Power transistor overheating", 0, 0, false},
	BMSFaultCellVoltHighProt:    {BMSFaultCellVoltHighProt, "Cell overvoltage", 0, 0, false},
	BMSFaultPackVoltLowProt:     {BMSFaultPackVoltLowProt, "Battery pack undervoltage", 0, 0, false},
	BMSFaultCellVoltLowProt:     {BMSFaultCellVoltLowProt, "Cell undervoltage", 0, 0, false},
	BMSFaultCrgOverCurrentProt:  {BMSFaultCrgOverCurrentProt, "Charging overcurrent", 0, 0, false},
	BMSFaultDsgOverCurrentProt:  {BMSFaultDsgOverCurrentProt, "Discharge overcurrent", 0, 0, false},
	BMSFaultShortCircuitProt:    {BMSFaultShortCircuitProt, "Short circuit detected", 0, 0, false},
	BMSFaultReserved:            {BMSFaultReserved, "Reserved fault 1", 0, 0, false},
	BMSFaultReserved2:           {BMSFaultReserved2, "Reserved fault 2", 0, 0, false},

	BMSFaultBMSNotFollowingCmd: {BMSFaultBMSNotFollowingCmd, "Battery not responding to commands", 5 * time.Second, 10 * time.Second, false},
	BMSFaultBMSZeroData:        {BMSFaultBMSZeroData, "Battery data unavailable", 0, 0, true},
	BMSFaultBMSCommsError:      {BMSFaultBMSCommsError, "Battery communication failed", 5 * time.Second, 10 * time.Second, true},
	BMSFaultNFCReaderError:     {BMSFaultNFCReaderError, "NFC reader malfunction", 30 * time.Second, 0, true},
}

func (r *BatteryReader) initializeFaultManagement() {
	r.faultStates = make(map[BMSFault]*FaultState)
	for fault := range faultConfigs {
		r.faultStates[fault] = &FaultState{}
	}
}

func (r *BatteryReader) setFault(fault BMSFault, present bool) {
	config, exists := faultConfigs[fault]
	if !exists {
		r.service.logger.Warnf("Battery %d: Unknown fault %d", r.index, fault)
		return
	}

	state, exists := r.faultStates[fault]
	if !exists {
		state = &FaultState{}
		r.faultStates[fault] = state
	}

	if state.Present == present && !state.PendingSet && !state.PendingReset {
		return
	}

	if state.SetTimer != nil {
		state.SetTimer.Stop()
		state.SetTimer = nil
		state.PendingSet = false
	}
	if state.ResetTimer != nil {
		state.ResetTimer.Stop()
		state.ResetTimer = nil
		state.PendingReset = false
	}

	if present {
		if config.DebounceTimeSet == 0 {
			r.activateFault(fault, config)
			state.Present = true
		} else {
			state.PendingSet = true
			state.SetTimer = time.AfterFunc(config.DebounceTimeSet, func() {
				r.activateFault(fault, config)
				state.Present = true
				state.PendingSet = false
				state.SetTimer = nil
			})
		}
	} else {
		if config.DebounceTimeReset == 0 {
			r.deactivateFault(fault, config)
			state.Present = false
		} else {
			state.PendingReset = true
			state.ResetTimer = time.AfterFunc(config.DebounceTimeReset, func() {
				r.deactivateFault(fault, config)
				state.Present = false
				state.PendingReset = false
				state.ResetTimer = nil
			})
		}
	}
}

func (r *BatteryReader) activateFault(fault BMSFault, config FaultConfig) {
	r.service.logger.Warnf("Battery %d: Fault %s (%d) activated", r.index, config.Description, fault)

	if config.IsCritical {
		r.clearLesserFaults(fault, false)
		r.sendNotPresent()
	}

	r.reportFault(fault, config, true)
}

func (r *BatteryReader) deactivateFault(fault BMSFault, config FaultConfig) {
	r.service.logger.Infof("Battery %d: Fault %s (%d) cleared", r.index, config.Description, fault)

	r.reportFault(fault, config, false)
}

func (r *BatteryReader) clearLesserFaults(referenceFault BMSFault, includeReference bool) {
	for fault, state := range r.faultStates {
		if fault < referenceFault || (includeReference && fault == referenceFault) {
			if state.Present {
				r.setFault(fault, false)
			}
		}
	}
}

func (r *BatteryReader) sendNotPresent() {
	r.data = BMSData{}
	r.sendStatusUpdate()
	r.service.logger.Warnf("Battery %d: Reported as not present due to critical fault", r.index)
}

func (r *BatteryReader) reportFault(fault BMSFault, config FaultConfig, present bool) {
	batteryName := fmt.Sprintf("battery:%d", r.index)
	faultSetKey := fmt.Sprintf("battery:%d:fault", r.index)

	if present {
		if err := r.redis.SAdd(r.ctx, faultSetKey, fmt.Sprintf("%d", fault)).Err(); err != nil {
			r.service.logger.Warnf("Battery %d: Failed to add fault to set: %v", r.index, err)
		}

		if err := r.redis.XAdd(r.ctx, &redis.XAddArgs{
			Stream: "events:faults",
			MaxLen: 1000,
			Values: map[string]interface{}{
				"group":       batteryName,
				"code":        fmt.Sprintf("%d", fault),
				"description": config.Description,
			},
		}).Err(); err != nil {
			r.service.logger.Warnf("Battery %d: Failed to add fault event to stream: %v", r.index, err)
		}

		if err := r.redis.Publish(r.ctx, batteryName, "fault").Err(); err != nil {
			r.service.logger.Warnf("Battery %d: Failed to publish fault notification: %v", r.index, err)
		}
	} else {
		if err := r.redis.SRem(r.ctx, faultSetKey, fmt.Sprintf("%d", fault)).Err(); err != nil {
			r.service.logger.Warnf("Battery %d: Failed to remove fault from set: %v", r.index, err)
		}

		if err := r.redis.XAdd(r.ctx, &redis.XAddArgs{
			Stream: "events:faults",
			MaxLen: 1000,
			Values: map[string]interface{}{
				"group": batteryName,
				"code":  fmt.Sprintf("-%d", fault),
			},
		}).Err(); err != nil {
			r.service.logger.Warnf("Battery %d: Failed to add fault clear event to stream: %v", r.index, err)
		}

		if err := r.redis.Publish(r.ctx, batteryName, "fault").Err(); err != nil {
			r.service.logger.Warnf("Battery %d: Failed to publish fault clear notification: %v", r.index, err)
		}
	}
}

func (r *BatteryReader) parseHardwareFaults(faultCode uint) {
	for bit := 0; bit < 16; bit++ {
		fault := BMSFault(bit + 1)
		present := (faultCode & (1 << bit)) != 0
		r.setFault(fault, present)
	}
}

func (r *BatteryReader) updateFaultsFromBatteryData() {
	r.parseHardwareFaults(r.data.FaultCode)

	isZeroData := r.data.EmptyOr0Data > 0
	r.setFault(BMSFaultBMSZeroData, isZeroData)
}

func (r *BatteryReader) cleanupFaultManagement() {
	for _, state := range r.faultStates {
		if state.SetTimer != nil {
			state.SetTimer.Stop()
		}
		if state.ResetTimer != nil {
			state.ResetTimer.Stop()
		}
	}
}
