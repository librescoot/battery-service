package hal

// Error codes
const (
	// Application error codes
	ErrCodeTagDeparted  = -0x100
	ErrCodeMultipleTags = -0x104

	// Transient error codes
	ErrCodeArbiterBusy = -0x103

	// I2C error codes (0x200 range)
	ErrCodeI2CWrite   = -0x201
	ErrCodeI2CRead    = -0x202
	ErrCodeI2CPoll    = -0x203
	ErrCodeI2CTimeout = -0x204

	// NCI protocol error codes (0x300 range)
	ErrCodeNCIInvalidHeader   = -0x301
	ErrCodeNCIInvalidData     = -0x302
	ErrCodeNCIInvalidOID      = -0x303
	ErrCodeNCIIncompleteRead  = -0x304
	ErrCodeNCIIncompleteMsg   = -0x305
	ErrCodeNCIUnexpectedReset = -0x306
)

// NFCError is the base interface for all NFC-related errors
type NFCError interface {
	error
	IsNFCError() bool
	Code() int
}

// HALError represents hardware-level errors that require recovery
type HALError interface {
	NFCError
	IsHALError() bool
}

// I2CError represents I2C communication errors (subclass of HALError)
type I2CError interface {
	HALError
	IsI2CError() bool
}

// NCIError represents NCI protocol errors (subclass of HALError)
type NCIError interface {
	HALError
	IsNCIError() bool
}

// TransientError represents temporary errors that can be retried
type TransientError interface {
	NFCError
	IsTransientError() bool
}

// ApplicationError represents expected conditions that should be handled at the application level
type ApplicationError interface {
	NFCError
	IsApplicationError() bool
}

// baseError provides common functionality for all error types
type baseError struct {
	code    int
	message string
}

func (e *baseError) Error() string {
	return e.message
}

func (e *baseError) Code() int {
	return e.code
}

func (e *baseError) IsNFCError() bool {
	return true
}

// i2cError represents I2C communication errors
type i2cError struct {
	baseError
	cause error
}

func (e *i2cError) IsHALError() bool {
	return true
}

func (e *i2cError) IsI2CError() bool {
	return true
}

func (e *i2cError) Unwrap() error {
	return e.cause
}

// nciError represents NCI protocol errors
type nciError struct {
	baseError
	cause error
}

func (e *nciError) IsHALError() bool {
	return true
}

func (e *nciError) IsNCIError() bool {
	return true
}

func (e *nciError) Unwrap() error {
	return e.cause
}

// I2C error constructors

func NewI2CWriteError(message string, cause error) error {
	return &i2cError{
		baseError: baseError{code: ErrCodeI2CWrite, message: message},
		cause:     cause,
	}
}

func NewI2CReadError(message string, cause error) error {
	return &i2cError{
		baseError: baseError{code: ErrCodeI2CRead, message: message},
		cause:     cause,
	}
}

func NewI2CPollError(message string, cause error) error {
	return &i2cError{
		baseError: baseError{code: ErrCodeI2CPoll, message: message},
		cause:     cause,
	}
}

func NewI2CTimeoutError(message string) error {
	return &i2cError{
		baseError: baseError{code: ErrCodeI2CTimeout, message: message},
		cause:     nil,
	}
}

// NCI error constructors

func NewNCIInvalidHeaderError(message string) error {
	return &nciError{
		baseError: baseError{code: ErrCodeNCIInvalidHeader, message: message},
		cause:     nil,
	}
}

func NewNCIInvalidDataError(message string) error {
	return &nciError{
		baseError: baseError{code: ErrCodeNCIInvalidData, message: message},
		cause:     nil,
	}
}

func NewNCIInvalidOIDError(message string) error {
	return &nciError{
		baseError: baseError{code: ErrCodeNCIInvalidOID, message: message},
		cause:     nil,
	}
}

func NewNCIIncompleteReadError(message string) error {
	return &nciError{
		baseError: baseError{code: ErrCodeNCIIncompleteRead, message: message},
		cause:     nil,
	}
}

func NewNCIIncompleteMsgError(message string) error {
	return &nciError{
		baseError: baseError{code: ErrCodeNCIIncompleteMsg, message: message},
		cause:     nil,
	}
}

func NewNCIUnexpectedResetError(message string) error {
	return &nciError{
		baseError: baseError{code: ErrCodeNCIUnexpectedReset, message: message},
		cause:     nil,
	}
}

// transientError represents temporary errors that should be retried
type transientError struct {
	baseError
}

func (e *transientError) IsTransientError() bool {
	return true
}

// NewTransientError creates a new transient error
func NewTransientError(message string) error {
	return &transientError{
		baseError: baseError{message: message},
	}
}

// applicationError represents expected application-level conditions
type applicationError struct {
	baseError
}

func (e *applicationError) IsApplicationError() bool {
	return true
}

// NewApplicationError creates a new application error
func NewApplicationError(message string) error {
	return &applicationError{
		baseError: baseError{message: message},
	}
}

// Specific error types

// TagDepartedError indicates the NFC tag has been removed
type TagDepartedError struct {
	applicationError
}

func NewTagDepartedError(message string) error {
	return &TagDepartedError{
		applicationError: applicationError{
			baseError: baseError{code: ErrCodeTagDeparted, message: message},
		},
	}
}

// ArbiterBusyError indicates the NTAG arbiter is locked to the I2C interface
type ArbiterBusyError struct {
	transientError
}

func NewArbiterBusyError(message string) error {
	return &ArbiterBusyError{
		transientError: transientError{
			baseError: baseError{code: ErrCodeArbiterBusy, message: message},
		},
	}
}

// MultipleTagsError indicates multiple tags detected when only one expected
type MultipleTagsError struct {
	applicationError
}

func NewMultipleTagsError(message string) error {
	return &MultipleTagsError{
		applicationError: applicationError{
			baseError: baseError{code: ErrCodeMultipleTags, message: message},
		},
	}
}

// Helper functions for error type checking

// IsHALError checks if an error is a HAL error requiring reinitialization
func IsHALError(err error) bool {
	if err == nil {
		return false
	}
	halErr, ok := err.(HALError)
	return ok && halErr.IsHALError()
}

// IsI2CError checks if an error is an I2C communication error
func IsI2CError(err error) bool {
	if err == nil {
		return false
	}
	i2cErr, ok := err.(I2CError)
	return ok && i2cErr.IsI2CError()
}

// IsNCIError checks if an error is an NCI protocol error
func IsNCIError(err error) bool {
	if err == nil {
		return false
	}
	nciErr, ok := err.(NCIError)
	return ok && nciErr.IsNCIError()
}

// IsTransientError checks if an error is transient and can be retried
func IsTransientError(err error) bool {
	if err == nil {
		return false
	}
	transErr, ok := err.(TransientError)
	return ok && transErr.IsTransientError()
}

// IsApplicationError checks if an error is an expected application-level condition
func IsApplicationError(err error) bool {
	if err == nil {
		return false
	}
	appErr, ok := err.(ApplicationError)
	return ok && appErr.IsApplicationError()
}

// IsTagDepartedError checks if an error indicates tag departure
func IsTagDepartedError(err error) bool {
	if err == nil {
		return false
	}
	_, ok := err.(*TagDepartedError)
	return ok
}

// IsArbiterBusyError checks if an error indicates arbiter is busy
func IsArbiterBusyError(err error) bool {
	if err == nil {
		return false
	}
	_, ok := err.(*ArbiterBusyError)
	return ok
}

// IsMultipleTagsError checks if an error indicates multiple tags detected
func IsMultipleTagsError(err error) bool {
	if err == nil {
		return false
	}
	_, ok := err.(*MultipleTagsError)
	return ok
}
