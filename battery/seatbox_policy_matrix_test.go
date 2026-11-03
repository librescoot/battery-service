package battery

import (
	"context"
	"fmt"
	"testing"

	"battery-service/battery/fsm"
)

func TestSeatboxPolicyMatrix(t *testing.T) {
	states := append([]VehicleState{VehicleStateReadyToDrive, VehicleStateOther, ""}, nonDriveTestStates...)
	sequences := []struct {
		name   string
		closed []bool
	}{
		{"open", []bool{false}},
		{"closed", []bool{true}},
		{"open-close", []bool{false, true}},
		{"bounce", []bool{false, true, false, true, false}},
		{"duplicate-open", []bool{false, false}},
		{"duplicate-close", []bool{true, true}},
	}
	for mode := 0; mode < 4; mode++ {
		for _, role := range []BatteryRole{BatteryRoleActive, BatteryRoleInactive} {
			for slot := 0; slot < 2; slot++ {
				for _, vehicle := range states {
					for _, pack := range []string{"active", "idle", "asleep", "unknown", "empty"} {
						for _, sequence := range sequences {
							t.Run(fmt.Sprintf("manual=%t/aux=%t/%s/slot%d/%s/%s/%s", mode&1 != 0, mode&2 != 0, role, slot, vehicle, pack, sequence.name), func(t *testing.T) {
								r, _ := newDriveLatchReader(slot)
								r.role = role
								r.enabled = role == BatteryRoleActive
								r.vehicleState = vehicle
								r.service.config.KeepActiveOnSeatboxOpen.Store(mode&1 != 0)
								r.service.config.AuxLowKeepActive.Store(mode&2 != 0)
								switch pack {
								case "idle":
									r.data.State = BMSStateIdle
								case "asleep":
									r.data.State = BMSStateAsleep
								case "unknown":
									r.data.State = BMSStateUnknown
								case "empty":
									r.data.Charge, r.data.LowSOC = 0, true
								}
								for step, closed := range sequence.closed {
									r.handleSeatboxLockChange(closed)
									wantClosed := closed || vehicle == VehicleStateReadyToDrive
									wantKeep := role == BatteryRoleActive && mode != 0
									wantEnabled := role == BatteryRoleActive && (mode != 0 || wantClosed)
									if r.seatboxLockClosed != closed || r.GetSeatboxLockClosed() != wantClosed || r.GetEnabled() != wantEnabled || r.ShouldKeepActiveOnSeatboxOpen() != wantKeep || r.ShouldSendOn() != (wantEnabled && pack != "empty") {
										t.Fatalf("step %d closed=%t: raw=%t latch=%t enabled=%t keep=%t ON=%t", step, closed, r.seatboxLockClosed, r.GetSeatboxLockClosed(), r.GetEnabled(), r.ShouldKeepActiveOnSeatboxOpen(), r.ShouldSendOn())
									}
								}
							})
						}
					}
				}
			}
		}
	}
}

func waitKeepActivePolicyState(t *testing.T, r *BatteryReader) {
	t.Helper()
	state := fsm.StateWaitUpdate
	if !r.GetEnabled() {
		state = fsm.StateSendOpened
	}
	waitDriveLatchState(t, r, state)
}

func TestKeepActiveSettingTransitionMatrix(t *testing.T) {
	testKeepActiveSettingTransitions(t, false)
}

func TestKeepActiveSettingCommandMatrix(t *testing.T) {
	testKeepActiveSettingTransitions(t, true)
}

func testKeepActiveSettingTransitions(t *testing.T, runFSM bool) {
	t.Helper()
	for from := 0; from < 4; from++ {
		for to := 0; to < 4; to++ {
			for _, role := range []BatteryRole{BatteryRoleActive, BatteryRoleInactive} {
				if runFSM && role != BatteryRoleActive {
					continue
				}
				for _, closed := range []bool{false, true} {
					for slot := 0; slot < 2; slot++ {
						for _, vehicle := range []VehicleState{VehicleStateStandby, VehicleStateParked, VehicleStateReadyToDrive} {
							t.Run(fmt.Sprintf("%d-to-%d/%s/closed=%t/slot%d/%s", from, to, role, closed, slot, vehicle), func(t *testing.T) {
								if runFSM {
									t.Parallel()
								}
								values := map[string]string{
									"settings/scooter.battery-keep-active-on-seatbox-open": fmt.Sprint(to&1 != 0),
									"aux-battery/voltage": "12500",
								}
								if to&2 != 0 {
									values["aux-battery/voltage"] = "11000"
								}
								s, _ := admissionSettingService(t, values)
								r, actions := newDriveLatchReader(slot)
								r.role, r.enabled, r.vehicleState, r.service = role, role == BatteryRoleActive, vehicle, s
								s.readers = make([]*BatteryReader, 2)
								s.readers[slot] = r
								s.config.KeepActiveOnSeatboxOpen.Store(from&1 != 0)
								s.config.AuxLowKeepActive.Store(from&2 != 0)
								s.config.AuxLowKeepActiveEvaluated.Store(true)
								s.config.AuxLowKeepActiveEnterMv.Store(auxLowKeepActiveDefaultEnterMv)
								s.config.AuxLowKeepActiveExitMv.Store(auxLowKeepActiveDefaultExitMv)
								r.handleSeatboxLockChange(closed)
								select {
								case <-r.restartChan:
								default:
								}
								if runFSM {
									ctx, cancel := context.WithCancel(context.Background())
									t.Cleanup(cancel)
									if err := r.fsm.Start(ctx); err != nil {
										t.Fatal(err)
									}
									r.fsm.SendEvent(fsm.EvInitComplete)
									waitDriveLatchState(t, r, fsm.StateWaitArrival)
									r.fsm.SendEvent(fsm.EvTagArrived)
									waitKeepActivePolicyState(t, r)
								}
								before := len(actions.snapshot())
								wasEnabled := r.GetEnabled()
								s.reloadBoolSetting(s.keepActiveOnSeatboxOpenSettingSpec())
								s.recomputeAuxLowKeepActive(true)
								select {
								case <-r.keepActiveChan:
									r.handleKeepActiveChange()
								default:
								}
								restarted := false
								select {
								case <-r.restartChan:
									restarted = true
									r.handleRestart()
								default:
								}
								want := role == BatteryRoleActive && (to != 0 || closed || vehicle == VehicleStateReadyToDrive)
								if r.GetEnabled() != want {
									t.Fatalf("enabled=%t, want %t after settings change", r.GetEnabled(), want)
								}
								if runFSM {
									if restarted != (wasEnabled != want) {
										t.Fatalf("restart=%t for enabled %t to %t", restarted, wasEnabled, want)
									}
									if restarted {
										if want {
											waitDriveLatchState(t, r, fsm.StateSendClosed)
										} else {
											waitDriveLatchState(t, r, fsm.StateSendOff)
										}
										waitKeepActivePolicyState(t, r)
									}
									commands := actions.snapshot()[before:]
									if !restarted && len(commands) != 0 {
										t.Fatalf("unchanged policy interrupted commands: %v", commands)
									}
									if restarted {
										wantLast := fsm.BMSCmdSeatboxOpened
										if want {
											wantLast = fsm.BMSCmdOn
											for _, cmd := range commands {
												if cmd == fsm.BMSCmdOff || cmd == fsm.BMSCmdSeatboxOpened {
													t.Fatalf("keep-active interrupted running supply: %v", commands)
												}
											}
										}
										if len(commands) != 2 || commands[len(commands)-1] != wantLast {
											t.Fatalf("commands after policy change = %v, want final %s", commands, wantLast)
										}
									}
								}
							})
						}
					}
				}
			}
		}
	}
}
