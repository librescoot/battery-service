package battery

import "testing"

// The BMS latches second-level over-temperature in EEPROM and drives the fuse
// trigger from it, so a pack reporting fault code 6 has fired its fuse and is
// permanently out of service. It must be published as not present, not as a
// present pack with a non-critical fault.
func TestSecondLvlOverTempIsCritical(t *testing.T) {
	config, ok := faultConfigs[BMSFaultSecondLvlOverTemp]
	if !ok {
		t.Fatal("no fault config for BMSFaultSecondLvlOverTemp")
	}
	if !config.IsCritical {
		t.Error("BMSFaultSecondLvlOverTemp must be marked critical")
	}
	if !BMSFaultSecondLvlOverTemp.IsCritical() {
		t.Error("BMSFaultSecondLvlOverTemp.IsCritical() must agree with the config")
	}
}

func TestIsCriticalFollowsConfig(t *testing.T) {
	for fault, config := range faultConfigs {
		if got := fault.IsCritical(); got != config.IsCritical {
			t.Errorf("fault %d: IsCritical()=%t, config=%t", fault, got, config.IsCritical)
		}
	}

	if BMSFaultNone.IsCritical() {
		t.Error("BMSFaultNone must not be critical")
	}
	if BMSFaultCellVoltLowProt.IsCritical() {
		t.Error("BMSFaultCellVoltLowProt must remain non-critical")
	}
	if !BMSFaultBMSZeroData.IsCritical() {
		t.Error("BMSFaultBMSZeroData must remain critical")
	}
}
