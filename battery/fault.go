package battery

import (
	"time"
)

type FaultConfig struct {
	Fault             BMSFault
	Description       string
	DebounceTimeSet   time.Duration // Time to confirm fault presence
	DebounceTimeReset time.Duration // Time to confirm fault absence
	IsCritical        bool          // Critical faults cause battery not present
}

var faultConfigs = map[BMSFault]FaultConfig{
	BMSFaultChgTempOverHighProt: {BMSFaultChgTempOverHighProt, "CHG_TEMP_OVER_HIGH_PROT", 0, 0, false},
	BMSFaultChgTempOverLowProt:  {BMSFaultChgTempOverLowProt, "CHG_TEMP_OVER_LOW_PROT", 0, 0, false},
	BMSFaultDsgTempOverHighProt: {BMSFaultDsgTempOverHighProt, "DSG_TEMP_OVER_HIGH_PROT", 0, 0, false},
	BMSFaultDsgTempOverLowProt:  {BMSFaultDsgTempOverLowProt, "DSG_TEMP_OVER_LOW_PROT", 0, 0, false},
	BMSFaultSignalWireBrokeProt: {BMSFaultSignalWireBrokeProt, "SIGNAL_WIRE_BROKE_PROT", 0, 0, false},
	BMSFaultSecondLvlOverTemp:   {BMSFaultSecondLvlOverTemp, "SECOND_LVL_OVER_TEMP", 0, 0, false},
	BMSFaultPackVoltHighProt:    {BMSFaultPackVoltHighProt, "PACK_VOLT_HIGH_PROT", 0, 0, false},
	BMSFaultMosTempOverHighProt: {BMSFaultMosTempOverHighProt, "MOS_TEMP_OVER_HIGH_PROT", 0, 0, false},
	BMSFaultCellVoltHighProt:    {BMSFaultCellVoltHighProt, "CELL_VOLT_HIGH_PROT", 0, 0, false},
	BMSFaultPackVoltLowProt:     {BMSFaultPackVoltLowProt, "PACK_VOLT_LOW_PROT", 0, 0, false},
	BMSFaultCellVoltLowProt:     {BMSFaultCellVoltLowProt, "CELL_VOLT_LOW_PROT", 0, 0, false},
	BMSFaultCrgOverCurrentProt:  {BMSFaultCrgOverCurrentProt, "CRG_OVER_CURRENT_PROT", 0, 0, false},
	BMSFaultDsgOverCurrentProt:  {BMSFaultDsgOverCurrentProt, "DSG_OVER_CURRENT_PROT", 0, 0, false},
	BMSFaultShortCircuitProt:    {BMSFaultShortCircuitProt, "SHORT_CIRCUIT_PROT", 0, 0, false},
	BMSFaultReserved:            {BMSFaultReserved, "RESERVED", 0, 0, false},
	BMSFaultReserved2:           {BMSFaultReserved2, "RESERVED2", 0, 0, false},

	BMSFaultBMSNotFollowingCmd: {BMSFaultBMSNotFollowingCmd, "BMS_NOT_FOLLOWING_CMD", 5 * time.Second, 10 * time.Second, false},
	BMSFaultBMSZeroData:        {BMSFaultBMSZeroData, "BMS_ZERO_DATA", 0, 0, true},
	BMSFaultBMSCommsError:      {BMSFaultBMSCommsError, "BMS_COMMS_ERROR", 5 * time.Second, 10 * time.Second, true},
	BMSFaultNFCReaderError:     {BMSFaultNFCReaderError, "NFC_READER_ERROR", 30 * time.Second, 0, true},
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
		r.service.logger.Printf("Battery %d: Unknown fault %d", r.index, fault)
		return
	}

	state, exists := r.faultStates[fault]
	if !exists {
		state = &FaultState{}
		r.faultStates[fault] = state
	}

	if state.Present == present {
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
	r.service.logger.Printf("Battery %d: Fault %s (%d) activated", r.index, config.Description, fault)

	if config.IsCritical {
		r.clearLesserFaults(fault, false)
		r.sendNotPresent()
	}

	r.reportFault(fault, config, true)
}

func (r *BatteryReader) deactivateFault(fault BMSFault, config FaultConfig) {
	r.service.logger.Printf("Battery %d: Fault %s (%d) cleared", r.index, config.Description, fault)

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
	r.data.Present = false
	r.sendStatusUpdate()
	r.service.logger.Printf("Battery %d: Reported as not present due to critical fault", r.index)
}

func (r *BatteryReader) reportFault(fault BMSFault, config FaultConfig, present bool) {
}

func (r *BatteryReader) hasCriticalFaults() bool {
	for fault, state := range r.faultStates {
		config, exists := faultConfigs[fault]
		if exists && config.IsCritical && state.Present {
			return true
		}
	}
	return false
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
