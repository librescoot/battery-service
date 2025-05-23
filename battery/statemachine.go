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
		{StateIdleStandby, EventDisabled, StateDisabled, sm.actionDisable},

		// From IdleReady
		{StateIdleReady, EventSeatboxOpened, StateIdleStandby, sm.actionSeatboxOpened},
		{StateIdleReady, EventCBChargeHigh, StateIdleStandby, sm.actionCBChargeHigh},
		{StateIdleReady, EventVehicleStandby, StateIdleStandby, sm.actionVehicleStandby},
		{StateIdleReady, EventSeatboxClosed, StateActiveRequested, sm.actionRequestActivation},
		{StateIdleReady, EventBatteryRemoved, StateNotPresent, sm.actionBatteryRemoved},
		{StateIdleReady, EventLowSOC, StateIdleStandby, sm.actionLowSOC},
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
		{StateActive, EventCBChargeHigh, StateDeactivating, sm.actionRequestDeactivation},
		{StateActive, EventVehicleStandby, StateDeactivating, sm.actionRequestDeactivation},
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

// Action methods for state transitions

func (sm *BatteryStateMachine) actionInitializeBattery(machine *BatteryStateMachine, event BatteryEvent) error {
	// Read initial status
	if err := sm.reader.readBatteryStatus(); err != nil {
		// If we can't read status, send InsertedInScooter as recovery
		if err := sm.reader.sendCommand(sm.reader.service.ctx, BatteryCommandInsertedInScooter); err != nil {
			return fmt.Errorf("failed to send InsertedInScooter: %w", err)
		}
		time.Sleep(10 * time.Second) // Wait like C code does

		// Try reading status again
		if err := sm.reader.readBatteryStatus(); err != nil {
			return fmt.Errorf("failed to read status after InsertedInScooter: %w", err)
		}
	}

	// If we got here, battery is responding properly
	sm.reader.Lock()
	sm.reader.readyToScoot = true // Just set it true, don't wait for response
	sm.reader.justInserted = true
	sm.reader.data.Present = true
	sm.reader.Unlock()

	// Send the ready event
	sm.SendEvent(EventReadyToScoot)

	return nil
}

func (sm *BatteryStateMachine) actionBatteryReady(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.reader.Lock()
	sm.reader.readyToScoot = true
	sm.reader.justInserted = false
	sm.reader.data.State = BatteryStateIdle
	sm.reader.Unlock()

	// Update Redis with initial state
	if err := sm.reader.updateRedisStatus(); err != nil {
		return fmt.Errorf("failed to update Redis: %w", err)
	}

	// Check initial conditions to determine if we should be in standby or ready
	sm.reader.service.Lock()
	vehicleState := sm.reader.service.vehicleState
	cbCharge := sm.reader.service.cbBatteryCharge
	seatboxOpen := sm.reader.service.seatboxOpen
	sm.reader.service.Unlock()

	// If conditions suggest standby mode, send appropriate event
	if seatboxOpen || (vehicleState == "stand-by" && cbCharge >= cbBatteryActivationThreshold) {
		sm.SendEvent(EventSeatboxOpened) // This will transition to IdleStandby
	}

	return nil
}

func (sm *BatteryStateMachine) actionBatteryRemoved(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.reader.Lock()
	sm.reader.data.Present = false
	sm.reader.readyToScoot = false
	sm.reader.justInserted = false
	sm.reader.consecutiveTagAbsences = 0
	sm.reader.Unlock()

	// Send BatteryRemoved command
	if err := sm.reader.sendCommand(sm.reader.service.ctx, BatteryCommandBatteryRemoved); err != nil {
		sm.logger(hal.LogLevelWarning, fmt.Sprintf("Failed to send BatteryRemoved command: %v", err))
	}

	// Update Redis
	if err := sm.reader.updateRedisStatus(); err != nil {
		sm.logger(hal.LogLevelWarning, fmt.Sprintf("Failed to update Redis after battery removal: %v", err))
	}

	return nil
}

func (sm *BatteryStateMachine) actionSeatboxOpened(machine *BatteryStateMachine, event BatteryEvent) error {
	// Send UserOpenedSeatbox command
	if err := sm.reader.sendCommand(sm.reader.service.ctx, BatteryCommandUserOpenedSeatbox); err != nil {
		return fmt.Errorf("failed to send UserOpenedSeatbox: %w", err)
	}
	return nil
}

func (sm *BatteryStateMachine) actionSeatboxClosed(machine *BatteryStateMachine, event BatteryEvent) error {
	// Send UserClosedSeatbox command
	if err := sm.reader.sendCommand(sm.reader.service.ctx, BatteryCommandUserClosedSeatbox); err != nil {
		return fmt.Errorf("failed to send UserClosedSeatbox: %w", err)
	}
	return nil
}

func (sm *BatteryStateMachine) actionCheckActivationConditions(machine *BatteryStateMachine, event BatteryEvent) error {
	// Check if battery should be activated based on current conditions
	if sm.reader.index != 0 {
		return nil // Only battery 0 can be activated
	}

	sm.reader.service.Lock()
	vehicleState := sm.reader.service.vehicleState
	cbCharge := sm.reader.service.cbBatteryCharge
	seatboxOpen := sm.reader.service.seatboxOpen
	sm.reader.service.Unlock()

	sm.reader.Lock()
	ready := sm.reader.readyToScoot
	lowSOC := sm.reader.data.LowSOC
	enabled := sm.reader.enabled
	sm.reader.Unlock()

	shouldActivate := enabled && ready && !lowSOC && !seatboxOpen &&
		(vehicleState != "stand-by" || (cbCharge >= 0 && cbCharge < cbBatteryActivationThreshold))

	if shouldActivate {
		sm.SendEvent(EventSeatboxClosed) // This will trigger activation
	}

	return nil
}

func (sm *BatteryStateMachine) actionRequestActivation(machine *BatteryStateMachine, event BatteryEvent) error {
	// Only battery 0 can be activated
	if sm.reader.index != 0 {
		return nil
	}

	// Check pre-conditions
	sm.reader.Lock()
	ready := sm.reader.readyToScoot
	lowSOC := sm.reader.data.LowSOC
	enabled := sm.reader.enabled
	state := sm.reader.data.State
	sm.reader.Unlock()

	if !enabled || !ready || lowSOC {
		return fmt.Errorf("activation preconditions not met: enabled=%v, ready=%v, lowSOC=%v", enabled, ready, lowSOC)
	}

	if state == BatteryStateActive {
		sm.SendEvent(EventStateVerified)
		return nil
	}

	// Send ON command
	if err := sm.reader.sendCommand(sm.reader.service.ctx, BatteryCommandOn); err != nil {
		sm.SendEvent(EventCommandFailed)
		return fmt.Errorf("failed to send ON command: %w", err)
	}

	// Schedule state verification
	go func() {
		time.Sleep(timeStateVerify)
		if err := sm.reader.readBatteryStatus(); err != nil {
			sm.SendEvent(EventStateVerificationFailed)
		} else {
			sm.reader.Lock()
			newState := sm.reader.data.State
			sm.reader.Unlock()

			if newState == BatteryStateActive {
				sm.SendEvent(EventStateVerified)
			} else {
				sm.SendEvent(EventStateVerificationFailed)
			}
		}
	}()

	return nil
}

func (sm *BatteryStateMachine) actionRequestDeactivation(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.reader.Lock()
	state := sm.reader.data.State
	sm.reader.Unlock()

	if state == BatteryStateIdle || state == BatteryStateAsleep {
		sm.SendEvent(EventStateVerified)
		return nil
	}

	// Send OFF command
	if err := sm.reader.sendCommand(sm.reader.service.ctx, BatteryCommandOff); err != nil {
		sm.SendEvent(EventCommandFailed)
		return fmt.Errorf("failed to send OFF command: %w", err)
	}

	// Schedule state verification
	go func() {
		time.Sleep(timeStateVerify)
		if err := sm.reader.readBatteryStatus(); err != nil {
			sm.SendEvent(EventStateVerificationFailed)
		} else {
			sm.reader.Lock()
			newState := sm.reader.data.State
			sm.reader.Unlock()

			if newState == BatteryStateIdle || newState == BatteryStateAsleep {
				sm.SendEvent(EventStateVerified)
			} else {
				sm.SendEvent(EventStateVerificationFailed)
			}
		}
	}()

	return nil
}

func (sm *BatteryStateMachine) actionActivationSuccess(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.logger(hal.LogLevelInfo, "Battery successfully activated")
	return sm.reader.updateRedisStatus()
}

func (sm *BatteryStateMachine) actionDeactivationSuccess(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.logger(hal.LogLevelInfo, "Battery successfully deactivated")
	return sm.reader.updateRedisStatus()
}

func (sm *BatteryStateMachine) actionActivationFailed(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.reader.Lock()
	sm.reader.data.Faults.NotFollowingCommand = true
	sm.reader.Unlock()

	sm.logger(hal.LogLevelError, "Battery activation failed")
	return sm.reader.updateRedisStatus()
}

func (sm *BatteryStateMachine) actionDeactivationFailed(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.reader.Lock()
	sm.reader.data.Faults.NotFollowingCommand = true
	sm.reader.Unlock()

	sm.logger(hal.LogLevelError, "Battery deactivation failed")
	return sm.reader.updateRedisStatus()
}

func (sm *BatteryStateMachine) actionHeartbeat(machine *BatteryStateMachine, event BatteryEvent) error {
	// Send ScooterHeartbeat for active battery
	if sm.reader.index == 0 {
		return sm.reader.sendCommand(sm.reader.service.ctx, BatteryCommandScooterHeartbeat)
	}
	return nil
}

func (sm *BatteryStateMachine) actionActiveStatusPoll(machine *BatteryStateMachine, event BatteryEvent) error {
	// Poll status for active battery
	return sm.reader.readBatteryStatus()
}

func (sm *BatteryStateMachine) actionStartMaintenance(machine *BatteryStateMachine, event BatteryEvent) error {
	// Start maintenance cycle for idle batteries
	sm.logger(hal.LogLevelDebug, "Starting maintenance cycle")
	return nil
}

func (sm *BatteryStateMachine) actionMaintenanceComplete(machine *BatteryStateMachine, event BatteryEvent) error {
	// Complete maintenance cycle
	sm.logger(hal.LogLevelDebug, "Maintenance cycle complete")
	return sm.reader.readBatteryStatus()
}

func (sm *BatteryStateMachine) actionLowSOC(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.logger(hal.LogLevelWarning, "Battery SOC is low")
	return sm.reader.updateRedisStatus()
}

func (sm *BatteryStateMachine) actionCBChargeHigh(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.logger(hal.LogLevelDebug, "CB battery charge is high, entering standby mode")
	return nil
}

func (sm *BatteryStateMachine) actionVehicleStandby(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.logger(hal.LogLevelDebug, "Vehicle entered standby mode")
	return nil
}

func (sm *BatteryStateMachine) actionVehicleActive(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.logger(hal.LogLevelDebug, "Vehicle became active (non-standby) - checking activation conditions")

	// Check if battery should be activated based on current conditions
	if sm.reader.index != 0 {
		return nil // Only battery 0 can be activated
	}

	sm.reader.service.Lock()
	vehicleState := sm.reader.service.vehicleState
	cbCharge := sm.reader.service.cbBatteryCharge
	seatboxOpen := sm.reader.service.seatboxOpen
	sm.reader.service.Unlock()

	sm.reader.Lock()
	ready := sm.reader.readyToScoot
	lowSOC := sm.reader.data.LowSOC
	enabled := sm.reader.enabled
	sm.reader.Unlock()

	shouldActivate := enabled && ready && !lowSOC && !seatboxOpen &&
		(vehicleState != "stand-by" || (cbCharge >= 0 && cbCharge < cbBatteryActivationThreshold))

	if shouldActivate {
		sm.logger(hal.LogLevelInfo, fmt.Sprintf("Vehicle active conditions met for Battery 0 activation: vehicleState=%s, enabled=%v, ready=%v, lowSOC=%v, seatboxOpen=%v", vehicleState, enabled, ready, lowSOC, seatboxOpen))
		sm.SendEvent(EventSeatboxClosed) // This will trigger activation through StateIdleReady -> StateActiveRequested
	} else {
		sm.logger(hal.LogLevelDebug, fmt.Sprintf("Vehicle active but activation conditions not met: vehicleState=%s, enabled=%v, ready=%v, lowSOC=%v, seatboxOpen=%v, cbCharge=%d", vehicleState, enabled, ready, lowSOC, seatboxOpen, cbCharge))
	}

	return nil
}

func (sm *BatteryStateMachine) actionCommandFailed(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.reader.Lock()
	sm.reader.data.Faults.CommunicationError = true
	sm.reader.Unlock()

	sm.logger(hal.LogLevelError, "Battery command failed")
	return sm.reader.updateRedisStatus()
}

func (sm *BatteryStateMachine) actionHALError(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.reader.Lock()
	sm.reader.data.Faults.ReaderError = true
	sm.reader.Unlock()

	sm.logger(hal.LogLevelError, "HAL error occurred")
	return sm.reader.updateRedisStatus()
}

func (sm *BatteryStateMachine) actionRecovery(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.reader.Lock()
	sm.reader.data.Faults.ReaderError = false
	sm.reader.data.Faults.CommunicationError = false
	sm.reader.data.Faults.NotFollowingCommand = false
	sm.reader.Unlock()

	sm.logger(hal.LogLevelInfo, "Recovery from error state")
	return sm.reader.updateRedisStatus()
}

func (sm *BatteryStateMachine) actionDisable(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.reader.Lock()
	sm.reader.enabled = false
	sm.reader.Unlock()

	sm.logger(hal.LogLevelInfo, "Battery reader disabled")
	return nil
}

func (sm *BatteryStateMachine) actionEnable(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.reader.Lock()
	sm.reader.enabled = true
	sm.reader.Unlock()

	sm.logger(hal.LogLevelInfo, "Battery reader enabled")
	return nil
}

func (sm *BatteryStateMachine) actionVehicleActiveWhileDisabled(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.logger(hal.LogLevelInfo, "Vehicle became active while battery disabled - checking if battery should be enabled")

	// Check if this is battery 0 (only battery 0 can be activated)
	if sm.reader.index != 0 {
		sm.logger(hal.LogLevelDebug, "Battery 1 remains disabled (only battery 0 can be activated)")
		return fmt.Errorf("battery 1 should not be enabled")
	}

	// Get current conditions
	sm.reader.service.Lock()
	vehicleState := sm.reader.service.vehicleState
	cbCharge := sm.reader.service.cbBatteryCharge
	seatboxOpen := sm.reader.service.seatboxOpen
	sm.reader.service.Unlock()

	sm.reader.Lock()
	batteryPresent := sm.reader.data.Present
	sm.reader.Unlock()

	// In parked mode, we should always enable battery 0 if it's present
	if vehicleState == "parked" && batteryPresent {
		sm.logger(hal.LogLevelInfo, fmt.Sprintf("Enabling Battery 0 due to parked state (present=%v, seatboxOpen=%v)", batteryPresent, seatboxOpen))

		// Enable the battery
		sm.reader.Lock()
		sm.reader.enabled = true
		sm.reader.Unlock()

		// Send EventEnabled to trigger proper initialization
		go func() {
			// Small delay to ensure state transition completes first
			time.Sleep(10 * time.Millisecond)
			sm.logger(hal.LogLevelDebug, "Sending EventEnabled after vehicle active")
			sm.SendEvent(EventEnabled)
		}()

		sm.logger(hal.LogLevelInfo, "Battery reader enabled")
		return nil // Transition to StateNotPresent
	}

	// For non-parked states, check normal activation conditions
	shouldEnable := batteryPresent && !seatboxOpen &&
		(vehicleState != "stand-by" || (cbCharge >= 0 && cbCharge < cbBatteryActivationThreshold))

	if shouldEnable {
		sm.logger(hal.LogLevelInfo, fmt.Sprintf("Enabling Battery 0 due to vehicle state '%s' (present=%v, seatboxOpen=%v, cbCharge=%d)", vehicleState, batteryPresent, seatboxOpen, cbCharge))

		// Enable the battery
		sm.reader.Lock()
		sm.reader.enabled = true
		sm.reader.Unlock()

		// Send EventEnabled to trigger proper initialization
		go func() {
			// Small delay to ensure state transition completes first
			time.Sleep(10 * time.Millisecond)
			sm.logger(hal.LogLevelDebug, "Sending EventEnabled after vehicle active")
			sm.SendEvent(EventEnabled)
		}()

		sm.logger(hal.LogLevelInfo, "Battery reader enabled")
		return nil // Transition to StateNotPresent
	}

	sm.logger(hal.LogLevelDebug, fmt.Sprintf("Battery 0 activation conditions not met (vehicleState=%s, present=%v, seatboxOpen=%v, cbCharge=%d)", vehicleState, batteryPresent, seatboxOpen, cbCharge))
	return fmt.Errorf("battery activation conditions not met")
}

func (sm *BatteryStateMachine) actionBatteryInsertedWhileDisabled(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.logger(hal.LogLevelInfo, "Battery inserted while disabled - checking if battery should be enabled")

	// Check if this is battery 0 (only battery 0 can be activated)
	if sm.reader.index != 0 {
		sm.logger(hal.LogLevelDebug, "Battery 1 remains disabled (only battery 0 can be activated)")
		// For battery 1, we don't enable it, so we should stay in disabled state
		// Return an error to prevent the transition to StateNotPresent
		return fmt.Errorf("battery 1 should not be enabled")
	}

	// Get current conditions
	sm.reader.service.Lock()
	vehicleState := sm.reader.service.vehicleState
	cbCharge := sm.reader.service.cbBatteryCharge
	seatboxOpen := sm.reader.service.seatboxOpen
	sm.reader.service.Unlock()

	sm.reader.Lock()
	batteryPresent := sm.reader.data.Present
	sm.reader.Unlock()

	// Check if conditions suggest the battery should be enabled
	// Enable for non-standby states (like "parked") when battery is present
	shouldEnable := batteryPresent && !seatboxOpen &&
		(vehicleState != "stand-by" || (cbCharge >= 0 && cbCharge < cbBatteryActivationThreshold))

	if shouldEnable {
		sm.logger(hal.LogLevelInfo, fmt.Sprintf("Enabling Battery 0 due to vehicle state '%s' (present=%v, seatboxOpen=%v, cbCharge=%d)", vehicleState, batteryPresent, seatboxOpen, cbCharge))

		// Enable the battery (this action sets enabled=true like actionEnable)
		sm.reader.Lock()
		sm.reader.enabled = true
		sm.reader.Unlock()

		// If battery is present, schedule a battery insertion event to trigger initialization
		if batteryPresent {
			go func() {
				// Small delay to ensure state transition completes first
				time.Sleep(10 * time.Millisecond)
				sm.logger(hal.LogLevelDebug, "Sending EventBatteryInserted after enabling")
				sm.SendEvent(EventBatteryInserted)
			}()
		}

		sm.logger(hal.LogLevelInfo, "Battery reader enabled")
		return nil // Transition to StateNotPresent
	} else {
		sm.logger(hal.LogLevelDebug, fmt.Sprintf("Battery 0 activation conditions not met (vehicleState=%s, present=%v, seatboxOpen=%v, cbCharge=%d)", vehicleState, batteryPresent, seatboxOpen, cbCharge))
		// Don't enable, stay in disabled state
		return fmt.Errorf("battery activation conditions not met")
	}
}

func (sm *BatteryStateMachine) actionLowSOCWhileActive(machine *BatteryStateMachine, event BatteryEvent) error {
	// Get current vehicle state to determine if we should deactivate
	sm.reader.service.Lock()
	vehicleState := sm.reader.service.vehicleState
	sm.reader.service.Unlock()

	sm.logger(hal.LogLevelWarning, fmt.Sprintf("Battery SOC is low while active (vehicleState=%s)", vehicleState))

	// Update Redis status with low SOC condition
	if err := sm.reader.updateRedisStatus(); err != nil {
		sm.logger(hal.LogLevelWarning, fmt.Sprintf("Failed to update Redis after low SOC: %v", err))
	}

	// Only deactivate if vehicle is in standby mode
	// During rides or in park mode, keep the battery active despite low SOC
	if vehicleState == "stand-by" {
		sm.logger(hal.LogLevelInfo, "Vehicle is in standby mode - deactivating battery due to low SOC")
		// Trigger deactivation by sending an event that will transition to StateDeactivating
		go func() {
			// Small delay to ensure current state transition completes
			time.Sleep(10 * time.Millisecond)
			sm.SendEvent(EventVehicleStandby)
		}()
	} else {
		sm.logger(hal.LogLevelInfo, fmt.Sprintf("Vehicle is in %s mode - keeping battery active despite low SOC", vehicleState))
	}

	return nil
}
