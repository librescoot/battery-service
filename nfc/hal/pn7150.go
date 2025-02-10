package hal

import (
	"encoding/hex"
	"fmt"
	"sync"
	"time"
	"golang.org/x/sys/unix"
)

const (
	nciBufferSize = 256
	maxTags       = 10
	maxRetries    = 3
	readTimeout   = 1 * time.Second
	maxUIDSize    = 10 // Maximum size of NFC tag UID
)

// State represents the state of the PN7150 HAL
type state int

const (
	stateUninitialized state = iota
	stateInitializing
	stateIdle
	stateDiscovering
	statePresent
)

// String returns the string representation of the state
func (s state) String() string {
	switch s {
	case stateUninitialized:
		return "Uninitialized"
	case stateInitializing:
		return "Initializing"
	case stateIdle:
		return "Idle"
	case stateDiscovering:
		return "Discovering"
	case statePresent:
		return "Present"
	default:
		return "Unknown"
	}
}

// PN7150 implements the HAL interface for the NXP PN7150 NFC controller
type PN7150 struct {
	sync.Mutex
	state         state
	fd            int
	logCallback   LogCallback
	app           interface{}
	txBuf         [256]byte
	txSize        int
	rxBuf         []byte
	standbyEnabled bool
	lpcdEnabled   bool
	tagSelected   bool
	numTags       int
	tags          []Tag
	verbose       bool
	paramWriteTry uint
	paramWriteTries uint
}

func NewPN7150(devName string, logCallback LogCallback, app interface{}, standbyEnabled, lpcdEnabled bool) (*PN7150, error) {
	fd, err := unix.Open(devName, unix.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open device %s: %v", devName, err)
	}

	hal := &PN7150{
		fd:            fd,
		logCallback:   logCallback,
		app:           app,
		rxBuf:         make([]byte, nciBufferSize),
		standbyEnabled: standbyEnabled,
		lpcdEnabled:   lpcdEnabled,
		tags:          make([]Tag, maxTags),
		verbose:       true, // Enable verbose logging by default
		paramWriteTries: 3,  // Maximum number of parameter write attempts
	}

	return hal, nil
}

// logNCI logs NCI messages with direction
func (p *PN7150) logNCI(buf []byte, size int, direction string) {
	if !p.verbose {
		return
	}

	hexStr := hex.EncodeToString(buf[:size])
	msg := fmt.Sprintf("NCI %s: %s", direction, hexStr)
	if p.logCallback != nil {
		p.logCallback(LogLevelDebug, msg)
	}
}

// Initialize implements HAL.Initialize
func (p *PN7150) Initialize() error {
	p.Lock()
	defer p.Unlock()

	if p.state != stateUninitialized {
		return fmt.Errorf("invalid state for initialization: %s", p.state)
	}

	p.state = stateInitializing
	if p.logCallback != nil {
		p.logCallback(LogLevelInfo, "Initializing PN7150")
	}

	// Power on the device
	if err := p.setPower(true); err != nil {
		return fmt.Errorf("failed to power on device: %v", err)
	}

	// Wait for device to stabilize after power up
	time.Sleep(100 * time.Millisecond)

	// Send Core Reset command with RF config reset
	resetCmd := buildCoreReset()
	resp, err := p.transfer(resetCmd)
	if err != nil {
		return fmt.Errorf("core reset failed: %v", err)
	}

	// Wait for reset notification
	time.Sleep(10 * time.Millisecond)

	// Send Core Init command
	initCmd := buildCoreInit()
	resp, err = p.transfer(initCmd)
	if err != nil {
		return fmt.Errorf("core init failed: %v", err)
	}

	// Extract firmware version
	if len(resp) >= 20 {
		hwVer := resp[17]
		romVer := resp[18]
		fwVerMajor := resp[19]
		fwVerMinor := resp[20]
		if p.logCallback != nil {
			p.logCallback(LogLevelInfo, fmt.Sprintf("Reader info: hw_version: %d, rom_version: %d, fw_version: %d.%d",
				hwVer, romVer, fwVerMajor, fwVerMinor))
		}
	}

	// Send NCI Proprietary Activation command
	propActCmd := []byte{
		0x2F, // MT=CMD (1 << 5), GID=Proprietary
		0x02, // OID=Proprietary Act
		0x00, // No payload
	}
	resp, err = p.transfer(propActCmd)
	if err != nil {
		return fmt.Errorf("proprietary activation failed: %v", err)
	}

	// Set initial parameters
	type nciParam struct {
		id    uint16
		value []byte
	}

	params := []nciParam{
		{0xA003, []byte{0x08}},                    // CLOCK_SEL_CFG: 27.12 MHz crystal
		{0xA00E, []byte{0x02, 0x09, 0x00}},       // PMU_CFG
	}

	// Set each parameter
	for _, param := range params {
		configCmd := []byte{
			0x20, // MT=CMD (1 << 5), GID=CORE
			0x02, // OID=SET_CONFIG
			0x04, // Length
			0x01, // Number of parameters
			byte(param.id >> 8),
			byte(param.id & 0xFF),
			byte(len(param.value)),
		}
		configCmd = append(configCmd, param.value...)
		configCmd[2] = byte(len(configCmd) - 3)

		resp, err = p.transfer(configCmd)
		if err != nil {
			return fmt.Errorf("parameter configuration failed: %v", err)
		}

		nciResp, err := parseNCIResponse(resp)
		if err != nil {
			return fmt.Errorf("failed to parse parameter response: %v", err)
		}

		if !isSuccessResponse(nciResp) {
			return fmt.Errorf("parameter configuration failed with status: %02x", nciResp.Status)
		}
	}

	// Set up RF transitions using CORE_SET_CONFIG
	type rfTransition struct {
		id     byte
		offset byte
		value  []byte
	}

	transitions := []rfTransition{
		{0x04, 0x35, []byte{0x90, 0x01, 0xf4, 0x01}},
		{0x06, 0x44, []byte{0x01, 0x90, 0x03, 0x00}},
		{0x06, 0x30, []byte{0xb0, 0x01, 0x10, 0x00}},
		{0x06, 0x42, []byte{0x02, 0x00, 0xff, 0xff}},
		{0x06, 0x3f, []byte{0x04}},
		{0x20, 0x42, []byte{0x88, 0x00, 0xff, 0xff}},
		{0x22, 0x44, []byte{0x23, 0x00}},
		{0x22, 0x2d, []byte{0x50, 0x34, 0x0c, 0x00}},
		{0x32, 0x42, []byte{0xf8, 0x00, 0xff, 0xff}},
		{0x34, 0x2d, []byte{0x24, 0x37, 0x0c, 0x00}},
		{0x34, 0x33, []byte{0x86, 0x80, 0x00, 0x70}},
		{0x34, 0x44, []byte{0x22, 0x00}},
		{0x42, 0x2d, []byte{0x15, 0x45, 0x0d, 0x00}},
		{0x46, 0x44, []byte{0x22, 0x00}},
		{0x46, 0x2d, []byte{0x05, 0x59, 0x0e, 0x00}},
		{0x44, 0x42, []byte{0x88, 0x00, 0xff, 0xff}},
		{0x56, 0x2d, []byte{0x05, 0x9f, 0x0c, 0x00}},
		{0x54, 0x42, []byte{0x88, 0x00, 0xff, 0xff}},
		{0x0a, 0x33, []byte{0x80, 0x86, 0x00, 0x70}},
	}

	// Build CORE_SET_CONFIG command
	configCmd := []byte{
		0x20, // MT=CMD (1 << 5), GID=CORE
		0x02, // OID=SET_CONFIG
		0x00, // Length placeholder
		byte(len(transitions)), // Number of parameters
	}

	// Add each transition parameter
	for _, t := range transitions {
		configCmd = append(configCmd,
			0xA0, // RF_TRANSITION_CFG >> 8
			0x0D, // RF_TRANSITION_CFG & 0xFF
			byte(2+len(t.value)), // Parameter length
			t.id,     // Transition ID
			t.offset, // Offset
		)
		configCmd = append(configCmd, t.value...)
	}

	// Update length
	configCmd[2] = byte(len(configCmd) - 3)

	// Send CORE_SET_CONFIG command
	resp, err = p.transfer(configCmd)
	if err != nil {
		return fmt.Errorf("RF transitions configuration failed: %v", err)
	}

	nciResp, err := parseNCIResponse(resp)
	if err != nil {
		return fmt.Errorf("failed to parse RF transitions response: %v", err)
	}

	if !isSuccessResponse(nciResp) {
		return fmt.Errorf("RF transitions configuration failed with status: %02x", nciResp.Status)
	}

	// Set up RF discovery map
	mapCmd := buildRFDiscoverMapCmd()

	resp, err = p.transfer(mapCmd)
	if err != nil {
		return fmt.Errorf("RF discover map failed: %v", err)
	}

	nciResp, err = parseNCIResponse(resp)
	if err != nil {
		return fmt.Errorf("failed to parse RF discover map response: %v", err)
	}

	if !isSuccessResponse(nciResp) {
		return fmt.Errorf("RF discover map failed with status: %02x", nciResp.Status)
	}

	p.state = stateIdle

	// Release the mutex before starting discovery
	p.Unlock()
	err = p.StartDiscovery(100)
	p.Lock() // Re-acquire the mutex
	if err != nil {
		return fmt.Errorf("failed to start discovery after initialization: %v", err)
	}

	return nil
}

// Helper function to convert bool to byte
func boolToByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}

// setPower controls the device power state through IOCTL
func (p *PN7150) setPower(on bool) error {
	if p.fd < 0 {
		return nil
	}
	if p.logCallback != nil {
		p.logCallback(LogLevelDebug, fmt.Sprintf("Set power: %v", on))
	}
	
	const pn5xxSetPwr = 0xE901
	
	var value uintptr
	if on {
		value = 1
	}
	
	// Call IOCTL using raw fd
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(p.fd),
		uintptr(pn5xxSetPwr),
		value,
	)
	
	if errno != 0 {
		return fmt.Errorf("ioctl error: %v", errno)
	}
	return nil
}

// checkParams verifies the device parameters
func (p *PN7150) checkParams() error {
	// TODO: Implement parameter checking
	return nil
}

// writeParams writes the device parameters
func (p *PN7150) writeParams() error {
	// TODO: Implement parameter writing
	return nil
}

// Deinitialize implements HAL.Deinitialize
func (p *PN7150) Deinitialize() {
	p.Lock()
	defer p.Unlock()

	if p.fd >= 0 {
		unix.Close(p.fd)
		p.fd = -1
	}

	p.state = stateUninitialized
	if p.logCallback != nil {
		p.logCallback(LogLevelInfo, "Deinitialized PN7150")
	}
}

// StartDiscovery implements HAL.StartDiscovery
func (p *PN7150) StartDiscovery(pollPeriod uint) error {
	p.Lock()
	defer p.Unlock()

	// First stop any existing discovery
	resp, err := p.transfer(buildRFDeactivateCmd())
	if err != nil {
		return fmt.Errorf("RF deactivate command failed: %v", err)
	}

	nciResp, err := parseNCIResponse(resp)
	if err != nil {
		return fmt.Errorf("failed to parse RF deactivate response: %v", err)
	}

	if !isSuccessResponse(nciResp) && nciResp.Status != nciStatusSemanticError {
		return fmt.Errorf("RF deactivate failed with status: %02x", nciResp.Status)
	}

	// Set poll period
	if pollPeriod > 2750 {
		return fmt.Errorf("invalid poll period: %d", pollPeriod)
	}

	// Start RF discovery
	discoverCmd := buildRFDiscoverCmd(pollPeriod)
	resp, err = p.transfer(discoverCmd)
	if err != nil {
		return fmt.Errorf("RF discover command failed: %v", err)
	}

	nciResp, err = parseNCIResponse(resp)
	if err != nil {
		return fmt.Errorf("failed to parse RF discover response: %v", err)
	}

	if !isSuccessResponse(nciResp) {
		return fmt.Errorf("RF discover failed with status: %02x", nciResp.Status)
	}

	p.state = stateDiscovering
	p.numTags = 0
	p.tagSelected = false

	if p.logCallback != nil {
		p.logCallback(LogLevelInfo, fmt.Sprintf("Started discovery with poll period %d ms", pollPeriod))
	}

	return nil
}

// StopDiscovery implements HAL.StopDiscovery
func (p *PN7150) StopDiscovery() error {
	p.Lock()
	defer p.Unlock()

	if p.state != stateDiscovering {
		return fmt.Errorf("invalid state for stopping discovery: %s", p.state)
	}

	resp, err := p.transfer(buildRFDeactivateCmd())
	if err != nil {
		return fmt.Errorf("RF deactivate command failed: %v", err)
	}

	nciResp, err := parseNCIResponse(resp)
	if err != nil {
		return fmt.Errorf("failed to parse RF deactivate response: %v", err)
	}

	if !isSuccessResponse(nciResp) {
		return fmt.Errorf("RF deactivate failed with status: %02x", nciResp.Status)
	}

	p.state = stateIdle
	if p.logCallback != nil {
		p.logCallback(LogLevelInfo, "Stopped discovery")
	}

	return nil
}

// GetState implements HAL.GetState
func (p *PN7150) GetState() State {
	p.Lock()
	defer p.Unlock()
	return State(p.state)
}

// DetectTags implements HAL.DetectTags
func (p *PN7150) DetectTags() ([]Tag, error) {
	p.Lock()
	defer p.Unlock()

	// Read any pending notifications
	resp, err := p.transfer(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to read notifications: %v", err)
	}

	// No notifications, return current tags
	if len(resp) == 0 {
		if p.state == statePresent && p.numTags > 0 {
			return p.tags[:p.numTags], nil
		}
		return nil, nil
	}

	// Parse NCI header
	if len(resp) < 3 {
		return nil, fmt.Errorf("incomplete NCI header")
	}

	mt := (resp[0] >> nciMsgTypeBit) & 0x03
	gid := resp[0] & 0x0F
	oid := resp[1] & 0x3F

	// Handle status notifications
	if mt == nciMsgTypeNotification && gid == nciGroupStatus {
		if p.logCallback != nil {
			p.logCallback(LogLevelDebug, fmt.Sprintf("Status notification received: %02x %02x", oid, resp[2]))
		}
		if p.state == statePresent && p.numTags > 0 {
			return p.tags[:p.numTags], nil
		}
		return nil, nil
	}

	// Only process RF notifications
	if mt != nciMsgTypeNotification || gid != nciGroupRF {
		if p.state == statePresent && p.numTags > 0 {
			return p.tags[:p.numTags], nil
		}
		return nil, nil
	}

	switch oid {
	case nciRFDiscoverOID:
		if len(resp) < 7 {
			return nil, fmt.Errorf("invalid RF_DISCOVER_NTF length")
		}

		rfProtocol := resp[4]
		rfTech := resp[5]

		// Only check for NFC-A passive poll mode technology
		if rfTech != nciRFTechNFCAPassivePoll {
			if p.logCallback != nil {
				p.logCallback(LogLevelDebug, fmt.Sprintf("Ignoring unsupported technology: tech=%02x", rfTech))
			}
			return nil, nil
		}

		// Store tag information
		if p.numTags < maxTags {
			tag := Tag{
				RFProtocol: RFProtocol(rfProtocol),
			}
			// Extract UID if present
			if len(resp) >= 10 && resp[9] <= maxUIDSize {
				tag.ID = make([]byte, resp[9])
				copy(tag.ID, resp[10:10+resp[9]])
			}
			p.tags[p.numTags] = tag
			p.numTags++
			if p.logCallback != nil {
				p.logCallback(LogLevelDebug, fmt.Sprintf("Tag discovered: protocol=%s, uid_len=%d, uid=%X", tag.RFProtocol, len(tag.ID), tag.ID))
			}
		}

		// Check if this is the last tag
		if resp[len(resp)-1] == 0x02 {
			// Not the last tag, keep waiting for more
			return nil, nil
		}

		// Tag is now present and selected
		p.state = statePresent
		p.tagSelected = true
		return p.tags[:p.numTags], nil

	case nciRFIntfActivatedOID:
		tag, err := parseRFIntfActivatedNtf(resp)
		if err != nil {
			return nil, err
		}
		// Update state and store tag
		p.state = statePresent
		p.numTags = 1
		p.tags[0] = *tag
		p.tagSelected = true
		return []Tag{*tag}, nil

	case nciRFDeactivateOID:
		if p.logCallback != nil {
			p.logCallback(LogLevelDebug, "Tag departure")
		}
		// Only transition to discovering if we're not in the middle of a read operation
		if p.state != statePresent || !p.tagSelected {
			p.numTags = 0
			p.state = stateDiscovering
			p.tagSelected = false
		}
	}

	if p.state == statePresent && p.numTags > 0 {
		return p.tags[:p.numTags], nil
	}
	return nil, nil
}

// ReadBinary implements HAL.ReadBinary
func (p *PN7150) ReadBinary(address uint16) ([]byte, error) {
	p.Lock()
	defer p.Unlock()

	if p.state != statePresent {
		return nil, fmt.Errorf("invalid state for reading: %s", p.state)
	}

	// Check if we have a tag and what protocol it is
	if p.numTags == 0 || !p.tagSelected || p.tags[0].RFProtocol == RFProtocolUnknown {
		return nil, fmt.Errorf("no valid tag present")
	}

	var cmd []byte
	if p.tags[0].RFProtocol == RFProtocolT2T {
		// T2T read command: 0x30 followed by block number
		cmd = []byte{0x30, byte(address >> 2)} // Convert address to block number (4 bytes per block)
	} else if p.tags[0].RFProtocol == RFProtocolISODEP {
		cmd = []byte{0x00, 0xB0, byte(address >> 8), byte(address & 0xFF), 0x02}
	} else {
		return nil, fmt.Errorf("unsupported protocol: %s", p.tags[0].RFProtocol)
	}

	// Send as DATA packet
	p.txBuf[0] = nciMsgTypeData << nciMsgTypeBit // DATA packet
	p.txBuf[1] = 0 // Connection ID
	p.txBuf[2] = byte(len(cmd)) // Payload length
	copy(p.txBuf[3:], cmd)
	p.txSize = 3 + len(cmd)

	if p.logCallback != nil {
		p.logCallback(LogLevelDebug, fmt.Sprintf("DATA_TX: %X", cmd))
	}

	// Add retries for RF frame corruption errors
	const maxRFRetries = 3
	var lastErr error

	for retry := 0; retry < maxRFRetries; retry++ {
		resp, err := p.transfer(p.txBuf[:p.txSize])
		if err != nil {
			lastErr = err
			// Only retry on temporary errors
			if err == unix.EINTR || err == unix.EAGAIN {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			// Fatal error, transition to discovering
			p.state = stateDiscovering
			p.tagSelected = false
			return nil, fmt.Errorf("read command failed: %v", err)
		}

		// We may receive multiple responses - keep reading until we get the actual data
		for {
			// Check for CORE_CONN_CREDITS_NTF
			if len(resp) >= 3 && resp[0] == 0x60 && resp[1] == 0x06 {
				// This is a credit notification, read the next response
				resp, err = p.transfer(nil)
				if err != nil {
					lastErr = err
					break
				}
				
				// If we get no response after credit notification, treat it as a communication issue
				if len(resp) == 0 {
					if p.logCallback != nil {
						p.logCallback(LogLevelDebug, "No response after credit notification - reinitializing")
					}
					
					// We need to unlock before calling StartDiscovery to avoid deadlock
					p.Unlock()
					err = p.StartDiscovery(100)
					p.Lock() // Re-acquire the lock
					
					if err != nil {
						lastErr = fmt.Errorf("failed to reinitialize after credit timeout: %v", err)
						break
					}

					// Wait for tag to be rediscovered and activated
					p.Unlock() // Release lock while waiting for discovery
					deadline := time.Now().Add(100 * time.Millisecond)
					var tags []Tag
					for time.Now().Before(deadline) {
						tags, err = p.DetectTags()
						if err == nil && len(tags) > 0 {
							break
						}
						time.Sleep(10 * time.Millisecond)
					}
					p.Lock() // Re-acquire lock
					
					if err != nil || len(tags) == 0 {
						lastErr = fmt.Errorf("failed to detect tag after credit timeout: %v", err)
						break
					}
					
					// Tag should now be activated and present
					if p.state != statePresent {
						lastErr = fmt.Errorf("tag not present after credit timeout")
						break
					}

					lastErr = fmt.Errorf("reinitialized after credit timeout")
					break
				}
				continue
			}

			// Check for special response codes
			if len(resp) >= 5 && resp[3] == 0x03 && resp[4] == 0x00 {
				// Got 0300 response - need to reinitialize
				if p.logCallback != nil {
					p.logCallback(LogLevelDebug, "Received 0300 response - reinitializing communication")
				}
				
				// We need to unlock before calling StartDiscovery to avoid deadlock
				p.Unlock()
				err = p.StartDiscovery(100)
				p.Lock() // Re-acquire the lock
				
				if err != nil {
					lastErr = fmt.Errorf("failed to reinitialize after 0300: %v", err)
					break
				}

				// Wait for tag to be rediscovered and activated
				p.Unlock() // Release lock while waiting for discovery
				deadline := time.Now().Add(500 * time.Millisecond)
				var tags []Tag
				for time.Now().Before(deadline) {
					tags, err = p.DetectTags()
					if err == nil && len(tags) > 0 {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				p.Lock() // Re-acquire lock
				
				if err != nil || len(tags) == 0 {
					lastErr = fmt.Errorf("failed to detect tag after reinitialization: %v", err)
					break
				}
				
				// Tag should now be activated and present
				if p.state != statePresent {
					lastErr = fmt.Errorf("tag not present after reinitialization")
					break
				}

				// Small delay to let the tag stabilize
				time.Sleep(10 * time.Millisecond)
				lastErr = fmt.Errorf("reinitialized after 0300")
				break
			}

			// For DATA packets, first 3 bytes are NCI header
			if len(resp) < 3 {
				lastErr = fmt.Errorf("response too short")
				break
			}

			mt := (resp[0] >> nciMsgTypeBit) & 0x03
			if mt != nciMsgTypeData {
				lastErr = fmt.Errorf("unexpected response type: %02x", mt)
				break
			}

			// Success - return the payload
			if p.logCallback != nil {
				p.logCallback(LogLevelDebug, fmt.Sprintf("DATA_RX: %X", resp[3:]))
			}
			return resp[3:], nil
		}

		// If we get here, we need to retry
		time.Sleep(10 * time.Millisecond)
	}

	// All retries failed
	if lastErr != nil {
		p.state = stateDiscovering
		p.tagSelected = false
		return nil, fmt.Errorf("read failed after %d retries: %v", maxRFRetries, lastErr)
	}

	return nil, fmt.Errorf("read failed with unknown error")
}

// WriteBinary implements HAL.WriteBinary
func (p *PN7150) WriteBinary(address uint16, data []byte) error {
	p.Lock()
	defer p.Unlock()

	if p.state != statePresent {
		return fmt.Errorf("invalid state for writing: %s", p.state)
	}

	// Check if we have a tag and what protocol it is
	if p.numTags == 0 || !p.tagSelected || p.tags[0].RFProtocol == RFProtocolUnknown {
		return fmt.Errorf("no valid tag present")
	}

	var cmd []byte
	if p.tags[0].RFProtocol == RFProtocolT2T {
		// T2T write command: 0xA2 followed by block number and data
		cmd = make([]byte, 6)
		cmd[0] = 0xA2                    // T2T WRITE command
		cmd[1] = byte(address >> 2)      // Convert address to block number (4 bytes per block)
		copy(cmd[2:], data)              // Copy the data (4 bytes)
	} else if p.tags[0].RFProtocol == RFProtocolISODEP {
		// For ISO-DEP, we need to send a different command
		// The command is: CLA=0x00, INS=0xD6 (UPDATE BINARY), P1=high byte, P2=low byte, Lc=len(data), Data
		cmd = make([]byte, 5+len(data))
		cmd[0] = 0x00  // CLA
		cmd[1] = 0xD6  // INS (UPDATE BINARY)
		cmd[2] = byte(address >> 8)    // P1 (high byte of address)
		cmd[3] = byte(address & 0xFF)  // P2 (low byte of address)
		cmd[4] = byte(len(data))       // Lc (length of data)
		copy(cmd[5:], data)
	} else {
		return fmt.Errorf("unsupported protocol: %s", p.tags[0].RFProtocol)
	}

	// Send as DATA packet
	p.txBuf[0] = nciMsgTypeData << nciMsgTypeBit // DATA packet
	p.txBuf[1] = 0 // Connection ID
	p.txBuf[2] = byte(len(cmd)) // Payload length
	copy(p.txBuf[3:], cmd)
	p.txSize = 3 + len(cmd)

	if p.logCallback != nil {
		p.logCallback(LogLevelDebug, fmt.Sprintf("DATA_TX: %X", cmd))
	}

	// Add retries for RF frame corruption errors
	const maxRFRetries = 3
	var lastErr error

	for retry := 0; retry < maxRFRetries; retry++ {
		resp, err := p.transfer(p.txBuf[:p.txSize])
		if err != nil {
			lastErr = err
			// Only retry on temporary errors
			if err == unix.EINTR || err == unix.EAGAIN {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			// Fatal error, transition to discovering
			p.state = stateDiscovering
			p.tagSelected = false
			return fmt.Errorf("write command failed: %v", err)
		}

		// We may receive multiple responses - keep reading until we get the actual ACK
		for {
			// Check for special response codes
			if len(resp) >= 5 && resp[3] == 0x03 && resp[4] == 0x00 {
				// Got 0300 response - need to reinitialize
				if p.logCallback != nil {
					p.logCallback(LogLevelDebug, "Received 0300 response - reinitializing communication")
				}
				
				// We need to unlock before calling StartDiscovery to avoid deadlock
				p.Unlock()
				err = p.StartDiscovery(100)
				p.Lock() // Re-acquire the lock
				
				if err != nil {
					lastErr = fmt.Errorf("failed to reinitialize after 0300: %v", err)
					break
				}

				// Wait for tag to be rediscovered and activated
				p.Unlock() // Release lock while waiting for discovery
				deadline := time.Now().Add(500 * time.Millisecond)
				var tags []Tag
				for time.Now().Before(deadline) {
					tags, err = p.DetectTags()
					if err == nil && len(tags) > 0 {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				p.Lock() // Re-acquire lock
				
				if err != nil || len(tags) == 0 {
					lastErr = fmt.Errorf("failed to detect tag after reinitialization: %v", err)
					break
				}
				
				// Tag should now be activated and present
				if p.state != statePresent {
					lastErr = fmt.Errorf("tag not present after reinitialization")
					break
				}

				// Small delay to let the tag stabilize
				time.Sleep(10 * time.Millisecond)
				lastErr = fmt.Errorf("reinitialized after 0300")
				break
			}

			// For T2T, we expect an ACK (0x0A) response
			if p.tags[0].RFProtocol == RFProtocolT2T {
				if len(resp) >= 4 && resp[3] == 0x0A {
					return nil
				}
			}

			// For ISO-DEP, check the response status
			if p.tags[0].RFProtocol == RFProtocolISODEP {
				if len(resp) >= 5 && resp[3] == 0x90 && resp[4] == 0x00 {
					return nil
				}
			}

			// Check if this is a CORE_CONN_CREDITS_NTF
			if len(resp) >= 3 && resp[0] == 0x60 && resp[1] == 0x06 {
				// This is a credit notification, read the next response
				resp, err = p.transfer(nil)
				if err != nil {
					lastErr = err
					break
				}
				
				// If we get no response after credit notification, treat it as a communication issue
				if len(resp) == 0 {
					if p.logCallback != nil {
						p.logCallback(LogLevelDebug, "No response after credit notification - reinitializing")
					}
					
					// We need to unlock before calling StartDiscovery to avoid deadlock
					p.Unlock()
					err = p.StartDiscovery(100)
					p.Lock() // Re-acquire the lock
					
					if err != nil {
						lastErr = fmt.Errorf("failed to reinitialize after credit timeout: %v", err)
						break
					}

					// Wait for tag to be rediscovered and activated
					p.Unlock() // Release lock while waiting for discovery
					deadline := time.Now().Add(200 * time.Millisecond)
					var tags []Tag
					for time.Now().Before(deadline) {
						tags, err = p.DetectTags()
						if err == nil && len(tags) > 0 {
							break
						}
						time.Sleep(10 * time.Millisecond)
					}
					p.Lock() // Re-acquire lock
					
					if err != nil || len(tags) == 0 {
						lastErr = fmt.Errorf("failed to detect tag after credit timeout: %v", err)
						break
					}
					
					// Tag should now be activated and present
					if p.state != statePresent {
						lastErr = fmt.Errorf("tag not present after credit timeout")
						break
					}

					lastErr = fmt.Errorf("reinitialized after credit timeout")
					break
				}
				continue
			}

			// If we get here, the response wasn't what we expected
			lastErr = fmt.Errorf("invalid response: %X", resp)
			break
		}

		// If we get here, we need to retry
		time.Sleep(10 * time.Millisecond)
	}

	// All retries failed
	p.state = stateDiscovering
	p.tagSelected = false
	if lastErr != nil {
		return fmt.Errorf("write failed after %d retries: %v", maxRFRetries, lastErr)
	}
	return fmt.Errorf("write failed with unknown error")
}

// GetFD implements HAL.GetFD
func (p *PN7150) GetFD() int {
	return p.fd
}

// flushReadBuffer reads and discards any pending data
func (p *PN7150) flushReadBuffer() error {
	buf := make([]byte, nciBufferSize)
	deadline := time.Now().Add(100 * time.Millisecond)

	for time.Now().Before(deadline) {
		_, err := unix.Read(p.fd, buf)
		if err != nil {
			if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
				// No more data available
				time.Sleep(time.Millisecond)
				continue
			}
			if err == unix.EINTR {
				continue
			}
			// Any other error means we're done
			return nil
		}
	}
	return nil
}

// transfer performs an NCI transfer operation
func (p *PN7150) transfer(tx []byte) ([]byte, error) {
	if tx != nil {
		if p.verbose {
			p.logNCI(tx, len(tx), "TX")
		}

		const i2cRetries = 10
		const i2cRetryTime = time.Millisecond

		for i := 0; i <= i2cRetries; i++ {
			n, err := unix.Write(p.fd, tx)
			if err != nil {
				if (err == unix.ENXIO || err == unix.EAGAIN) && i < i2cRetries {
					time.Sleep(i2cRetryTime)
					continue
				}
				return nil, fmt.Errorf("write error: %v", err)
			}
			
			if n != len(tx) {
				if i < i2cRetries {
					time.Sleep(i2cRetryTime)
					continue
				}
				return nil, fmt.Errorf("incomplete write: %d != %d", n, len(tx))
			}
			
			break
		}
	}

	// Read response or notifications
	pfd := unix.PollFd{
		Fd:     int32(p.fd),
		Events: unix.POLLIN,
	}

	readDeadline := time.Now().Add(readTimeout)

	for {
		if time.Now().After(readDeadline) {
			if tx == nil {
				return nil, nil // No notifications available
			}
			return nil, fmt.Errorf("read timeout")
		}

		timeout := int(time.Until(readDeadline) / time.Millisecond)
		if timeout < 1 {
			timeout = 1
		}

		n, err := unix.Poll([]unix.PollFd{pfd}, timeout)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return nil, fmt.Errorf("poll error: %v", err)
		}
		if n == 0 {
			if tx == nil {
				return nil, nil // No notifications available
			}
			continue // Keep waiting for response
		}

		// Read header first
		n, err = unix.Read(p.fd, p.rxBuf[:3])
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
				// No data available
				if tx == nil {
					return nil, nil // No notifications available
				}
				continue // Keep waiting for response
			}
			return nil, fmt.Errorf("read header error: %v", err)
		}

		if n == 0 {
			// Zero-length read - treat as no data available
			if tx == nil {
				return nil, nil // No notifications available
			}
			continue // Keep waiting for response
		}

		if n != 3 {
			return nil, fmt.Errorf("incomplete header read: %d", n)
		}

		// Basic validation
		mt := (p.rxBuf[0] >> nciMsgTypeBit) & 0x03
		pbf := p.rxBuf[0] & 0x10
		if mt > nciMsgTypeNotification || pbf != 0 {
			p.flushReadBuffer()
			return nil, fmt.Errorf("invalid NCI header")
		}

		payloadLen := int(p.rxBuf[2])
		if payloadLen > 0 {
			// Read payload
			n, err = unix.Read(p.fd, p.rxBuf[3:3+payloadLen])
			if err != nil {
				return nil, fmt.Errorf("read payload error: %v", err)
			}
			if n != payloadLen {
				return nil, fmt.Errorf("incomplete payload read: %d != %d", n, payloadLen)
			}
		}

		totalLen := 3 + payloadLen
		if p.verbose {
			p.logNCI(p.rxBuf[:totalLen], totalLen, "RX")
		}

		// Special case: If we sent a data packet (MT=0), expect a notification as response
		if tx != nil && mt == nciMsgTypeNotification && (tx[0] & 0xE0) == 0 {
			return p.rxBuf[:totalLen], nil
		}

		// For command responses
		if tx != nil && mt == nciMsgTypeResponse {
			return p.rxBuf[:totalLen], nil
		}

		// For notifications
		if mt == nciMsgTypeNotification {
			if tx == nil {
				return p.rxBuf[:totalLen], nil // Return notification when explicitly reading notifications
			}
			// If we're expecting a response, ignore the notification and keep reading
			continue
		}

		// For data messages
		if mt == nciMsgTypeData {
			return p.rxBuf[:totalLen], nil
		}
	}
}
