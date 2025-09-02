package battery

// BatteryRole defines the role of a battery reader
type BatteryRole string

const (
	// BatteryRoleActive is for batteries that need to be activated and monitored frequently
	// Multiple batteries can have this role and will all be activated
	BatteryRoleActive BatteryRole = "active"

	// BatteryRoleInactive is for batteries that only need periodic maintenance polling
	BatteryRoleInactive BatteryRole = "inactive"
)

// BatteryReaderConfig defines the configuration for each battery reader
type BatteryReaderConfig struct {
	// Index is the reader index (0 or 1)
	Index int

	// Role defines whether this is an active or inactive battery
	Role BatteryRole

	// Enabled determines if this reader should be started
	Enabled bool

	// DeviceName is the NFC device path
	DeviceName string

	// LogLevel for this specific reader
	LogLevel int
}

// BatteryConfiguration holds the configuration for all battery readers
type BatteryConfiguration struct {
	Readers []BatteryReaderConfig
}

// IsActive returns true if the battery reader has an active role
func (r *BatteryReader) IsActive() bool {
	return r.role == BatteryRoleActive
}

// IsInactive returns true if the battery reader has an inactive role
func (r *BatteryReader) IsInactive() bool {
	return r.role == BatteryRoleInactive
}
