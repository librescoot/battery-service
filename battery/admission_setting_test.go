package battery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"battery-service/battery/fsm"

	"github.com/redis/go-redis/v9"
)

// Only HGET is needed to exercise setting and voltage gates without hardware.
type admissionRedisHook struct {
	values map[string]string
	err    error
}

func (h *admissionRedisHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, errors.New("unexpected Redis connection")
	}
}

func (h *admissionRedisHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		if cmd.Name() != "hget" || len(args) != 3 {
			return fmt.Errorf("unexpected Redis command: %v", args)
		}
		result := cmd.(*redis.StringCmd)
		if h.err != nil {
			result.SetErr(h.err)
			return h.err
		}
		value, ok := h.values[fmt.Sprint(args[1])+"/"+fmt.Sprint(args[2])]
		if !ok {
			result.SetErr(redis.Nil)
			return redis.Nil
		}
		result.SetVal(value)
		return nil
	}
}

func (h *admissionRedisHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func admissionSettingService(t *testing.T, values map[string]string) (*Service, *admissionRedisHook) {
	t.Helper()
	hook := &admissionRedisHook{values: values}
	client := redis.NewClient(&redis.Options{Addr: "unused"})
	client.AddHook(hook)
	t.Cleanup(func() { _ = client.Close() })
	s := &Service{
		ctx: context.Background(), redis: client,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		config: &ServiceConfig{},
		batteryConfig: &BatteryConfiguration{Readers: []BatteryReaderConfig{
			{Index: 0, Role: BatteryRoleActive, Enabled: true},
			{Index: 1, Role: BatteryRoleInactive, Enabled: true},
		}},
	}
	s.config.MaxVoltageDelta.Store(DefaultMaxVoltageDeltaMV)
	s.readers = []*BatteryReader{
		{index: 0, role: BatteryRoleActive, service: s, ctx: s.ctx, logger: s.logger},
		{index: 1, role: BatteryRoleInactive, service: s, ctx: s.ctx, logger: s.logger},
	}
	return s, hook
}

func TestExperimentalAdmissionRequiresExplicitOptIn(t *testing.T) {
	for _, value := range []string{"", "false", "true", "TRUE", "1", "invalid"} {
		t.Run(value, func(t *testing.T) {
			values := map[string]string{"settings/scooter.dual-battery": "true"}
			if value != "" {
				values["settings/experimental.battery.dual-pack-admission"] = value
			}
			s, _ := admissionSettingService(t, values)
			s.loadAdmissionSetting()
			s.loadDualBatterySetting()
			if (s.admission != nil) != (value == "true") {
				t.Fatalf("admission enabled for value %q: %t", value, s.admission != nil)
			}
			if s.readers[1].role != BatteryRoleActive {
				t.Fatal("dual-battery role was not loaded")
			}
			if s.admission == nil {
				if !s.readers[1].admitCommand(fsm.BMSCmdOn, time.Now()) {
					t.Fatal("disabled admission intercepted a command")
				}
			} else if !s.admission.coordinated {
				t.Fatal("enabled admission did not incorporate dual-battery setting")
			}
		})
	}
}

func TestExperimentalAdmissionReadFailureStaysDisabled(t *testing.T) {
	s, hook := admissionSettingService(t, nil)
	hook.err = errors.New("Redis unavailable")
	s.loadAdmissionSetting()
	if s.admission != nil {
		t.Fatal("read failure enabled admission")
	}
}

func TestNonExperimentalStartupAndRoleChangeVoltageGate(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			s, _ := admissionSettingService(t, map[string]string{
				"settings/experimental.battery.dual-pack-admission": fmt.Sprint(enabled),
				"settings/scooter.dual-battery":                     "true",
				"battery:0/voltage":                                 "55000", "battery:1/voltage": "51000",
			})
			s.loadAdmissionSetting()
			s.loadDualBatterySetting()
			want := BatteryRoleInactive
			if enabled {
				want = BatteryRoleActive
			}
			if s.readers[1].role != want {
				t.Fatalf("startup role = %v, want %v", s.readers[1].role, want)
			}
			s.readers[1].role = BatteryRoleInactive
			s.handleDualBatterySettingChange()
			if s.readers[1].role != want {
				t.Fatalf("reloaded role = %v, want %v", s.readers[1].role, want)
			}
		})
	}
}

func TestNonExperimentalInsertionVoltageGate(t *testing.T) {
	for _, experimental := range []bool{false, true} {
		for _, unconfirmed := range []bool{false, true} {
			t.Run(fmt.Sprintf("experimental=%t/unconfirmed=%t", experimental, unconfirmed), func(t *testing.T) {
				s, _ := admissionSettingService(t, map[string]string{
					"settings/experimental.battery.dual-pack-admission": fmt.Sprint(experimental),
					"battery:0/voltage": "55000",
				})
				s.loadAdmissionSetting()
				r := s.readers[1]
				r.role, r.enabled = BatteryRoleActive, true
				r.data = admissionFrame(51000, BMSStateIdle)
				r.fresh.unconfirmed = unconfirmed
				r.checkActivationVoltageDelta()
				wantBlocked := !experimental && !unconfirmed
				if r.voltageDeltaBlocked != wantBlocked || r.enabled == wantBlocked {
					t.Fatalf("blocked=%t enabled=%t, want blocked=%t", r.voltageDeltaBlocked, r.enabled, wantBlocked)
				}
			})
		}
	}
}

func TestNonExperimentalVoltageDecisionLatchesPerInsertion(t *testing.T) {
	s, hook := admissionSettingService(t, map[string]string{"battery:0/voltage": "53000"})
	r := s.readers[1]
	r.role, r.enabled = BatteryRoleActive, true
	r.data = admissionFrame(53000, BMSStateIdle)
	r.checkActivationVoltageDelta()
	if !r.voltageDeltaChecked || r.voltageDeltaBlocked {
		t.Fatal("compatible insertion was not accepted")
	}
	r.previousData = r.data
	hook.values["battery:0/voltage"] = "55000"
	r.checkActivationVoltageDelta()
	if !r.enabled || r.voltageDeltaBlocked {
		t.Fatal("voltage drift disabled a supply after admission")
	}
	r.previousData.Present = false
	r.checkActivationVoltageDelta()
	if r.enabled || !r.voltageDeltaBlocked {
		t.Fatal("new insertion did not reevaluate voltage")
	}
}
