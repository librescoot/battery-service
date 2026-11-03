package battery

import (
	"errors"
	"fmt"
	"strconv"
	"testing"
)

func TestAuxKeepActiveVoltageMatrix(t *testing.T) {
	for _, thresholds := range [][2]uint64{{11500, 12000}, {12000, 12500}} {
		for _, manual := range []bool{false, true} {
			for _, was := range []bool{false, true} {
				for _, history := range []bool{false, true} {
					for _, voltage := range []string{"0", "11499", "11500", "11700", "11999", "12000", "12500", "missing", "invalid", "-1", "redis-error"} {
						t.Run(fmt.Sprintf("%d-%d/manual=%t/was=%t/history=%t/%s", thresholds[0], thresholds[1], manual, was, history, voltage), func(t *testing.T) {
							values := map[string]string{"aux-battery/voltage": voltage}
							if voltage == "missing" {
								delete(values, "aux-battery/voltage")
							}
							s, hook := admissionSettingService(t, values)
							if voltage == "redis-error" {
								hook.err = errors.New("Redis unavailable")
							}
							r, _ := newDriveLatchReader(0)
							r.service = s
							s.readers = []*BatteryReader{r}
							s.config.KeepActiveOnSeatboxOpen.Store(manual)
							s.config.AuxLowKeepActive.Store(was)
							s.config.AuxLowKeepActiveEvaluated.Store(history)
							s.config.AuxLowKeepActiveEnterMv.Store(thresholds[0])
							s.config.AuxLowKeepActiveExitMv.Store(thresholds[1])
							r.handleSeatboxLockChange(false)
							s.recomputeAuxLowKeepActive(true)
							select {
							case <-r.keepActiveChan:
								r.handleKeepActiveChange()
							default:
							}
							wantAux := was
							if mv, err := strconv.ParseUint(voltage, 10, 64); err == nil {
								threshold := thresholds[0]
								if was || !history {
									threshold = thresholds[1]
								}
								wantAux = mv < threshold
							}
							if s.config.AuxLowKeepActive.Load() != wantAux || r.GetEnabled() != (manual || wantAux) {
								t.Fatalf("aux=%t enabled=%t, want aux=%t enabled=%t", s.config.AuxLowKeepActive.Load(), r.GetEnabled(), wantAux, manual || wantAux)
							}
						})
					}
				}
			}
		}
	}
}
