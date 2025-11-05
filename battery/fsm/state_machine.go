package fsm

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

const eventQueueSize = 100

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
	WriteCommand(cmd BMSCommand)
	GetEnabled() bool
	GetSeatboxLockClosed() bool
	GetVehicleActive() bool
	CheckStateCorrect() bool
	GetRemainingCmdTime() time.Duration
	GetOpenedTime() time.Duration
	GetInsertedTime() time.Duration
	IsInactive() bool
	ZeroRetryCounters()
	StopHeartbeatTimer()
	StartHeartbeatTimer()
	ClearHeartbeatTimer()
	StopTimerIfBatteryEmpty()
	IsRoleInactive() bool
}

type StateMachine struct {
	mu     sync.RWMutex
	state  State
	events chan Event
	log    *slog.Logger

	actions BatteryActions

	timers map[string]*time.Timer

	justInserted         bool
	justOpened           bool
	latchedSeatboxClosed bool
	lastOperationSuccess bool

	tagAbsentCancel context.CancelFunc
}

func New(
	actions BatteryActions,
	log *slog.Logger,
) *StateMachine {
	return &StateMachine{
		state:                StateInit,
		events:               make(chan Event, eventQueueSize),
		log:                  log,
		actions:              actions,
		timers:               make(map[string]*time.Timer),
		justInserted:         true,
		latchedSeatboxClosed: false,
		justOpened:           false,
	}
}

func (sm *StateMachine) Run(ctx context.Context) {
	sm.log.Info("state machine started", "initial_state", sm.state.String())
	sm.enterState(ctx, sm.state)

	for {
		select {
		case <-ctx.Done():
			sm.log.Info("state machine stopping")
			sm.stopAllTimers()
			return

		case event := <-sm.events:
			sm.handleEvent(ctx, event)
		}
	}
}

func (sm *StateMachine) SendEvent(event Event) {
	select {
	case sm.events <- event:
	default:
		sm.log.Warn("event queue full, dropping event", "event", event.Type())
	}
}

func (sm *StateMachine) State() State {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.state
}

func (sm *StateMachine) handleEvent(ctx context.Context, event Event) {
	sm.mu.Lock()
	currentState := sm.state
	sm.mu.Unlock()

	sm.log.Debug("handling event", "event", event.Type(), "state", currentState.String())

	transition := sm.getTransition(event)

	if transition.NextState != currentState {
		sm.log.Info(fmt.Sprintf("State Transition: %s -(%s)-> %s",
			currentState.String(),
			event.Type(),
			transition.NextState.String()))

		sm.exitState(ctx, currentState)

		sm.mu.Lock()
		sm.state = transition.NextState
		sm.mu.Unlock()

		if transition.Action != nil {
			transition.Action(ctx)
		}

		sm.enterStateWithDefault(ctx, transition.NextState)
	} else if transition.NextState == currentState && transition.Action != nil {
		sm.log.Debug("same-state transition with action",
			"state", currentState.String(),
			"event", event.Type())

		transition.Action(ctx)
	}
}

func (sm *StateMachine) enterStateWithDefault(ctx context.Context, state State) {
	sm.enterState(ctx, state)

	sm.mu.Lock()
	currentState := sm.state
	sm.mu.Unlock()

	if currentState != state {
		return
	}

	// Handle condition nodes (jump transitions)
	if state.IsCondition() {
		jumpTarget, reason := sm.evaluateJump(state)
		if jumpTarget != StateRoot && jumpTarget != state {
			sm.log.Info(fmt.Sprintf("State Transition: %s -(%s)-> %s",
				state.String(),
				reason,
				jumpTarget.String()))
			sm.mu.Lock()
			sm.state = jumpTarget
			sm.mu.Unlock()
			sm.enterStateWithDefault(ctx, jumpTarget)
			return
		}
	}

	// Handle default child entry
	defaultChild := state.DefaultChild()
	if defaultChild != StateRoot {
		sm.log.Debug("auto-entering default child", "parent", state.String(), "child", defaultChild.String())
		sm.mu.Lock()
		sm.state = defaultChild
		sm.mu.Unlock()
		sm.enterStateWithDefault(ctx, defaultChild)
	}
}

func (sm *StateMachine) isInHierarchy(target State) bool {
	sm.mu.RLock()
	current := sm.state
	sm.mu.RUnlock()

	for current != StateRoot {
		if current == target {
			return true
		}
		current = current.Parent()
	}
	return false
}

func (sm *StateMachine) startTimer(name string, duration time.Duration, callback func()) {
	sm.stopTimer(name)

	timer := time.AfterFunc(duration, func() {
		sm.mu.Lock()
		delete(sm.timers, name)
		sm.mu.Unlock()
		callback()
	})

	sm.mu.Lock()
	sm.timers[name] = timer
	sm.mu.Unlock()
}

func (sm *StateMachine) stopTimer(name string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if timer, exists := sm.timers[name]; exists {
		timer.Stop()
		delete(sm.timers, name)
	}
}

func (sm *StateMachine) stopAllTimers() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	for name, timer := range sm.timers {
		timer.Stop()
		delete(sm.timers, name)
	}
}

func (sm *StateMachine) evaluateJump(state State) (State, string) {
	switch state {
	case StateCondCheckPresence:
		if sm.lastOperationSuccess {
			return StateWaitLastCmd, "presence_ok"
		}
		return StateCheckPresence, "presence_failed"

	case StateCondSeatboxLock:
		if sm.latchedSeatboxClosed {
			sm.justInserted = false
			return StateHeartbeat, "seatbox_closed"
		}
		return StateCondJustInserted, "seatbox_open"

	case StateCondStateOK:
		if sm.actions.CheckStateCorrect() {
			return StateWaitUpdate, "state_ok"
		}
		return StateSendInsertedClosed, "state_incorrect"

	case StateCondJustInserted:
		if sm.justInserted {
			return StateCondOff, "just_inserted"
		}
		return StateSendOff, "not_just_inserted"

	case StateCondOff:
		if sm.actions.IsInactive() {
			sm.justOpened = true
			return StateSendOpened, "inactive"
		}
		return StateSendOff, "active"

	default:
		return StateRoot, "unknown"
	}
}
