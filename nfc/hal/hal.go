package hal

import (
	"fmt"
)

// HAL represents the NFC Hardware Abstraction Layer interface
type HAL interface {
	// Initialize initializes the NFC controller
	Initialize() error

	// Deinitialize deinitializes the NFC controller
	Deinitialize()

	// FullReinitialize completely reinitializes the HAL including file descriptor renewal
	FullReinitialize() error

	// StartDiscovery starts RF discovery with the given poll period in milliseconds
	StartDiscovery(pollPeriod uint) error

	// StopDiscovery stops RF discovery
	StopDiscovery() error

	// DetectTags detects NFC tags in the field
	DetectTags() ([]Tag, error)

	// ReadBinary reads binary data from a tag at the given address
	ReadBinary(address uint16) ([]byte, error)

	// WriteBinary writes binary data to a tag at the given address
	WriteBinary(address uint16, data []byte) error

	// GetState returns the current state of the HAL
	GetState() State

	// GetTagEventChannel returns a channel that receives tag events
	GetTagEventChannel() <-chan TagEvent

	// GetFd returns the file descriptor for the NFC device
	GetFd() int
}

// State represents the state of the NFC controller
type State int

const (
	StateUninitialized State = iota
	StateInitializing
	StateIdle
	StateDiscovering
	StatePresent
)

// String returns a string representation of the state
func (s State) String() string {
	switch s {
	case StateUninitialized:
		return "Uninitialized"
	case StateInitializing:
		return "Initializing"
	case StateIdle:
		return "Idle"
	case StateDiscovering:
		return "Discovering"
	case StatePresent:
		return "Present"
	default:
		return "Unknown"
	}
}

// Error codes
const (
	ErrTagDeparted = -100
)

// Error represents an NFC error
type Error struct {
	Code    int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("NFC error %d: %s", e.Code, e.Message)
}

// NewError creates a new NFC error
func NewError(code int, message string) error {
	return &Error{
		Code:    code,
		Message: message,
	}
}
