package hal

// LogLevel represents the severity level of a log message
type LogLevel int

const (
	LogLevelNone LogLevel = iota
	LogLevelError
	LogLevelWarning
	LogLevelInfo
	LogLevelDebug
)

// String returns a string representation of the log level
func (l LogLevel) String() string {
	switch l {
	case LogLevelNone:
		return "NONE"
	case LogLevelError:
		return "ERROR"
	case LogLevelWarning:
		return "WARNING"
	case LogLevelInfo:
		return "INFO"
	case LogLevelDebug:
		return "DEBUG"
	default:
		return "UNKNOWN"
	}
}

// LogCallback is a function type for logging messages
type LogCallback func(level LogLevel, message string)

const (
	RFProtocolUnknown RFProtocol = 0x00
)

// String returns the string representation of the RF protocol
func (p RFProtocol) String() string {
	switch p {
	case RFProtocolISODEP:
		return "ISO14443-4"
	case RFProtocolT2T:
		return "T2T"
	default:
		return "Unknown"
	}
}

// Tag represents an NFC tag
type Tag struct {
	RFProtocol RFProtocol
	ID         []byte
}
