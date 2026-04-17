package fsm

import (
	"context"
	"log/slog"
	"time"

	"github.com/librescoot/librefsm"
)

// Timing constants
const (
	timeCmd           = 400 * time.Millisecond
	timeReinit        = 2 * time.Second
	timeDeparture     = 500 * time.Millisecond
	timeCheckReader   = 10 * time.Second
	timeCheckPresence = 5 * time.Second

	// Poll backoff bounds for check_presence recovery: start short (tag
	// mirror typically clears within tens of ms) and cap well below
	// timeCheckPresence so we get multiple attempts in the window.
	checkPresencePollMin = 100 * time.Millisecond
	checkPresencePollMax = 800 * time.Millisecond
	// Note: Heartbeat interval is dynamic - see GetHeartbeatInterval()
	// Active batteries: 40s (configurable via --heartbeat-timeout)
	// Inactive batteries: 30min (configurable via --off-update-time)
	timeMaintPollInterval = 30 * time.Minute
)

// maxConsecutiveReadFailures is the number of back-to-back status read
// failures tolerated in the heartbeat cycle before falling back to a full
// presence-check restart. A single arbiter-busy NACK is absorbed so the
// heartbeat can resend ON on the next cycle and keep the BMS active.
const maxConsecutiveReadFailures = 3

// BatteryActions is the interface for battery hardware operations
type BatteryActions interface {
	TakeInhibitor()
	ReleaseInhibitor()
	StartDiscovery() error
	StopDiscovery()
	SelectTag()
	PollForTagArrival()
	Initialize() error
	Deinitialize()
	ReadStatus() error
	SendCheckPresenceReady()
	WriteCommand(cmd BMSCommand)
	GetEnabled() bool
	GetSeatboxLockClosed() bool
	GetVehicleActive() bool
	CheckStateCorrect() bool
	GetRemainingCmdTime() time.Duration
	GetOpenedTime() time.Duration
	GetInsertedTime() time.Duration
	GetHeartbeatInterval() time.Duration
	IsInactive() bool
	ZeroRetryCounters()
	StopHeartbeatTimer()
	ShouldKeepActiveOnSeatboxOpen() bool
	StartHeartbeatTimer()
	ClearHeartbeatTimer()
	StopTimerIfBatteryEmpty()
	IsRoleInactive() bool
}

// fsmData holds the FSM-specific state
type fsmData struct {
	actions                 BatteryActions
	log                     *slog.Logger
	ctx                     context.Context // FSM context for cancellation
	justInserted            bool
	justOpened              bool
	latchedSeatboxClosed    bool
	tagAbsentCancel         context.CancelFunc
	checkPresenceCancel     context.CancelFunc
	consecutiveReadFailures int
}

// StateMachine wraps librefsm.Machine to provide the same interface as before
type StateMachine struct {
	machine *librefsm.Machine
	data    *fsmData
	log     *slog.Logger
}

// New creates a new StateMachine
func New(actions BatteryActions, log *slog.Logger) *StateMachine {
	data := &fsmData{
		actions: actions,
		log:     log,
	}

	def := buildDefinition(data)

	machine, err := def.Build(
		librefsm.WithData(data),
		librefsm.WithLogger(log),
		librefsm.WithStateChangeCallback(func(from, to librefsm.StateID) {
			log.Info("State Transition", "from", from, "to", to)
		}),
	)
	if err != nil {
		log.Error("Failed to build FSM", "error", err)
		return nil
	}

	return &StateMachine{
		machine: machine,
		data:    data,
		log:     log,
	}
}

// Run starts the FSM event loop
func (sm *StateMachine) Run(ctx context.Context) {
	sm.log.Info("state machine started", "initial_state", StateInit)

	// Store context in fsmData so state handlers can use it for cancellation
	sm.data.ctx = ctx

	if err := sm.machine.Start(ctx); err != nil {
		sm.log.Error("Failed to start FSM", "error", err)
		return
	}

	// Block until context is done
	<-ctx.Done()
	sm.machine.Stop()
	sm.log.Info("state machine stopping")
}

// SendEvent sends an event to the FSM
func (sm *StateMachine) SendEvent(id librefsm.EventID) {
	sm.machine.Send(librefsm.Event{ID: id})
}

// State returns the current state
func (sm *StateMachine) State() State {
	return sm.machine.CurrentState()
}

// IsInState checks if the FSM is in the given state or any of its children
func (sm *StateMachine) IsInState(id State) bool {
	return sm.machine.IsInState(id)
}

// readStatusAction reads battery status before a timeout event fires.
// A single transient failure (e.g. NTAG arbiter busy) is absorbed: the
// natural timer transition continues with last-known state, and the next
// heartbeat cycle resends ON and retries the read. Only after
// maxConsecutiveReadFailures back-to-back misses do we fall back to
// EvRestart, which re-enters cond_check_presence for a full recovery.
func readStatusAction(c *librefsm.Context) error {
	d := c.Data.(*fsmData)
	err := d.actions.ReadStatus()
	if err == nil {
		d.consecutiveReadFailures = 0
		return nil
	}
	d.consecutiveReadFailures++
	if d.consecutiveReadFailures >= maxConsecutiveReadFailures {
		d.log.Warn("status read failed repeatedly, restarting",
			"error", err, "failures", d.consecutiveReadFailures)
		d.consecutiveReadFailures = 0
		c.Send(librefsm.Event{ID: EvRestart})
		return nil
	}
	d.log.Debug("status read failed, will retry on next heartbeat",
		"error", err, "failures", d.consecutiveReadFailures)
	return nil
}

// markJustInsertedAction sets justInserted on any EvTagArrived transition
// into StateTagPresent, so StateCondJustInserted reflects the real insertion
// state and not just a sticky FSM-lifetime flag from first startup.
func markJustInsertedAction(c *librefsm.Context) error {
	d := c.Data.(*fsmData)
	d.justInserted = true
	return nil
}

// buildDefinition creates the librefsm definition for the battery FSM
func buildDefinition(data *fsmData) *librefsm.Definition {
	return librefsm.NewDefinition().
		// ================================================================
		// Root-level states
		// ================================================================

		// Init state - just sends init complete event
		State(StateInit,
			librefsm.WithOnEnter(func(c *librefsm.Context) error {
				// Don't auto-send init complete - reader.go controls this
				return nil
			}),
		).

		// NFC Reader Off - deinitialize and wait for reinit
		State(StateNFCReaderOff,
			librefsm.WithTimeout(timeReinit, EvReinit),
			librefsm.WithOnEnter(func(c *librefsm.Context) error {
				d := c.Data.(*fsmData)
				d.actions.Deinitialize()
				return nil
			}),
		).

		// NFC Reader On - parent for discovery and tag present states
		State(StateNFCReaderOn,
			librefsm.WithDefaultChild(StateDiscoverTag),
			librefsm.WithOnEnter(func(c *librefsm.Context) error {
				d := c.Data.(*fsmData)
				if err := d.actions.Initialize(); err != nil {
					d.log.Error("failed to initialize NFC", "error", err)
					c.Send(librefsm.Event{ID: EvReinit})
				}
				return nil
			}),
		).

		// ================================================================
		// Discovery states (children of NFCReaderOn)
		// ================================================================

		// Discover Tag - parent for wait arrival and tag absent
		State(StateDiscoverTag,
			librefsm.WithParent(StateNFCReaderOn),
			librefsm.WithDefaultChild(StateWaitArrival),
		).

		// Wait Arrival - polling for tag with departure timeout
		State(StateWaitArrival,
			librefsm.WithParent(StateDiscoverTag),
			librefsm.WithTimeout(timeDeparture, EvDepartureTimeout),
			librefsm.WithOnEnter(func(c *librefsm.Context) error {
				d := c.Data.(*fsmData)
				d.actions.TakeInhibitor()
				if err := d.actions.StartDiscovery(); err != nil {
					d.log.Warn("failed to start discovery in wait_arrival", "error", err)
				}
				return nil
			}),
			librefsm.WithOnExit(func(c *librefsm.Context) error {
				d := c.Data.(*fsmData)
				d.actions.ReleaseInhibitor()
				return nil
			}),
		).

		// Tag Absent - tag was present but now gone, poll for return
		State(StateTagAbsent,
			librefsm.WithParent(StateDiscoverTag),
			librefsm.WithTimeout(timeCheckReader, EvCheckReaderTimeout),
			librefsm.WithOnEnter(func(c *librefsm.Context) error {
				d := c.Data.(*fsmData)
				if err := d.actions.StartDiscovery(); err != nil {
					d.log.Error("failed to start discovery", "error", err)
					c.Send(librefsm.Event{ID: EvReinit})
					return nil
				}

				// Start polling goroutine for tag arrivals
				// Derive from FSM context so goroutine stops when FSM stops
				pollCtx, cancel := context.WithCancel(d.ctx)
				d.tagAbsentCancel = cancel

				go func() {
					ticker := time.NewTicker(100 * time.Millisecond)
					defer ticker.Stop()
					for {
						select {
						case <-pollCtx.Done():
							return
						case <-ticker.C:
							d.actions.PollForTagArrival()
						}
					}
				}()

				return nil
			}),
			librefsm.WithOnExit(func(c *librefsm.Context) error {
				d := c.Data.(*fsmData)
				// Cancel polling goroutine
				if d.tagAbsentCancel != nil {
					d.tagAbsentCancel()
					d.tagAbsentCancel = nil
				}
				d.actions.ReleaseInhibitor()
				return nil
			}),
		).

		// ================================================================
		// Tag Present states (children of NFCReaderOn)
		// ================================================================

		// Tag Present - parent for all tag communication states
		State(StateTagPresent,
			librefsm.WithParent(StateNFCReaderOn),
			librefsm.WithDefaultChild(StateCondCheckPresence),
		).

		// Condition: Check Presence - decides if tag read succeeded
		ConditionState(StateCondCheckPresence,
			func(c *librefsm.Context) librefsm.StateID {
				d := c.Data.(*fsmData)
				d.actions.TakeInhibitor()
				d.actions.ZeroRetryCounters()
				if d.actions.ReadStatus() == nil {
					d.consecutiveReadFailures = 0
					return StateWaitLastCmd
				}
				return StateCheckPresence
			},
			librefsm.WithParent(StateTagPresent),
			librefsm.WithOnExit(func(c *librefsm.Context) error {
				d := c.Data.(*fsmData)
				d.actions.ReleaseInhibitor()
				return nil
			}),
		).

		// Check Presence - send inserted command, then poll ReadStatus with
		// exponential backoff. Exits early via EvCheckPresenceReady on the
		// first successful read, or at timeCheckPresence via EvCheckPresenceTimeout.
		State(StateCheckPresence,
			librefsm.WithParent(StateTagPresent),
			librefsm.WithTimeout(timeCheckPresence, EvCheckPresenceTimeout),
			librefsm.WithOnEnter(func(c *librefsm.Context) error {
				d := c.Data.(*fsmData)
				d.log.Info("entering check_presence",
					"timeout", timeCheckPresence,
					"poll_min", checkPresencePollMin,
					"poll_max", checkPresencePollMax)
				d.actions.WriteCommand(BMSCmdInsertedInScooter)

				pollCtx, cancel := context.WithCancel(d.ctx)
				d.checkPresenceCancel = cancel

				go func() {
					backoff := checkPresencePollMin
					attempt := 0
					for {
						select {
						case <-pollCtx.Done():
							return
						case <-time.After(backoff):
						}
						attempt++
						if err := d.actions.ReadStatus(); err == nil {
							d.log.Info("check_presence recovered",
								"attempt", attempt, "backoff", backoff)
							d.actions.SendCheckPresenceReady()
							return
						} else {
							d.log.Debug("check_presence poll failed",
								"attempt", attempt, "backoff", backoff, "error", err)
						}
						backoff *= 2
						if backoff > checkPresencePollMax {
							backoff = checkPresencePollMax
						}
					}
				}()
				return nil
			}),
			librefsm.WithOnExit(func(c *librefsm.Context) error {
				d := c.Data.(*fsmData)
				if d.checkPresenceCancel != nil {
					d.checkPresenceCancel()
					d.checkPresenceCancel = nil
				}
				d.log.Info("exiting check_presence")
				return nil
			}),
		).

		// Wait Last Cmd - wait for remaining command time
		State(StateWaitLastCmd,
			librefsm.WithParent(StateTagPresent),
			librefsm.WithOnEnter(func(c *librefsm.Context) error {
				d := c.Data.(*fsmData)
				d.actions.ClearHeartbeatTimer()
				remaining := d.actions.GetRemainingCmdTime()
				c.StartTimer("last_cmd", remaining, librefsm.Event{ID: EvLastCmdTimeout})
				return nil
			}),
		).

		// Condition: Seatbox Lock - check seatbox state
		ConditionState(StateCondSeatboxLock,
			func(c *librefsm.Context) librefsm.StateID {
				d := c.Data.(*fsmData)
				if d.latchedSeatboxClosed {
					d.justInserted = false
					return StateHeartbeat
				}
				return StateCondJustInserted
			},
			librefsm.WithParent(StateTagPresent),
			librefsm.WithOnEnter(func(c *librefsm.Context) error {
				d := c.Data.(*fsmData)
				d.latchedSeatboxClosed = d.actions.GetSeatboxLockClosed()
				if !d.latchedSeatboxClosed {
					d.actions.ReleaseInhibitor()
				}
				return nil
			}),
		).

		// ================================================================
		// Heartbeat states (children of TagPresent)
		// ================================================================

		// Heartbeat - parent for heartbeat action states
		State(StateHeartbeat,
			librefsm.WithParent(StateTagPresent),
			librefsm.WithDefaultChild(StateHeartbeatActions),
			librefsm.WithOnEnter(func(c *librefsm.Context) error {
				d := c.Data.(*fsmData)
				d.actions.StartHeartbeatTimer()
				return nil
			}),
			librefsm.WithOnExit(func(c *librefsm.Context) error {
				d := c.Data.(*fsmData)
				d.actions.ClearHeartbeatTimer()
				return nil
			}),
		).

		// Heartbeat Actions - parent for send states
		State(StateHeartbeatActions,
			librefsm.WithParent(StateHeartbeat),
			librefsm.WithDefaultChild(StateSendClosed),
			librefsm.WithOnEnter(func(c *librefsm.Context) error {
				d := c.Data.(*fsmData)
				d.actions.TakeInhibitor()
				return nil
			}),
			librefsm.WithOnExit(func(c *librefsm.Context) error {
				d := c.Data.(*fsmData)
				d.actions.ReleaseInhibitor()
				return nil
			}),
		).

		// Send Closed - send seatbox closed command
		State(StateSendClosed,
			librefsm.WithParent(StateHeartbeatActions),
			librefsm.WithTimeout(timeCmd, EvClosedTimeout),
			librefsm.WithOnEnter(func(c *librefsm.Context) error {
				d := c.Data.(*fsmData)
				d.actions.WriteCommand(BMSCmdSeatboxClosed)
				return nil
			}),
		).

		// Send On/Off - send on or off command based on enabled state
		State(StateSendOnOff,
			librefsm.WithParent(StateHeartbeatActions),
			librefsm.WithOnEnter(func(c *librefsm.Context) error {
				d := c.Data.(*fsmData)
				var cmd BMSCommand
				if d.actions.GetEnabled() {
					cmd = BMSCmdOn
				} else {
					cmd = BMSCmdOff
				}
				d.actions.WriteCommand(cmd)
				// Timer starts AFTER write so the BMS has the full delay to process
				c.StartTimer("on_off", timeCmd, librefsm.Event{ID: EvOnOffTimeout}, readStatusAction)
				return nil
			}),
		).

		// Condition: State OK - check if BMS state is correct
		ConditionState(StateCondStateOK,
			func(c *librefsm.Context) librefsm.StateID {
				d := c.Data.(*fsmData)
				if d.actions.CheckStateCorrect() {
					return StateWaitUpdate
				}
				return StateSendInsertedClosed
			},
			librefsm.WithParent(StateHeartbeatActions),
			librefsm.WithOnEnter(func(c *librefsm.Context) error {
				d := c.Data.(*fsmData)
				if d.actions.CheckStateCorrect() {
					d.actions.StopDiscovery()
				}
				return nil
			}),
		).

		// Wait Update - wait for heartbeat timeout
		State(StateWaitUpdate,
			librefsm.WithParent(StateHeartbeatActions),
			librefsm.WithOnEnter(func(c *librefsm.Context) error {
				d := c.Data.(*fsmData)
				d.actions.ReleaseInhibitor()
				// Timer is managed by parent StateHeartbeat
				return nil
			}),
		).

		// Send Inserted Closed - send inserted command when state incorrect
		State(StateSendInsertedClosed,
			librefsm.WithParent(StateHeartbeatActions),
			librefsm.WithTimeout(timeCmd, EvInsertedClosedTimeout),
			librefsm.WithOnEnter(func(c *librefsm.Context) error {
				d := c.Data.(*fsmData)
				d.actions.WriteCommand(BMSCmdInsertedInScooter)
				return nil
			}),
		).

		// ================================================================
		// Seatbox open states (children of TagPresent)
		// ================================================================

		// Condition: Just Inserted - check if battery was just inserted
		ConditionState(StateCondJustInserted,
			func(c *librefsm.Context) librefsm.StateID {
				d := c.Data.(*fsmData)
				if d.justInserted {
					return StateCondOff
				}
				return StateSendOff
			},
			librefsm.WithParent(StateTagPresent),
		).

		// Condition: Off - check if battery is inactive
		ConditionState(StateCondOff,
			func(c *librefsm.Context) librefsm.StateID {
				d := c.Data.(*fsmData)
				if d.actions.IsInactive() {
					d.justOpened = true
					return StateSendOpened
				}
				// With keep-active-on-seatbox-open, a running battery skips
				// StateSendOff (which would deactivate it) and goes to heartbeat.
				if d.actions.ShouldKeepActiveOnSeatboxOpen() {
					d.justInserted = false
					return StateHeartbeat
				}
				return StateSendOff
			},
			librefsm.WithParent(StateTagPresent),
		).

		// Send Off - send off command
		State(StateSendOff,
			librefsm.WithParent(StateTagPresent),
			librefsm.WithOnEnter(func(c *librefsm.Context) error {
				d := c.Data.(*fsmData)
				d.actions.WriteCommand(BMSCmdOff)
				// Timer starts AFTER write so the BMS has the full delay to process
				c.StartTimer("off", timeCmd, librefsm.Event{ID: EvOffTimeout}, readStatusAction)
				return nil
			}),
		).

		// Send Opened - send seatbox opened command
		State(StateSendOpened,
			librefsm.WithParent(StateTagPresent),
			librefsm.WithOnEnter(func(c *librefsm.Context) error {
				d := c.Data.(*fsmData)
				d.actions.WriteCommand(BMSCmdSeatboxOpened)
				openedTime := d.actions.GetOpenedTime()
				c.StartTimer("opened", openedTime, librefsm.Event{ID: EvOpenedTimeout})
				return nil
			}),
		).

		// Send Inserted Open - send inserted command when seatbox open
		State(StateSendInsertedOpen,
			librefsm.WithParent(StateTagPresent),
			librefsm.WithOnEnter(func(c *librefsm.Context) error {
				d := c.Data.(*fsmData)
				d.actions.WriteCommand(BMSCmdInsertedInScooter)
				insertedTime := d.actions.GetInsertedTime()
				c.StartTimer("inserted_open", insertedTime, librefsm.Event{ID: EvInsertedOpenTimeout}, readStatusAction)
				return nil
			}),
		).

		// ================================================================
		// Transitions
		// ================================================================

		// Init transitions
		Transition(StateInit, EvInitComplete, StateNFCReaderOn,
			librefsm.WithAction(func(c *librefsm.Context) error {
				d := c.Data.(*fsmData)
				d.log.Info("Initialization complete, starting NFC operations")
				return nil
			}),
		).

		// NFC Reader Off transitions
		Transition(StateNFCReaderOff, EvReinit, StateNFCReaderOn).

		// NFC Reader On transitions
		Transition(StateNFCReaderOn, EvReinit, StateNFCReaderOff).
		Transition(StateNFCReaderOn, EvTagArrived, StateDiscoverTag).

		// Discover Tag transitions
		Transition(StateDiscoverTag, EvReinit, StateNFCReaderOff).
		Transition(StateDiscoverTag, EvTagArrived, StateTagPresent,
			librefsm.WithAction(markJustInsertedAction),
		).
		Transition(StateDiscoverTag, EvTagDeparted, StateTagAbsent).

		// Wait Arrival transitions
		Transition(StateWaitArrival, EvReinit, StateNFCReaderOff).
		Transition(StateWaitArrival, EvTagArrived, StateTagPresent,
			librefsm.WithAction(markJustInsertedAction),
		).
		Transition(StateWaitArrival, EvDepartureTimeout, StateTagAbsent).

		// Tag Absent transitions
		Transition(StateTagAbsent, EvReinit, StateNFCReaderOff).
		Transition(StateTagAbsent, EvTagArrived, StateTagPresent,
			librefsm.WithAction(markJustInsertedAction),
		).
		Transition(StateTagAbsent, EvRestart, StateTagAbsent,
			librefsm.WithAction(func(c *librefsm.Context) error {
				d := c.Data.(*fsmData)
				if err := d.actions.StartDiscovery(); err != nil {
					d.log.Error("failed to restart discovery", "error", err)
					c.Send(librefsm.Event{ID: EvReinit})
				}
				return nil
			}),
		).

		// Tag Present parent transitions (apply to ALL child states including condition states)
		Transition(StateTagPresent, EvTagDeparted, StateDiscoverTag).
		Transition(StateTagPresent, EvReinit, StateNFCReaderOff).
		Transition(StateTagPresent, EvRestart, StateTagPresent).

		// Check Presence transitions
		// Absorb EvRestart during check_presence - prevents restart loops while
		// already verifying tag presence. Self-transition is a no-op (LCA = self).
		Transition(StateCheckPresence, EvRestart, StateCheckPresence).
		Transition(StateCheckPresence, EvCheckPresenceTimeout, StateCondCheckPresence).
		Transition(StateCheckPresence, EvCheckPresenceReady, StateCondCheckPresence).

		// Wait Last Cmd transitions
		Transition(StateWaitLastCmd, EvLastCmdTimeout, StateCondSeatboxLock).

		// Heartbeat transitions (apply to all heartbeat substates via hierarchy)
		// Guarded: with keep-active-on-seatbox-open, the event is absorbed and
		// the battery keeps heartbeating across the seatbox open.
		Transition(StateHeartbeat, EvSeatboxOpened, StateCondJustInserted,
			librefsm.WithGuard(func(c *librefsm.Context) bool {
				d := c.Data.(*fsmData)
				return !d.actions.ShouldKeepActiveOnSeatboxOpen()
			}),
		).

		// HeartbeatActions transitions
		Transition(StateHeartbeatActions, EvHeartbeatTimeout, StateHeartbeat).

		// Send Closed transitions
		Transition(StateSendClosed, EvClosedTimeout, StateSendOnOff).

		// Send OnOff transitions
		Transition(StateSendOnOff, EvOnOffTimeout, StateCondStateOK).

		// Wait Update transitions
		// Transition to StateHeartbeat (not HeartbeatActions) to restart the timer
		// State check moved here from timer callback to avoid race conditions
		Transition(StateWaitUpdate, EvHeartbeatTimeout, StateHeartbeat,
			librefsm.WithAction(func(c *librefsm.Context) error {
				d := c.Data.(*fsmData)
				if !d.actions.CheckStateCorrect() {
					// BMS state doesn't match expected - trigger tag departure
					d.log.Warn("State mismatch on heartbeat, triggering departure")
					c.Send(librefsm.Event{ID: EvTagDeparted})
				}
				return nil
			}),
		).

		// Send Inserted Closed transitions
		Transition(StateSendInsertedClosed, EvInsertedClosedTimeout, StateSendClosed).

		// Seatbox open state transitions
		Transition(StateSendOff, EvOffTimeout, StateCondOff).
		Transition(StateSendOpened, EvOpenedTimeout, StateSendInsertedOpen,
			librefsm.WithAction(func(c *librefsm.Context) error {
				d := c.Data.(*fsmData)
				d.justOpened = false
				return nil
			}),
		).

		// With keep-active-on-seatbox-open, break out of the seatbox-open
		// maintenance loop into heartbeat after the wake-up cycle completes.
		// The battery has now seen OFF -> OPENED -> INSERTED_IN_SCOOTER and
		// is awake, so heartbeat can take over and send ON.
		Transition(StateSendInsertedOpen, EvInsertedOpenTimeout, StateHeartbeat,
			librefsm.WithGuard(func(c *librefsm.Context) bool {
				d := c.Data.(*fsmData)
				return d.actions.ShouldKeepActiveOnSeatboxOpen()
			}),
			librefsm.WithAction(func(c *librefsm.Context) error {
				d := c.Data.(*fsmData)
				d.justInserted = false
				return nil
			}),
		).
		Transition(StateSendInsertedOpen, EvInsertedOpenTimeout, StateSendOpened,
			librefsm.WithAction(func(c *librefsm.Context) error {
				d := c.Data.(*fsmData)
				d.justInserted = false
				return nil
			}),
		).

		// Set initial state
		Initial(StateInit)
}
