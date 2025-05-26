package battery

import (
	"fmt"
	"sync"
	"time"

	"battery-service/nfc/hal"
)

// BatteryEvent represents events that can trigger state transitions
type BatteryEvent int

const (
	EventBatteryInserted BatteryEvent = iota
	EventBatteryRemoved
	EventSeatboxOpened
	EventSeatboxClosed
	EventReadyToScoot
	EventNotReadyToScoot
	EventLowSOC
	EventSOCRestored
	EventCommandSent
	EventCommandFailed
	EventStateVerified
	EventStateVerificationFailed
	EventCBChargeHigh
	EventCBChargeLow
	EventVehicleStandby
	EventVehicleActive
	EventDisabled
	EventEnabled
	EventHeartbeatTick
	EventMaintenanceTick
	EventHALError
	EventHALRecovered
	EventAuxBatteryLow
	EventAuxBatteryOK
)

// String returns a string representation of the battery event
func (e BatteryEvent) String() string {
	switch e {
	case EventBatteryInserted:
		return "BatteryInserted"
	case EventBatteryRemoved:
		return "BatteryRemoved"
	case EventSeatboxOpened:
		return "SeatboxOpened"
	case EventSeatboxClosed:
		return "SeatboxClosed"
	case EventReadyToScoot:
		return "ReadyToScoot"
	case EventNotReadyToScoot:
		return "NotReadyToScoot"
	case EventLowSOC:
		return "LowSOC"
	case EventSOCRestored:
		return "SOCRestored"
	case EventCommandSent:
		return "CommandSent"
	case EventCommandFailed:
		return "CommandFailed"
	case EventStateVerified:
		return "StateVerified"
	case EventStateVerificationFailed:
		return "StateVerificationFailed"
	case EventCBChargeHigh:
		return "CBChargeHigh"
	case EventCBChargeLow:
		return "CBChargeLow"
	case EventVehicleStandby:
		return "VehicleStandby"
	case EventVehicleActive:
		return "VehicleActive"
	case EventDisabled:
		return "Disabled"
	case EventEnabled:
		return "Enabled"
	case EventHeartbeatTick:
		return "HeartbeatTick"
	case EventMaintenanceTick:
		return "MaintenanceTick"
	case EventHALError:
		return "HALError"
	case EventHALRecovered:
		return "HALRecovered"
	case EventAuxBatteryLow:
		return "AuxBatteryLow"
	case EventAuxBatteryOK:
		return "AuxBatteryOK"
	default:
		return "Unknown"
	}
}

// BatteryMachineState represents the state machine states
type BatteryMachineState int

const (
	StateNotPresent BatteryMachineState = iota
	StateInitializing
	StateIdleStandby
	StateIdleReady
	StateActiveRequested
	StateActive
	StateDeactivating
	StateError
	StateDisabled
	StateMaintenance
)

// String returns a string representation of the machine state
func (s BatteryMachineState) String() string {
	switch s {
	case StateNotPresent:
		return "NotPresent"
	case StateInitializing:
		return "Initializing"
	case StateIdleStandby:
		return "IdleStandby"
	case StateIdleReady:
		return "IdleReady"
	case StateActiveRequested:
		return "ActiveRequested"
	case StateActive:
		return "Active"
	case StateDeactivating:
		return "Deactivating"
	case StateError:
		return "Error"
	case StateDisabled:
		return "Disabled"
	case StateMaintenance:
		return "Maintenance"
	default:
		return "Unknown"
	}
}

// StateTransition represents a transition between states
type StateTransition struct {
	FromState BatteryMachineState
	Event     BatteryEvent
	ToState   BatteryMachineState
	Action    func(*BatteryStateMachine, BatteryEvent) error
}

// BatteryStateMachine manages the state transitions for a battery
type BatteryStateMachine struct {
	sync.RWMutex
	reader          *BatteryReader
	currentState    BatteryMachineState
	transitions     map[stateEventKey]*StateTransition
	eventQueue      chan BatteryEvent
	stopChan        chan struct{}
	lastStateChange time.Time
	stateHistory    []StateHistoryEntry
	maxHistorySize  int
	logger          func(level hal.LogLevel, message string)
}

type stateEventKey struct {
	state BatteryMachineState
	event BatteryEvent
}

type StateHistoryEntry struct {
	FromState BatteryMachineState
	ToState   BatteryMachineState
	Event     BatteryEvent
	Timestamp time.Time
	Error     error
}

// NewBatteryStateMachine creates a new state machine for a battery reader
func NewBatteryStateMachine(reader *BatteryReader) *BatteryStateMachine {
	sm := &BatteryStateMachine{
		reader:          reader,
		currentState:    StateNotPresent,
		transitions:     make(map[stateEventKey]*StateTransition),
		eventQueue:      make(chan BatteryEvent, 100), // Buffered to prevent blocking
		stopChan:        make(chan struct{}),
		maxHistorySize:  50,
		lastStateChange: time.Now(),
		logger:          reader.logCallback,
	}

	sm.setupTransitions()
	return sm
}

// setupTransitions defines all valid state transitions
func (sm *BatteryStateMachine) setupTransitions() {
	transitions := []StateTransition{
		// From NotPresent
		{StateNotPresent, EventBatteryInserted, StateInitializing, sm.actionInitializeBattery},
		{StateNotPresent, EventDisabled, StateDisabled, sm.actionDisable},

		// From Initializing
		{StateInitializing, EventReadyToScoot, StateIdleReady, sm.actionBatteryReady},
		{StateInitializing, EventBatteryRemoved, StateNotPresent, sm.actionBatteryRemoved},
		{StateInitializing, EventHALError, StateError, sm.actionHALError},
		{StateInitializing, EventDisabled, StateDisabled, sm.actionDisable},
		{StateInitializing, EventVehicleActive, StateInitializing, nil}, // Stay in Initializing, wait for ReadyToScoot

		// From IdleStandby
		{StateIdleStandby, EventSeatboxClosed, StateIdleReady, sm.actionSeatboxClosed},
		{StateIdleStandby, EventCBChargeLow, StateIdleReady, sm.actionCheckActivationConditions},
		{StateIdleStandby, EventVehicleActive, StateIdleReady, sm.actionVehicleActive},
		{StateIdleStandby, EventBatteryRemoved, StateNotPresent, sm.actionBatteryRemoved},
		{StateIdleStandby, EventLowSOC, StateIdleStandby, sm.actionLowSOC},
		{StateIdleStandby, EventMaintenanceTick, StateMaintenance, sm.actionStartMaintenance},
		{StateIdleStandby, EventHeartbeatTick, StateIdleStandby, sm.actionHeartbeat},
		{StateIdleStandby, EventDisabled, StateDisabled, sm.actionDisable},

		// From IdleReady
		{StateIdleReady, EventSeatboxOpened, StateIdleStandby, sm.actionSeatboxOpened},
		{StateIdleReady, EventCBChargeHigh, StateIdleStandby, sm.actionCBChargeHigh},
		{StateIdleReady, EventVehicleStandby, StateIdleStandby, sm.actionVehicleStandby},
		{StateIdleReady, EventSeatboxClosed, StateActiveRequested, sm.actionRequestActivation},
		{StateIdleReady, EventBatteryRemoved, StateNotPresent, sm.actionBatteryRemoved},
		{StateIdleReady, EventLowSOC, StateIdleStandby, sm.actionLowSOC},
		{StateIdleReady, EventHeartbeatTick, StateIdleReady, sm.actionHeartbeat},
		{StateIdleReady, EventDisabled, StateDisabled, sm.actionDisable},

		// From ActiveRequested
		{StateActiveRequested, EventStateVerified, StateActive, sm.actionActivationSuccess},
		{StateActiveRequested, EventStateVerificationFailed, StateError, sm.actionActivationFailed},
		{StateActiveRequested, EventCommandFailed, StateError, sm.actionCommandFailed},
		{StateActiveRequested, EventBatteryRemoved, StateNotPresent, sm.actionBatteryRemoved},
		{StateActiveRequested, EventSeatboxOpened, StateDeactivating, sm.actionRequestDeactivation},
		{StateActiveRequested, EventLowSOC, StateActiveRequested, sm.actionLowSOCWhileActive},
		{StateActiveRequested, EventDisabled, StateDisabled, sm.actionDisable},

		// From Active
		{StateActive, EventSeatboxOpened, StateDeactivating, sm.actionRequestDeactivation},
		{StateActive, EventLowSOC, StateActive, sm.actionLowSOCWhileActive},
		{StateActive, EventCBChargeHigh, StateActive, sm.actionCheckDeactivationConditions},
		{StateActive, EventVehicleStandby, StateActive, sm.actionCheckDeactivationConditions},
		{StateActive, EventAuxBatteryOK, StateActive, sm.actionCheckDeactivationConditions},
		{StateActive, EventBatteryRemoved, StateNotPresent, sm.actionBatteryRemoved},
		{StateActive, EventHeartbeatTick, StateActive, sm.actionHeartbeat},
		{StateActive, EventMaintenanceTick, StateActive, sm.actionActiveStatusPoll},
		{StateActive, EventHALError, StateError, sm.actionHALError},
		{StateActive, EventDisabled, StateDisabled, sm.actionDisable},

		// From Deactivating
		{StateDeactivating, EventStateVerified, StateIdleStandby, sm.actionDeactivationSuccess},
		{StateDeactivating, EventStateVerificationFailed, StateError, sm.actionDeactivationFailed},
		{StateDeactivating, EventCommandFailed, StateError, sm.actionCommandFailed},
		{StateDeactivating, EventBatteryRemoved, StateNotPresent, sm.actionBatteryRemoved},
		{StateDeactivating, EventDisabled, StateDisabled, sm.actionDisable},

		// From Error
		{StateError, EventHALRecovered, StateIdleStandby, sm.actionRecovery},
		{StateError, EventBatteryRemoved, StateNotPresent, sm.actionBatteryRemoved},
		{StateError, EventHeartbeatTick, StateError, sm.actionHeartbeatInError},
		{StateError, EventDisabled, StateDisabled, sm.actionDisable},

		// From Disabled
		{StateDisabled, EventEnabled, StateNotPresent, sm.actionEnable},
		{StateDisabled, EventVehicleActive, StateNotPresent, sm.actionVehicleActiveWhileDisabled},
		{StateDisabled, EventCBChargeLow, StateNotPresent, sm.actionEnable},
		{StateDisabled, EventBatteryInserted, StateDisabled, sm.actionBatteryInsertedWhileDisabled},
		{StateDisabled, EventBatteryRemoved, StateDisabled, sm.actionBatteryRemoved},

		// From Maintenance
		{StateMaintenance, EventMaintenanceTick, StateIdleStandby, sm.actionMaintenanceComplete},
		{StateMaintenance, EventBatteryRemoved, StateNotPresent, sm.actionBatteryRemoved},
		{StateMaintenance, EventHALError, StateError, sm.actionHALError},
		{StateMaintenance, EventDisabled, StateDisabled, sm.actionDisable},
	}

	// Build transition map
	for i := range transitions {
		t := &transitions[i]
		key := stateEventKey{t.FromState, t.Event}
		sm.transitions[key] = t
	}
}

// Start starts the state machine event processing loop
func (sm *BatteryStateMachine) Start() {
	go sm.eventProcessingLoop()
}

// Stop stops the state machine
func (sm *BatteryStateMachine) Stop() {
	close(sm.stopChan)
}

// SendEvent sends an event to the state machine
func (sm *BatteryStateMachine) SendEvent(event BatteryEvent) {
	select {
	case sm.eventQueue <- event:
	case <-sm.stopChan:
		return
	default:
		// Event queue is full, log warning but don't block
		sm.logger(hal.LogLevelWarning, fmt.Sprintf("Event queue full, dropping event: %s", event))
	}
}

// GetCurrentState returns the current state (thread-safe)
func (sm *BatteryStateMachine) GetCurrentState() BatteryMachineState {
	sm.RLock()
	defer sm.RUnlock()
	return sm.currentState
}

// eventProcessingLoop processes events sequentially to avoid race conditions
func (sm *BatteryStateMachine) eventProcessingLoop() {
	for {
		select {
		case <-sm.stopChan:
			return
		case event := <-sm.eventQueue:
			sm.processEvent(event)
		}
	}
}

// processEvent handles a single event (not thread-safe, called from eventProcessingLoop)
func (sm *BatteryStateMachine) processEvent(event BatteryEvent) {
	sm.Lock()
	defer sm.Unlock()

	currentState := sm.currentState
	key := stateEventKey{currentState, event}

	transition, exists := sm.transitions[key]
	if !exists {
		sm.logger(hal.LogLevelDebug, fmt.Sprintf("No transition for state %s on event %s", currentState, event))
		return
	}

	sm.logger(hal.LogLevelDebug, fmt.Sprintf("State transition: %s + %s -> %s", currentState, event, transition.ToState))

	var err error
	if transition.Action != nil {
		err = transition.Action(sm, event)
	}

	// Record transition in history
	historyEntry := StateHistoryEntry{
		FromState: currentState,
		ToState:   transition.ToState,
		Event:     event,
		Timestamp: time.Now(),
		Error:     err,
	}

	sm.stateHistory = append(sm.stateHistory, historyEntry)
	if len(sm.stateHistory) > sm.maxHistorySize {
		sm.stateHistory = sm.stateHistory[1:]
	}

	if err != nil {
		sm.logger(hal.LogLevelError, fmt.Sprintf("Action failed for transition %s + %s -> %s: %v", currentState, event, transition.ToState, err))
		// On action failure, transition to error state if not already there
		if transition.ToState != StateError && currentState != StateError {
			sm.currentState = StateError
			sm.lastStateChange = time.Now()
		}
	} else {
		// Successful transition
		sm.currentState = transition.ToState
		sm.lastStateChange = time.Now()
		sm.logger(hal.LogLevelInfo, fmt.Sprintf("State changed to %s", transition.ToState))
	}
}

// GetStateHistory returns a copy of the state history
func (sm *BatteryStateMachine) GetStateHistory() []StateHistoryEntry {
	sm.RLock()
	defer sm.RUnlock()

	history := make([]StateHistoryEntry, len(sm.stateHistory))
	copy(history, sm.stateHistory)
	return history
}

// CanTransition checks if a transition is valid without executing it
func (sm *BatteryStateMachine) CanTransition(event BatteryEvent) bool {
	sm.RLock()
	defer sm.RUnlock()

	key := stateEventKey{sm.currentState, event}
	_, exists := sm.transitions[key]
	return exists
}
