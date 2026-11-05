package battery

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"testing"

	"battery-service/battery/fsm"

	"github.com/redis/go-redis/v9"
)

type powerFaultHook struct {
	absenceRedisHook
	events []map[string]string
	active bool
}

func (h *powerFaultHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(_ context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		switch cmd.Name() {
		case "sadd":
			if fmt.Sprint(args[2]) == "36" {
				h.active = true
			}
		case "srem":
			if fmt.Sprint(args[2]) == "36" {
				h.active = false
			}
		case "xadd":
			event := map[string]string{}
			for i, arg := range args {
				if fmt.Sprint(arg) == "*" {
					for j := i + 1; j+1 < len(args); j += 2 {
						event[fmt.Sprint(args[j])] = fmt.Sprint(args[j+1])
					}
					break
				}
			}
			h.events = append(h.events, event)
		}
		return nil
	}
}

func newPowerFaultReader(t *testing.T, index int) (*BatteryReader, *powerFaultHook) {
	t.Helper()
	r := newFaultTestReader()
	r.ctx = context.Background()
	h := &powerFaultHook{}
	client := redis.NewClient(&redis.Options{Addr: "unused", MaxRetries: -1})
	client.AddHook(h)
	t.Cleanup(func() { _ = client.Close() })
	t.Cleanup(r.cleanupFaultManagement)
	t.Cleanup(func() { r.fresh.stopTimer() })
	r.service = &Service{redis: client}
	r.index, r.role, r.enabled = index, BatteryRoleActive, true
	r.vehicleState = VehicleStateReadyToDrive
	r.data = liveFrame
	r.data.SampleValid = true
	return r, h
}

func TestUnexpectedPowerOffFaultLifecycle(t *testing.T) {
	for _, index := range []int{0, 1} {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			r, h := newPowerFaultReader(t, index)
			r.observePowerState()
			r.data.State = BMSStateIdle
			r.data.Current = 29496
			r.data.FaultCode = 0
			r.observePowerState()
			if !h.active || !r.data.Present {
				t.Fatal("unexpected power loss not reported or pack hidden")
			}
			if len(h.events) != 1 {
				t.Fatalf("events=%v", h.events)
			}
			for key, want := range map[string]string{
				"group": fmt.Sprintf("battery:%d", index), "code": "36", "description": "Battery switched off unexpectedly",
				"state": "idle", "current-ma": "29496", "bms-fault-code": "0", "serial-number": liveFrame.SerialNumber,
				"voltage-mv": "50636", "charge": "37", "vehicle-state": "ready-to-drive",
			} {
				if got := h.events[0][key]; got != want {
					t.Errorf("%s=%q, want %q", key, got, want)
				}
			}
			for i := 0; i < 10; i++ {
				r.observePowerState()
			}
			if len(h.events) != 1 {
				t.Fatal("repeated idle duplicated fault history")
			}
			r.data.State = BMSStateActive
			r.observePowerState()
			if h.active || len(h.events) != 2 || h.events[1]["code"] != "-36" {
				t.Fatal("recovery did not clear fault", h.events)
			}
			r.data.State = BMSStateIdle
			r.observePowerState()
			if len(h.events) != 3 {
				t.Fatal("second episode missing")
			}
			r.handleDeparture("test")
			if h.active || len(h.events) != 4 || r.powerSample != (BMSData{}) {
				t.Fatal("removal failed to clear", h.events)
			}
		})
	}
}

func TestUnexpectedPowerOffRequiresExpectedSupply(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*BatteryReader)
	}{
		{"startup idle", func(r *BatteryReader) { r.powerSample = BMSData{} }},
		{"disabled", func(r *BatteryReader) { r.enabled = false }},
		{"storage role", func(r *BatteryReader) { r.role = BatteryRoleInactive }},
		{"empty pack", func(r *BatteryReader) { r.data.LowSOC = true; r.data.Charge = 0 }},
		{"different pack", func(r *BatteryReader) { r.data.SerialNumber = "different" }},
		{"invalid acquisition", func(r *BatteryReader) { r.data.SampleValid = false }},
		{"not present", func(r *BatteryReader) { r.data.Present = false }},
		{"unknown state", func(r *BatteryReader) { r.data.State = BMSStateUnknown }},
		{"tag change", func(r *BatteryReader) { r.noteTagPresent([]byte{4, 3, 2, 1}) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, h := newPowerFaultReader(t, 0)
			r.observePowerState()
			r.data.State = BMSStateIdle
			tc.setup(r)
			r.observePowerState()
			if h.active || len(h.events) != 0 {
				t.Fatal("false power-loss event", h.events)
			}
		})
	}
}

func TestUnexpectedPowerOffAsleepAndHardwareFault(t *testing.T) {
	r, h := newPowerFaultReader(t, 0)
	r.observePowerState()
	r.data.State = BMSStateAsleep
	r.data.FaultCode = 1 << 12
	r.observePowerState()
	if !h.active || h.events[0]["bms-fault-code"] != "4096" {
		t.Fatal("missing context", h.events)
	}
}

type powerControllerStub struct {
	discoveryControllerStub
	state    BMSState
	writeErr error
}

func (s *powerControllerStub) ReadBinary(address uint16) ([]byte, error) {
	data := make([]byte, 16)
	switch address {
	case 0x0300:
		binary.LittleEndian.PutUint16(data, 50000)
		data[4], data[5] = 2, 28
		binary.LittleEndian.PutUint16(data[6:], 16000)
		binary.LittleEndian.PutUint16(data[8:], 33000)
		data[12], data[13] = 20, 20
	case 0x0310:
		binary.LittleEndian.PutUint32(data, uint32(s.state))
		copy(data[4:], "T-UNU2220509")
	case 0x0320:
		copy(data, "0007")
		data[14], data[15] = 20, 20
	}
	return data, nil
}
func (s *powerControllerStub) WriteBinary(uint16, []byte) error { return s.writeErr }
func (s *powerControllerStub) StopDiscovery() error             { return nil }

func TestUnexpectedPowerOffThroughNFCReadAndWrite(t *testing.T) {
	for _, command := range []fsm.BMSCommand{fsm.BMSCmdOn, fsm.BMSCmdOff} {
		for _, failed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/failed=%t", command, failed), func(t *testing.T) {
				r, h := newPowerFaultReader(t, 0)
				controller := &powerControllerStub{state: BMSStateActive}
				r.hal, r.tagsDiscovered = controller, true
				if !r.readStatus() {
					t.Fatal("active read failed")
				}
				if failed {
					controller.writeErr = errors.New("write failed")
				}
				local, peer := net.Pipe()
				t.Cleanup(func() { _ = peer.Close() })
				r.suspendInhibitor = &SuspendInhibitor{conn: local}
				err := r.writeCommand(command, false)
				if (err != nil) != failed {
					t.Fatal("unexpected write result", err)
				}
				r.tagsDiscovered = true
				controller.state = BMSStateIdle
				if !r.readStatus() {
					t.Fatal("idle read failed")
				}
				wantFault := command != fsm.BMSCmdOff || failed
				if h.active != wantFault {
					t.Fatalf("fault=%t, want %t", h.active, wantFault)
				}
			})
		}
	}
}

func TestUnexpectedPowerOffDoesNotArmWhileDisabled(t *testing.T) {
	r, h := newPowerFaultReader(t, 0)
	r.enabled = false
	r.observePowerState()
	r.enabled = true
	r.data.State = BMSStateIdle
	r.observePowerState()
	if h.active {
		t.Fatal("startup activation mistaken for power loss")
	}
}
