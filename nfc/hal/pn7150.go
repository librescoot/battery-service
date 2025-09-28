package hal

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	nciBufferSize = 256
	maxTags       = 10
	maxRetries    = 3
	readTimeout   = 250 * time.Millisecond
	maxUIDSize    = 10 // Maximum size of NFC tag UID

	i2cMaxRetries     = 10
	i2cRetryTimeUs    = 1000 // microseconds
	paramCheckRetries = 3
	maxTotalDuration  = 2750 // ms
)

type state int

const (
	stateUninitialized state = iota
	stateInitializing
	stateIdle
	stateDiscovering
	statePresent
)

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

type PN7150 struct {
	mutex               sync.Mutex
	state               state
	fd                  int
	devicePath          string
	logCallback         LogCallback
	txBuf               [256]byte
	txSize              int
	rxBuf               []byte
	tagSelected         bool
	numTags             int
	tags                []Tag
	debug               bool
	standbyEnabled      bool
	lpcdEnabled         bool // Enable Low Power Card Detection
	transitionTableSent bool

	tagEventChan          chan TagEvent
	tagEventReaderStop    chan struct{}
	tagEventReaderRunning bool
	detectionFailures     int
}

func NewPN7150(devName string, logCallback LogCallback, app interface{}, standbyEnabled, lpcdEnabled bool, debugMode bool) (*PN7150, error) {
	fd, err := unix.Open(devName, unix.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open device %s: %v", devName, err)
	}

	hal := &PN7150{
		fd:                 fd,
		devicePath:         devName, // Store the device path
		logCallback:        logCallback,
		rxBuf:              make([]byte, nciBufferSize),
		tags:               make([]Tag, maxTags),
		debug:              debugMode,
		standbyEnabled:     standbyEnabled,
		lpcdEnabled:        lpcdEnabled,
		tagEventChan:       make(chan TagEvent, 10),
		tagEventReaderStop: make(chan struct{}),
	}

	return hal, nil
}

func (p *PN7150) logNCI(buf []byte, size int, direction string) {
	if !p.debug {
		return
	}

	hexStr := hex.EncodeToString(buf[:size])
	msg := fmt.Sprintf("NCI %s: %s", direction, hexStr)
	if p.logCallback != nil {
		p.logCallback(LogLevelDebug, msg)
	}
}

func (p *PN7150) Initialize() error {
	if p.state != stateUninitialized {
		return fmt.Errorf("invalid state for initialization: %s", p.state)
	}

	p.state = stateInitializing
	if p.logCallback != nil {
		p.logCallback(LogLevelInfo, "Initializing PN7150")
	}

	if err := p.SetPower(true); err != nil {
		return fmt.Errorf("failed to power on device: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	const maxInitRetries = 3
	var lastErr error
	var resp []byte
	var err error

	for initRetry := 0; initRetry < maxInitRetries; initRetry++ {
		if initRetry > 0 {
			if p.logCallback != nil {
				p.logCallback(LogLevelWarning, fmt.Sprintf("Initialization retry %d/%d", initRetry+1, maxInitRetries))
			}
			time.Sleep(100 * time.Millisecond)
		}

		resetCmd := buildCoreReset()
		_, err = p.transfer(resetCmd)
		if err != nil {
			lastErr = fmt.Errorf("core reset failed: %v", err)
			continue
		}

		time.Sleep(10 * time.Millisecond)

		initCmd := buildCoreInit()
		resp, err = p.transfer(initCmd)
		if err != nil {
			lastErr = fmt.Errorf("core init failed: %v", err)
			continue
		}

		lastErr = nil

		if len(resp) >= 28 {
			if p.logCallback != nil {
				p.logCallback(LogLevelInfo, fmt.Sprintf("Core Init response bytes: %x", resp))
			}

			// Parse version from Core Init response
			// Raw response: 40011900031e030008000102038081828302d002ff020004881001a0
			hwVer := resp[24]      // hw_version: 136 (0x88)
			romVer := resp[25]     // rom_version: 16 (0x10)
			fwVerMajor := resp[26] // fw_version major: 1 (0x01)
			fwVerMinor := resp[27] // fw_version minor: 160 (0xA0)
			if p.logCallback != nil {
				p.logCallback(LogLevelInfo, fmt.Sprintf("Reader info: hw_version: %d, rom_version: %d, fw_version: %d.%d",
					hwVer, romVer, fwVerMajor, fwVerMinor))
			}
		}
		break
	}

	if lastErr != nil {
		return lastErr
	}

	propActCmd := []byte{
		0x2F, // MT=CMD (1 << 5), GID=Proprietary
		0x02, // OID=Proprietary Act
		0x00, // No payload
	}
	_, err = p.transfer(propActCmd)
	if err != nil {
		return fmt.Errorf("proprietary activation failed: %v", err)
	}

	time.Sleep(30 * time.Millisecond)

	var powerMode byte = 0x00
	if p.standbyEnabled {
		powerMode = 0x01
	}
	propPowerCmd := []byte{
		0x2F,                          // MT=CMD (1 << 5), GID=Proprietary
		nciProprietarySetPowerModeOID, // OID=Set Power Mode
		0x01,                          // Payload length
		powerMode,                     // Power mode: 0=disabled, 1=standby enabled
	}
	_, err = p.transfer(propPowerCmd)
	if err != nil {
		return fmt.Errorf("proprietary power setting failed: %v", err)
	}

	if p.logCallback != nil {
		p.logCallback(LogLevelInfo, fmt.Sprintf("Set power mode: standby=%v, lpcd=%v", p.standbyEnabled, p.lpcdEnabled))
	}

	type nciParam struct {
		id    uint16
		value []byte
	}

	params := []nciParam{
		{0xA003, []byte{0x08}},             // CLOCK_SEL_CFG: 27.12 MHz crystal
		{0xA00E, []byte{0x02, 0x09, 0x00}}, // PMU_CFG
		{0xA040, []byte{0x01}},             // TAG_DETECTOR_CFG
	}

	needsParamWrite := false
	for _, param := range params {
		err := p.checkParam(param.id, param.value)
		if err != nil {
			needsParamWrite = true
			if p.logCallback != nil {
				p.logCallback(LogLevelInfo, fmt.Sprintf("Parameter 0x%04X needs update", param.id))
			}
			break
		}
	}

	if needsParamWrite {
		if p.logCallback != nil {
			p.logCallback(LogLevelWarning, "Writing NFC parameters")
		}

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

		for _, param := range params {
			err := p.checkParam(param.id, param.value)
			if err != nil {
				return fmt.Errorf("parameter verification failed after write: %v", err)
			}
		}
	}

	if !p.transitionTableSent {
		if p.logCallback != nil {
			p.logCallback(LogLevelInfo, "Sending RF transition table for first time")
		}

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

		configCmd := []byte{
			0x20,                   // MT=CMD (1 << 5), GID=CORE
			0x02,                   // OID=SET_CONFIG
			0x00,                   // Length placeholder
			byte(len(transitions)), // Number of parameters
		}

		for _, t := range transitions {
			configCmd = append(configCmd,
				0xA0,                 // RF_TRANSITION_CFG >> 8
				0x0D,                 // RF_TRANSITION_CFG & 0xFF
				byte(2+len(t.value)), // Parameter length
				t.id,                 // Transition ID
				t.offset,             // Offset
			)
			configCmd = append(configCmd, t.value...)
		}

		configCmd[2] = byte(len(configCmd) - 3)

		resp, err := p.transfer(configCmd)
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

		if p.logCallback != nil {
			p.logCallback(LogLevelDebug, "Verifying RF transitions")
		}
		for _, t := range transitions {
			err := p.checkRFTransition(t.id, t.offset, t.value)
			if err != nil {
				return fmt.Errorf("RF transition verification failed: %v", err)
			}
		}

		p.transitionTableSent = true

		if p.logCallback != nil {
			p.logCallback(LogLevelInfo, "RF transition table sent and verified successfully - will be skipped on future initializations")
		}
	}

	mapCmd := buildRFDiscoverMapCmd()

	resp, err = p.transfer(mapCmd)
	if err != nil {
		return fmt.Errorf("RF discover map failed: %v", err)
	}

	nciResp, err := parseNCIResponse(resp)
	if err != nil {
		return fmt.Errorf("failed to parse RF discover map response: %v", err)
	}

	if !isSuccessResponse(nciResp) {
		return fmt.Errorf("RF discover map failed with status: %02x", nciResp.Status)
	}

	p.state = stateIdle

	return nil
}

// SetPower controls the device power state through IOCTL
func (p *PN7150) SetPower(on bool) error {
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

// Deinitialize implements HAL.Deinitialize
func (p *PN7150) Deinitialize() {

	if p.tagEventReaderRunning {
		p.tagEventReaderRunning = false
		close(p.tagEventReaderStop)
		time.Sleep(50 * time.Millisecond)
		p.tagEventReaderStop = make(chan struct{})
	}

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

	if pollPeriod > maxTotalDuration {
		if p.logCallback != nil {
			p.logCallback(LogLevelError, fmt.Sprintf("start discovery: invalid poll_period: %d", pollPeriod))
		}
		return fmt.Errorf("invalid poll period: %d (max %d)", pollPeriod, maxTotalDuration)
	}

	resp, err := p.transfer(buildRFDeactivateCmd())
	if err != nil {
		return fmt.Errorf("RF deactivate command failed: %v", err)
	}

	nciResp, err := parseNCIResponse(resp)
	if err != nil {
		return fmt.Errorf("failed to parse RF deactivate response: %v", err)
	}

	// Accept semantic error (means discovery was already stopped)
	if !isSuccessResponse(nciResp) && nciResp.Status != nciStatusSemanticError {
		return fmt.Errorf("RF deactivate failed with status: %02x", nciResp.Status)
	}

	p.logCallback(LogLevelDebug, fmt.Sprintf("StartDiscovery: poll_period=%dms", pollPeriod))

	totalDurationPayload := []byte{
		byte(pollPeriod & 0xFF),        // LSB
		byte((pollPeriod >> 8) & 0xFF), // MSB
	}
	p.logCallback(LogLevelDebug, fmt.Sprintf("Setting TOTAL_DURATION (0x0000) to %X (%d ms)", totalDurationPayload, pollPeriod))
	totalDurationConfigCmd := []byte{
		(nciMsgTypeCommand << nciMsgTypeBit) | nciGroupCore, // 20
		nciCoreSetConfig,              // 02
		0x05,                          // Payload length: 1 (NumItems) + 1 (ID) + 1 (Len) + 2 (Value) = 5
		0x01,                          // Number of Parameter TLVs = 1
		byte(nciParamIDTotalDuration), // Parameter ID (0x00 for TOTAL_DURATION)
		0x02,                          // Parameter Length (2 bytes for uint16)
		totalDurationPayload[0],       // Value LSB
		totalDurationPayload[1],       // Value MSB
	}
	respTotalDuration, errTotalDuration := p.transfer(totalDurationConfigCmd)
	if errTotalDuration != nil {
		return fmt.Errorf("failed to set TOTAL_DURATION: %v", errTotalDuration)
	}
	nciRespTD, errParseTD := parseNCIResponse(respTotalDuration)
	if errParseTD != nil || !isSuccessResponse(nciRespTD) {
		errMsgTD := "set TOTAL_DURATION response error"
		if errParseTD != nil {
			errMsgTD += fmt.Sprintf(": %v", errParseTD)
		}
		if nciRespTD != nil {
			errMsgTD += fmt.Sprintf(" (status: %02x)", nciRespTD.Status)
		}
		return fmt.Errorf(errMsgTD)
	}
	p.logCallback(LogLevelDebug, "TOTAL_DURATION set successfully.")

	// Start RF discovery
	discoverCmd := buildRFDiscoverCmd()
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

func (p *PN7150) GetState() State {
	return State(p.state)
}

// DetectTags implements HAL.DetectTags
func (p *PN7150) DetectTags() ([]Tag, error) {
	resp, err := p.transfer(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to read notifications: %v", err)
	}

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

			// Extra log info after reinitialization
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
		// When a deactivation notification is received, it means no tag is currently active.
		// Always clear current tag information and transition to discovering state.
		p.numTags = 0
		p.state = stateDiscovering
		p.tagSelected = false
	}

	if p.state == statePresent && p.numTags > 0 {
		return p.tags[:p.numTags], nil
	}
	return nil, nil
}

// FullReinitialize completely reinitializes the PN7150 HAL from scratch
// This should be called when communication is severely broken and
// simple discovery restarts don't resolve the issue
// Caller must NOT hold the lock when calling this
func (p *PN7150) FullReinitialize() error {
	// Fast path: if we are already in the middle of an initialization, just return.
	// Note: We don't skip when state is Uninitialized because that's exactly when
	// we need to reinitialize (e.g., after power down or file descriptor issues)
	if p.state == stateInitializing {
		return nil
	}

	if p.logCallback != nil {
		p.logCallback(LogLevelInfo, "Performing full HAL reinitialization with power cycle")
	}

	if p.tagEventReaderRunning {
		p.tagEventReaderRunning = false
		close(p.tagEventReaderStop)
		time.Sleep(50 * time.Millisecond)
	}

	if err := p.SetPower(false); err != nil {
		if p.logCallback != nil {
			p.logCallback(LogLevelWarning, fmt.Sprintf("Error powering off during reinit: %v", err))
		}
	}

	// Remember the device path and close the current file descriptor (if any).
	devicePath := p.devicePath
	if p.fd >= 0 {
		unix.Close(p.fd)
		p.fd = -1
	}

	// Reset the internal state so that a fresh call to Initialize() can run.
	p.state = stateUninitialized
	p.numTags = 0
	p.tagSelected = false

	if p.logCallback != nil {
		p.logCallback(LogLevelInfo, "HAL FullReinitialize: sleeping before device reopen")
	}
	time.Sleep(250 * time.Millisecond)
	if p.logCallback != nil {
		p.logCallback(LogLevelInfo, "HAL FullReinitialize: sleep completed, reopening device")
	}

	// Re-open the device.
	fd, err := unix.Open(devicePath, unix.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("failed to reopen NFC device: %w", err)
	}

	// Store the new file descriptor.
	p.fd = fd
	// Recreate channels and stop channel for tag event reader
	p.tagEventChan = make(chan TagEvent, 10)
	p.tagEventReaderStop = make(chan struct{})

	// Re-run the normal initialization sequence that is already proven to
	// work at start-up.  All state transitions and discovery start are
	// handled inside Initialize().
	if err := p.Initialize(); err != nil {
		return fmt.Errorf("reinitialization failed: %w", err)
	}

	if p.logCallback != nil {
		p.logCallback(LogLevelInfo, "HAL reinitialization completed successfully with power cycle")
	}

	return nil
}

// ReadBinary implements HAL.ReadBinary
func (p *PN7150) ReadBinary(address uint16) ([]byte, error) {

	if p.state != statePresent {
		return nil, fmt.Errorf("invalid state for reading: %s", p.state)
	}

	if p.state == stateDiscovering {
		if p.logCallback != nil {
			p.logCallback(LogLevelDebug, "Stopping discovery before read operation")
		}
		resp, err := p.transfer(buildRFDeactivateCmd())
		if err == nil {
			nciResp, _ := parseNCIResponse(resp)
			if nciResp != nil && (isSuccessResponse(nciResp) || nciResp.Status == nciStatusSemanticError) {
				p.state = stateIdle
			}
		}
	}

	// Check if we have a tag and what protocol it is
	if p.numTags == 0 || !p.tagSelected || p.tags[0].RFProtocol == RFProtocolUnknown {
		return nil, fmt.Errorf("no valid tag present")
	}

	// Save tag info before potentially releasing lock
	protocol := p.tags[0].RFProtocol

	var cmd []byte
	switch protocol {
	case RFProtocolT2T:
		// T2T read command: 0x30 followed by block number
		blockNum := byte(address >> 2) // Convert address to block number (4 bytes per block)
		cmd = []byte{0x30, blockNum}
	case RFProtocolISODEP:
		cmd = []byte{0x00, 0xB0, byte(address >> 8), byte(address & 0xFF), 0x02}
	default:
		return nil, fmt.Errorf("unsupported protocol: %s", protocol)
	}

	// Send as DATA packet
	p.txBuf[0] = nciMsgTypeData << nciMsgTypeBit // DATA packet
	p.txBuf[1] = 0                               // Connection ID
	p.txBuf[2] = byte(len(cmd))                  // Payload length
	copy(p.txBuf[3:], cmd)
	p.txSize = 3 + len(cmd)

	if p.debug {
		if p.logCallback != nil {
			p.logCallback(LogLevelDebug, fmt.Sprintf("DATA_TX: %X", cmd))
		}
	}

	// Add retries for RF frame corruption errors
	const maxRFRetries = 4
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
			// Handle serious I/O errors with reinitialization
			return nil, p.handleSeriousErrorWithReinit(err, "read command")
		}

		// We may receive multiple responses - keep reading until we get the actual data
		for {
			// Check for CORE_CONN_CREDITS_NTF
			if len(resp) >= 3 && resp[0] == 0x60 && resp[1] == 0x06 {
				resp, err = p.handleCreditNotification()
				if err != nil {
					lastErr = err
					break
				}
				continue
			}

			// Check for special response codes - NTAG arbiter busy
			if len(resp) >= 5 && resp[3] == 0x03 {
				// 0x03 = NTAG arbiter busy (locked to I2C interface)
				// Always treat as arbiter busy since there are no other 0x03 errors

				shouldContinue, err := p.handle0300Error("read")
				if !shouldContinue {
					lastErr = err
					break
				}
				lastErr = err
				// Break to outer retry loop to re-attempt the read operation
				break
			}

			// For DATA packets, first 3 bytes are NCI header
			if len(resp) < 3 {
				lastErr = fmt.Errorf("response too short")
				break
			}

			mt := (resp[0] >> nciMsgTypeBit) & 0x03

			// Check for arbiter busy in message type field as well
			if mt == 0x03 {
				shouldContinue, err := p.handle0300Error("read")
				if !shouldContinue {
					lastErr = err
					break
				}
				lastErr = err
				// Break to outer retry loop to re-attempt the read operation
				break
			}

			if mt != nciMsgTypeData {
				lastErr = fmt.Errorf("unexpected response type: %02x", mt)
				break
			}

			// Success - return the payload
			if p.debug {
				if p.logCallback != nil {
					p.logCallback(LogLevelDebug, fmt.Sprintf("DATA_RX: %X", resp[3:]))
				}
			}
			result := make([]byte, len(resp)-3)
			copy(result, resp[3:])
			return result, nil
		}

		// If we broke from inner loop with arbiter busy (lastErr != nil but reselection succeeded), retry
		if lastErr != nil && strings.Contains(lastErr.Error(), "arbiter busy") {
			continue
		}

		// If we broke from inner loop with no error (soft recovery), continue retry
		if lastErr == nil {
			continue
		}

		// Otherwise we have an error, add delay and retry
		time.Sleep(10 * time.Millisecond)
	}

	if lastErr != nil {
		// Check if this is a tag departure error first
		if nfcErr, ok := lastErr.(*Error); ok && nfcErr.Code == ErrTagDeparted {
			return nil, lastErr // Return tag departure error directly without HAL reinitialization
		}
		// For serious errors (not 0300, timeout, or tag departure), perform full reinitialization
		if !strings.Contains(lastErr.Error(), "0300") && !strings.Contains(lastErr.Error(), "timeout") && !strings.Contains(lastErr.Error(), "reinitialization") && !strings.Contains(lastErr.Error(), "tag departed") {
			finalErr := p.handleSeriousErrorWithReinit(lastErr, "read after retries")
			return nil, fmt.Errorf("read failed after %d retries: %v", maxRFRetries, finalErr)
		}
		return nil, fmt.Errorf("read failed after %d retries: %v", maxRFRetries, lastErr)
	}

	return nil, fmt.Errorf("read failed with unknown error")
}

// WriteBinary implements HAL.WriteBinary
func (p *PN7150) WriteBinary(address uint16, data []byte) error {

	if p.state != statePresent {
		return fmt.Errorf("invalid state for writing: %s", p.state)
	}

	if p.state == stateDiscovering {
		if p.logCallback != nil {
			p.logCallback(LogLevelDebug, "Stopping discovery before write operation")
		}
		resp, err := p.transfer(buildRFDeactivateCmd())
		if err == nil {
			nciResp, _ := parseNCIResponse(resp)
			if nciResp != nil && (isSuccessResponse(nciResp) || nciResp.Status == nciStatusSemanticError) {
				p.state = stateIdle
			}
		}
	}

	// Check if we have a tag and what protocol it is
	if p.numTags == 0 || !p.tagSelected || p.tags[0].RFProtocol == RFProtocolUnknown {
		return fmt.Errorf("no valid tag present")
	}

	// Save tag info before potentially releasing lock
	protocol := p.tags[0].RFProtocol

	var cmd []byte
	switch protocol {
	case RFProtocolT2T:
		// T2T write command: 0xA2 followed by block number and data
		cmd = make([]byte, 6)
		cmd[0] = 0xA2               // T2T WRITE command
		cmd[1] = byte(address >> 2) // Convert address to block number (4 bytes per block)
		copy(cmd[2:], data)         // Copy the data (4 bytes)
	case RFProtocolISODEP:
		// For ISO-DEP, we need to send a different command
		// The command is: CLA=0x00, INS=0xD6 (UPDATE BINARY), P1=high byte, P2=low byte, Lc=len(data), Data
		cmd = make([]byte, 5+len(data))
		cmd[0] = 0x00                 // CLA
		cmd[1] = 0xD6                 // INS (UPDATE BINARY)
		cmd[2] = byte(address >> 8)   // P1 (high byte of address)
		cmd[3] = byte(address & 0xFF) // P2 (low byte of address)
		cmd[4] = byte(len(data))      // Lc (length of data)
		copy(cmd[5:], data)
	default:
		return fmt.Errorf("unsupported protocol: %s", protocol)
	}

	// Send as DATA packet
	p.txBuf[0] = nciMsgTypeData << nciMsgTypeBit // DATA packet
	p.txBuf[1] = 0                               // Connection ID
	p.txBuf[2] = byte(len(cmd))                  // Payload length
	copy(p.txBuf[3:], cmd)
	p.txSize = 3 + len(cmd)

	if p.debug {
		if p.logCallback != nil {
			p.logCallback(LogLevelDebug, fmt.Sprintf("DATA_TX: %X", cmd))
		}
	}

	// Add retries for RF frame corruption errors
	const maxRFRetries = 4
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
			// Handle serious I/O errors with reinitialization
			return p.handleSeriousErrorWithReinit(err, "write command")
		}

		// We may receive multiple responses - keep reading until we get the actual ACK
		for {
			// Check for special response codes - NTAG arbiter busy
			if len(resp) >= 5 && resp[3] == 0x03 {
				// 0x03 = NTAG arbiter busy (locked to I2C interface)
				// Always treat as arbiter busy since there are no other 0x03 errors

				shouldContinue, err := p.handle0300Error("write")
				if !shouldContinue {
					lastErr = err
					break
				}
				lastErr = err
				// Break to outer retry loop to re-attempt the write operation
				break
			}

			// For T2T, we expect an ACK (0x0A) response
			if protocol == RFProtocolT2T {
				if len(resp) >= 4 && resp[3] == 0x0A {
					return nil
				}
			}

			// For ISO-DEP, check the response status
			if protocol == RFProtocolISODEP {
				if len(resp) >= 5 && resp[3] == 0x90 && resp[4] == 0x00 {
					return nil
				}
			}

			// Check if this is a CORE_CONN_CREDITS_NTF
			if len(resp) >= 3 && resp[0] == 0x60 && resp[1] == 0x06 {
				resp, err = p.handleCreditNotification()
				if err != nil {
					lastErr = err
					break
				}
				continue
			}

			// If we get here, the response wasn't what we expected
			lastErr = fmt.Errorf("invalid response: %X", resp)
			break
		}

		// If we broke from inner loop with arbiter busy (lastErr != nil but reselection succeeded), retry
		if lastErr != nil && strings.Contains(lastErr.Error(), "arbiter busy") {
			continue
		}

		// If we broke from inner loop with no error (soft recovery), continue retry
		if lastErr == nil {
			continue
		}

		// Otherwise we have an error, add delay and retry
		time.Sleep(10 * time.Millisecond)
	}

	if lastErr != nil {
		// Check if this is a tag departure error first
		if nfcErr, ok := lastErr.(*Error); ok && nfcErr.Code == ErrTagDeparted {
			return lastErr // Return tag departure error directly without HAL reinitialization
		}
		// For serious errors (not 0300, timeout, or tag departure), perform full reinitialization
		if !strings.Contains(lastErr.Error(), "0300") && !strings.Contains(lastErr.Error(), "timeout") && !strings.Contains(lastErr.Error(), "reinitialization") && !strings.Contains(lastErr.Error(), "tag departed") {
			finalErr := p.handleSeriousErrorWithReinit(lastErr, "write after retries")
			return fmt.Errorf("write failed after %d retries: %v", maxRFRetries, finalErr)
		}
		return fmt.Errorf("write failed after %d retries: %v", maxRFRetries, lastErr)
	}
	return fmt.Errorf("write failed with unknown error")
}

// SelectTag selects a specific tag for communication
func (p *PN7150) SelectTag(tagIdx uint) error {

	if tagIdx >= uint(p.numTags) {
		if p.logCallback != nil {
			p.logCallback(LogLevelError, fmt.Sprintf("select tag: invalid tag_idx: %d", tagIdx))
		}
		return fmt.Errorf("invalid tag index: %d", tagIdx)
	}

	if p.logCallback != nil {
		p.logCallback(LogLevelDebug, fmt.Sprintf("select tag: tag_idx=%d", tagIdx))
	}

	// If a tag is already selected, deselect it first
	if p.tagSelected {
		// Deactivate to sleep mode
		cmd := []byte{
			0x21, // MT=CMD (1 << 5), GID=RF
			0x06, // OID=DEACTIVATE
			0x01, // Length
			0x01, // Deactivation type = Sleep
		}
		_, err := p.transfer(cmd)
		if err != nil {
			return fmt.Errorf("deactivate tag failed: %v", err)
		}

		// Wait for deactivation notification
		err = p.awaitNotification(0x0106, 250) // RF_DEACTIVATE notification
		if err != nil {
			return err
		}
		p.tagSelected = false
	}

	// Select the given tag
	cmd := []byte{
		0x21,             // MT=CMD (1 << 5), GID=RF
		0x04,             // OID=DISCOVER_SELECT
		0x03,             // Length
		byte(tagIdx + 1), // RF Discovery ID (1-based)
		byte(p.tags[tagIdx].RFProtocol),
		0x00, // RF Interface - will be set below
	}

	// Set appropriate interface based on protocol
	if p.tags[tagIdx].RFProtocol == RFProtocolISODEP {
		cmd[5] = nciRFInterfaceISODEP
	} else {
		cmd[5] = nciRFInterfaceFrame
	}

	resp, err := p.transfer(cmd)
	if err != nil {
		return fmt.Errorf("select tag command failed: %v", err)
	}

	nciResp, err := parseNCIResponse(resp)
	if err != nil {
		return fmt.Errorf("failed to parse select response: %v", err)
	}

	if !isSuccessResponse(nciResp) {
		return fmt.Errorf("select tag failed with status: %02x", nciResp.Status)
	}

	// Wait for tag activation
	err = p.awaitNotification(0x0105, 250) // RF_INTF_ACTIVATED notification
	if err != nil {
		if err.Error() == "timeout" {
			return fmt.Errorf("tag departed")
		}
		return err
	}

	p.tagSelected = true
	return nil
}

// reselectCurrentTag reselects the current tag to resolve NTAG I2C arbiter conflicts
// Uses RF_DEACTIVATE → RF_DISCOVER_SELECT sequence to reset tag state
// Note: This function assumes the caller already holds the PN7150 lock
func (p *PN7150) reselectCurrentTag() error {
	if p.logCallback != nil {
		p.logCallback(LogLevelInfo, "Reselecting tag to resolve arbiter busy conflict")
	}

	tagIdx := uint(0) // Always reselect tag 0

	if tagIdx >= uint(p.numTags) {
		return fmt.Errorf("invalid tag index for reselection: %d", tagIdx)
	}

	// Step 1: Deactivate current tag to sleep mode (if selected)
	if p.tagSelected {
		deactivateCmd := []byte{
			0x21, // MT=CMD (1 << 5), GID=RF
			0x06, // OID=DEACTIVATE
			0x01, // Length
			0x01, // Deactivation type = Sleep
		}

		resp, err := p.transfer(deactivateCmd)
		if err != nil {
			return fmt.Errorf("RF deactivate failed during reselection: %v", err)
		}

		// Check deactivate response
		if len(resp) < 4 || resp[0] != 0x41 || resp[1] != 0x06 || resp[3] != 0x00 {
			return fmt.Errorf("RF deactivate response invalid during reselection: %x", resp)
		}

		// Wait for deactivate notification
		time.Sleep(10 * time.Millisecond)
		p.tagSelected = false
	}

	// Step 2: RF_DISCOVER_SELECT to reselect the tag
	discoverSelectCmd := []byte{
		0x21, // MT=CMD (1 << 5), GID=RF
		0x04, // OID=DISCOVER_SELECT
		0x03, // Length
		0x01, // Discovery ID (1-based, so tag 0 = discovery ID 1)
		0x02, // RF Protocol = T2T
		0x01, // RF Interface = FRAME interface
	}

	resp, err := p.transfer(discoverSelectCmd)
	if err != nil {
		return fmt.Errorf("RF discover select failed during reselection: %v", err)
	}

	// Check discover select response
	if len(resp) < 4 || resp[0] != 0x41 || resp[1] != 0x04 || resp[3] != 0x00 {
		return fmt.Errorf("RF discover select response invalid during reselection: %x", resp)
	}

	// Step 3: Wait for RF_INTF_ACTIVATED notification (0x0105)
	// Give it some time to activate
	time.Sleep(20 * time.Millisecond)

	// Try to read the activation notification
	activationResp, err := p.transfer(nil)
	if err != nil {
		return fmt.Errorf("failed to receive activation notification during reselection: %v", err)
	}

	// Check for RF_INTF_ACTIVATED notification
	if len(activationResp) < 4 || activationResp[0] != 0x61 || activationResp[1] != 0x05 {
		return fmt.Errorf("expected RF_INTF_ACTIVATED notification, got: %x", activationResp)
	}

	p.tagSelected = true

	if p.logCallback != nil {
		p.logCallback(LogLevelInfo, "Tag reselection completed successfully")
	}

	return nil
}

// GetTagEventChannel implements HAL.GetTagEventChannel
func (p *PN7150) GetTagEventChannel() <-chan TagEvent {
	return p.tagEventChan
}

// SetTagEventReaderEnabled enables or disables the tag event reader goroutine
func (p *PN7150) SetTagEventReaderEnabled(enabled bool) {

	if enabled && !p.tagEventReaderRunning {
		p.tagEventReaderRunning = true
		go p.tagEventReader()
		if p.logCallback != nil {
			p.logCallback(LogLevelInfo, "Tag event reader started")
		}
	} else if !enabled && p.tagEventReaderRunning {
		p.tagEventReaderRunning = false
		close(p.tagEventReaderStop)
		// Wait a bit for the goroutine to stop
		time.Sleep(10 * time.Millisecond)
		// Recreate the stop channel for next time
		p.tagEventReaderStop = make(chan struct{})
		if p.logCallback != nil {
			p.logCallback(LogLevelInfo, "Tag event reader stopped")
		}
	}
}

// handleSeriousErrorWithReinit handles serious I/O errors by detecting tag departure or performing full HAL reinitialization
// It unlocks the mutex, performs reinitialization, and re-locks the mutex
// Returns an error that includes both the original error and any reinitialization error
func (p *PN7150) handleSeriousErrorWithReinit(err error, operation string) error {
	// Only handle serious I/O errors, not temporary ones
	if err == unix.EINTR || err == unix.EAGAIN || err == unix.ETIMEDOUT {
		return err
	}

	// Check if this error indicates tag departure (common tag departure errors)
	if err == unix.ENODEV || err == unix.ENXIO || err == unix.ENOENT ||
		strings.Contains(err.Error(), "no such device") ||
		strings.Contains(err.Error(), "device or address") {

		if p.logCallback != nil {
			p.logCallback(LogLevelInfo, fmt.Sprintf("%s failed with tag departure error: %v - sending tag departure event", operation, err))
		}

		// Send tag departure event immediately instead of HAL reinitialization
		select {
		case p.tagEventChan <- TagEvent{Type: TagDeparture}:
		default:
		}
		return NewError(ErrTagDeparted, fmt.Sprintf("tag departed during %s", operation))
	}

	if p.logCallback != nil {
		p.logCallback(LogLevelError, fmt.Sprintf("%s failed with serious error: %v - HAL reinitialization disabled", operation, err))
	}

	// HAL reinitialization disabled - just return the error
	// // Release lock before reinitialization
	// reinitErr := p.FullReinitialize()

	// Return the original error since reinitialization is disabled
	return fmt.Errorf("%s failed: %v", operation, err)
}

// handle0300Error handles NTAG arbiter busy (0x03) errors with tag reselection
// Returns true if the operation should continue retrying, false if it should abort
func (p *PN7150) handle0300Error(operation string) (bool, error) {
	if p.logCallback != nil {
		p.logCallback(LogLevelWarning, fmt.Sprintf("NTAG arbiter busy in %s", operation))
	}

	// Immediately attempt tag reselection for arbiter busy
	if p.numTags > 0 {
		if p.logCallback != nil {
			p.logCallback(LogLevelInfo, "Attempting tag reselection for arbiter busy conflict")
		}

		// Try tag reselection to resolve arbiter conflict
		err := p.reselectCurrentTag()
		if err != nil {
			if p.logCallback != nil {
				p.logCallback(LogLevelWarning, fmt.Sprintf("Tag reselection failed: %v", err))
			}
			// Return error to allow retry
			return true, fmt.Errorf("tag reselection failed: %v", err)
		}
		// Tag reselection succeeded (already logged in reselectCurrentTag), continue retry
		return true, nil
	}

	// Avoid HAL reinitialization on 0x03 errors - just keep retrying
	// If no tags, return error
	return true, fmt.Errorf("NTAG arbiter busy")
}

// handleCreditNotification handles CORE_CONN_CREDITS_NTF and reads the next response
// Returns the next response or an error
func (p *PN7150) handleCreditNotification() ([]byte, error) {
	// This is a credit notification, read the next response
	resp, err := p.transfer(nil)
	if err != nil {
		return nil, err
	}

	// If we get no response after credit notification, this indicates tag departure
	if len(resp) == 0 {
		if p.logCallback != nil {
			p.logCallback(LogLevelInfo, "No response after credit notification - tag departed")
		}

		// Send tag departure event immediately instead of HAL reinitialization
		select {
		case p.tagEventChan <- TagEvent{Type: TagDeparture}:
		default:
		}
		return nil, NewError(ErrTagDeparted, "tag departed during credit notification")
	}

	return resp, nil
}

// awaitNotification waits for a specific notification message with timeout tracking
func (p *PN7150) awaitNotification(msgID uint16, timeoutMs uint) error {
	startTime := time.Now()
	remainingTimeout := time.Duration(timeoutMs) * time.Millisecond

	for {
		// Check if we've exceeded the timeout
		elapsed := time.Since(startTime)
		if elapsed >= time.Duration(timeoutMs)*time.Millisecond {
			if p.logCallback != nil {
				p.logCallback(LogLevelWarning, "await notification timeout")
			}
			return fmt.Errorf("timeout waiting for notification 0x%04X", msgID)
		}

		// Calculate remaining timeout
		remainingTimeout = time.Duration(timeoutMs)*time.Millisecond - elapsed

		// Try to read a packet with the remaining timeout
		resp, err := p.transferWithTimeout(nil, remainingTimeout)
		if err != nil {
			return err
		}

		// Check if this is the notification we're waiting for
		if len(resp) >= 3 {
			mt := (resp[0] >> nciMsgTypeBit) & 0x03
			if mt == nciMsgTypeNotification {
				gid := resp[0] & 0x0F
				oid := resp[1] & 0x3F
				gotMsgID := uint16(gid)<<8 | uint16(oid)
				if gotMsgID == msgID {
					return nil // Found the notification
				}
			}
		}

		// Update elapsed time for next iteration
		startTime = time.Now()
	}
}

// transferWithTimeout performs a transfer with a specific timeout
func (p *PN7150) transferWithTimeout(tx []byte, timeout time.Duration) ([]byte, error) {
	if tx != nil {
		if p.debug {
			p.logNCI(tx, len(tx), "TX")
		}

		var writeErr error
		for i := 0; i <= i2cMaxRetries; i++ {
			n, err := unix.Write(p.fd, tx)
			if err == nil && n == len(tx) {
				// Success
				break
			}

			if err != nil {
				writeErr = err
				// Retry on NACK or arbitration lost
				if (err == unix.ENXIO || err == unix.EAGAIN) && i < i2cMaxRetries {
					time.Sleep(time.Duration(i2cRetryTimeUs) * time.Microsecond)
					if p.debug && p.logCallback != nil {
						p.logCallback(LogLevelDebug, fmt.Sprintf("Retrying to send data, try %d/%d", i+1, i2cMaxRetries))
					}
					continue
				}
				return nil, fmt.Errorf("write error: %v", err)
			}

			if n != len(tx) {
				writeErr = fmt.Errorf("incomplete write: %d != %d", n, len(tx))
				if i < i2cMaxRetries {
					time.Sleep(time.Duration(i2cRetryTimeUs) * time.Microsecond)
					continue
				}
			}
		}

		if writeErr != nil {
			return nil, writeErr
		}
	}

	// Direct read with custom timeout
	pfd := unix.PollFd{
		Fd:     int32(p.fd),
		Events: unix.POLLIN,
	}

	readDeadline := time.Now().Add(timeout)

	for {
		if time.Now().After(readDeadline) {
			if tx == nil {
				return nil, nil // No notifications available
			}
			return nil, fmt.Errorf("read timeout")
		}

		timeoutMs := int(time.Until(readDeadline) / time.Millisecond)
		if timeoutMs < 1 {
			timeoutMs = 1
		}

		n, err := unix.Poll([]unix.PollFd{pfd}, timeoutMs)
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

		// Read header first with retry logic for NACK handling
		var readErr error
		var readN int
		for retry := 0; retry <= i2cMaxRetries; retry++ {
			readN, err = unix.Read(p.fd, p.rxBuf[:3])
			if err == nil && readN > 0 {
				// Success
				break
			}

			if err != nil {
				if err == unix.EINTR {
					continue
				}
				if err == unix.ENXIO && retry < i2cMaxRetries {
					if p.logCallback != nil {
						p.logCallback(LogLevelWarning, fmt.Sprintf("Read header NACKed: %v, retry %d/%d", err, retry+1, i2cMaxRetries))
					}
					time.Sleep(time.Duration(i2cRetryTimeUs) * time.Microsecond)
					continue
				}
				if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
					// No data available
					if tx == nil {
						return nil, nil // No notifications available
					}
					continue // Keep waiting for response
				}
				readErr = fmt.Errorf("read header error: %v", err)
				break
			}
		}

		if readErr != nil {
			return nil, readErr
		}

		if readN == 0 {
			// Zero-length read - treat as no data available
			if tx == nil {
				return nil, nil // No notifications available
			}
			continue // Keep waiting for response
		}

		if readN != 3 {
			return nil, fmt.Errorf("incomplete header read: %d", readN)
		}

		// Basic validation
		mt := (p.rxBuf[0] >> nciMsgTypeBit) & 0x03
		pbf := p.rxBuf[0] & 0x10

		if mt == nciMsgTypeCommand || pbf != 0 {
			if p.logCallback != nil {
				p.logCallback(LogLevelWarning, fmt.Sprintf("Invalid header: MT=%d, PBF=%d", mt, pbf))
			}
			p.flushReadBuffer()
			return nil, fmt.Errorf("invalid NCI header")
		}

		// Additional validation based on message type
		if mt == nciMsgTypeData {
			// For data messages, check connection ID is valid
			if p.rxBuf[1] != 0 {
				if p.logCallback != nil {
					p.logCallback(LogLevelWarning, fmt.Sprintf("Invalid data header: ConnID=%02X", p.rxBuf[1]))
				}
				p.flushReadBuffer()
				return nil, fmt.Errorf("invalid data header")
			}
		} else {
			// For commands/responses/notifications, check OID validity
			if (p.rxBuf[1] & ^byte(0x3F)) != 0 {
				if p.logCallback != nil {
					p.logCallback(LogLevelWarning, fmt.Sprintf("Invalid header: OID byte=%02X", p.rxBuf[1]))
				}
				p.flushReadBuffer()
				return nil, fmt.Errorf("invalid header OID")
			}
		}

		payloadLen := int(p.rxBuf[2])
		if payloadLen > 0 {
			// Check if we can read from the reader
			pfdCheck := unix.PollFd{
				Fd:     int32(p.fd),
				Events: unix.POLLIN,
			}
			pollN, err := unix.Poll([]unix.PollFd{pfdCheck}, 0)
			if err == nil && pollN <= 0 {
				// No data available - header without payload is invalid
				if p.logCallback != nil {
					p.logCallback(LogLevelWarning, "Timed out waiting for payload")
				}
				return nil, fmt.Errorf("incomplete message: no payload available")
			}

			// Read payload with retry logic
			for retry := 0; retry <= i2cMaxRetries; retry++ {
				payloadN, err := unix.Read(p.fd, p.rxBuf[3:3+payloadLen])
				if err == nil && payloadN == payloadLen {
					// Success
					break
				}

				if err != nil {
					if err == unix.ENXIO && retry < i2cMaxRetries {
						// Address NACK, retry
						time.Sleep(time.Duration(i2cRetryTimeUs) * time.Microsecond)
						continue
					}
					return nil, fmt.Errorf("read payload error: %v", err)
				}

				if payloadN != payloadLen {
					if retry < i2cMaxRetries {
						time.Sleep(time.Duration(i2cRetryTimeUs) * time.Microsecond)
						continue
					}
					return nil, fmt.Errorf("incomplete payload read: %d != %d", payloadN, payloadLen)
				}
			}
		}

		totalLen := 3 + payloadLen
		if p.debug {
			p.logNCI(p.rxBuf[:totalLen], totalLen, "RX")
		}

		if mt == nciMsgTypeNotification {
			gid := p.rxBuf[0] & 0x0F
			oid := p.rxBuf[1] & 0x3F
			if gid == nciGroupCore && oid == nciCoreReset {
				if p.logCallback != nil {
					p.logCallback(LogLevelError, fmt.Sprintf("Unexpected reset notification: %X", p.rxBuf[3:totalLen]))
				}
				return nil, fmt.Errorf("unexpected NFC controller reset")
			}
		}

		// Special case: If we sent a data packet (MT=0), expect a notification as response
		// Make sure tx is not nil and has at least 1 element before accessing tx[0]
		if len(tx) > 0 && mt == nciMsgTypeNotification && (tx[0]&0xE0) == 0 {
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

// checkRFTransition verifies an RF transition configuration value
func (p *PN7150) checkRFTransition(id, offset byte, expectedValue []byte) error {
	for check := 0; check < paramCheckRetries; check++ {
		// Build RF_GET_TRANSITION command (proprietary PN7150 command)
		cmd := []byte{
			0x2F,
			0x14, // OID=RF_GET_TRANSITION
			0x02, // Length
			id,
			offset,
		}

		resp, err := p.transfer(cmd)
		if err != nil {
			return err
		}

		// Parse response
		if len(resp) < 5+len(expectedValue) {
			return fmt.Errorf("invalid RF_GET_TRANSITION_RSP length")
		}

		// Check response format
		if resp[4] != byte(len(expectedValue)) {
			return fmt.Errorf("invalid RF_GET_TRANSITION_RSP format")
		}

		// Check if value matches
		if bytes.Equal(resp[5:5+len(expectedValue)], expectedValue) {
			return nil // Success
		}

		if check < paramCheckRetries-1 {
			if p.logCallback != nil {
				p.logCallback(LogLevelWarning, fmt.Sprintf("RF transition id=0x%02X offset=0x%02X mismatch, retry %d/%d", id, offset, check+1, paramCheckRetries))
			}
		}
	}

	return fmt.Errorf("RF transition id=0x%02X offset=0x%02X incorrect after %d checks", id, offset, paramCheckRetries)
}

// checkParam verifies a configuration parameter value matches expected value
func (p *PN7150) checkParam(paramID uint16, expectedValue []byte) error {
	for check := 0; check < paramCheckRetries; check++ {
		// Build CORE_GET_CONFIG command
		cmd := []byte{
			0x20, // MT=CMD (1 << 5), GID=CORE
			0x03, // OID=GET_CONFIG
			0x03, // Length (1 byte num params + 2 bytes param ID)
			0x01, // Number of parameters
			byte(paramID >> 8),
			byte(paramID & 0xFF),
		}

		resp, err := p.transfer(cmd)
		if err != nil {
			return err
		}

		// Parse response
		if len(resp) < 8+len(expectedValue) {
			return fmt.Errorf("invalid CORE_GET_CONFIG_RSP length")
		}

		// Check response format
		if resp[4] != 1 || // Number of parameters
			resp[5] != byte(paramID>>8) ||
			resp[6] != byte(paramID&0xFF) ||
			resp[7] != byte(len(expectedValue)) {
			return fmt.Errorf("invalid CORE_GET_CONFIG_RSP format")
		}

		// Check if value matches
		if bytes.Equal(resp[8:8+len(expectedValue)], expectedValue) {
			return nil // Success
		}

		if check < paramCheckRetries-1 {
			if p.logCallback != nil {
				p.logCallback(LogLevelWarning, fmt.Sprintf("Parameter 0x%04X mismatch, retry %d/%d", paramID, check+1, paramCheckRetries))
			}
		}
	}

	return fmt.Errorf("parameter 0x%04X incorrect after %d checks", paramID, paramCheckRetries)
}

// flushReadBuffer reads and discards any pending data
func (p *PN7150) flushReadBuffer() error {
	buf := make([]byte, nciBufferSize)
	deadline := time.Now().Add(100 * time.Millisecond)

	for time.Now().Before(deadline) {
		// Use poll to check if data is available
		pfd := unix.PollFd{
			Fd:     int32(p.fd),
			Events: unix.POLLIN,
		}
		n, err := unix.Poll([]unix.PollFd{pfd}, 0) // Non-blocking poll
		if err != nil || n <= 0 {
			// No data available or error
			return nil
		}

		// Read and discard the data
		r, err := unix.Read(p.fd, buf)
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
		if p.logCallback != nil {
			p.logCallback(LogLevelInfo, fmt.Sprintf("Flushed %d bytes", r))
		}
	}
	return nil
}

// transfer performs an NCI transfer operation
func (p *PN7150) transfer(tx []byte) ([]byte, error) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	return p.transferWithTimeout(tx, readTimeout)
}

// tagEventReader is a goroutine that continuously monitors for tag arrival/departure events
func (p *PN7150) tagEventReader() {
	// Recover from panics and restart the goroutine
	defer func() {
		if r := recover(); r != nil {
			if p.logCallback != nil {
				p.logCallback(LogLevelError, fmt.Sprintf("Tag event reader panicked: %v, restarting...", r))
			}
			// Try to restart the goroutine if HAL is still running
			if p.tagEventReaderRunning && p.state != stateUninitialized {
				go p.tagEventReader()
			}
		}
	}()

	var previousTags []Tag
	ticker := time.NewTicker(100 * time.Millisecond) // Poll every 100ms
	defer ticker.Stop()

	if p.logCallback != nil {
		p.logCallback(LogLevelDebug, "Tag event reader started")
	}

	for {
		select {
		case <-p.tagEventReaderStop:
			if p.logCallback != nil {
				p.logCallback(LogLevelDebug, "Tag event reader stopped")
			}
			return
		case <-ticker.C:
			// Only process if we're in discovering or present state
			state := p.GetState()
			if state != StateDiscovering && state != StatePresent {
				continue
			}

			// Get current tags
			currentTags, err := p.DetectTags()
			if err != nil {
				// Check for serious errors that might indicate goroutine should exit
				if strings.Contains(err.Error(), "invalid state") || strings.Contains(err.Error(), "unexpected reset") {
					if p.logCallback != nil {
						p.logCallback(LogLevelError, fmt.Sprintf("Tag event reader detected serious error: %v, exiting for reinit", err))
					}
					// Send a tag departed event to trigger recovery
					select {
					case p.tagEventChan <- TagEvent{Type: TagDeparture}:
					default:
					}
					return
				}

				// Increment detection failures for robust departure detection
				p.detectionFailures++
				failures := p.detectionFailures

				// If we have consecutive failures and previously had tags, treat as departure
				if failures >= 3 && len(previousTags) > 0 {
					if p.logCallback != nil {
						p.logCallback(LogLevelWarning, fmt.Sprintf("Tag detection failed %d times, treating as departure", failures))
					}

					// Generate departure event for each previously detected tag
					for _, prevTag := range previousTags {
						tagCopy := prevTag
						event := TagEvent{
							Type: TagDeparture,
							Tag:  &tagCopy,
						}
						select {
						case p.tagEventChan <- event:
							if p.logCallback != nil {
								p.logCallback(LogLevelInfo, fmt.Sprintf("Tag departed (detection failure): %X", prevTag.ID))
							}
						default:
							if p.logCallback != nil {
								p.logCallback(LogLevelWarning, "Tag event channel full, dropping departure event")
							}
						}
					}

					// Clear previous tags and reset failure counter
					previousTags = nil
					p.detectionFailures = 0
				}
				continue
			}

			// Reset detection failures on successful detection
			p.detectionFailures = 0

			// Check for tag arrivals
			for _, currentTag := range currentTags {
				found := false
				for _, prevTag := range previousTags {
					if tagsEqual(&currentTag, &prevTag) {
						found = true
						break
					}
				}
				if !found {
					// New tag arrived
					tagCopy := currentTag
					event := TagEvent{
						Type: TagArrival,
						Tag:  &tagCopy,
					}
					select {
					case p.tagEventChan <- event:
						if p.logCallback != nil {
							p.logCallback(LogLevelInfo, fmt.Sprintf("Tag arrived: %X", currentTag.ID))
						}
					default:
						// Channel full, drop event
						if p.logCallback != nil {
							p.logCallback(LogLevelWarning, "Tag event channel full, dropping arrival event")
						}
					}
				}
			}

			// Check for tag departures
			for _, prevTag := range previousTags {
				found := false
				for _, currentTag := range currentTags {
					if tagsEqual(&prevTag, &currentTag) {
						found = true
						break
					}
				}
				if !found {
					// Tag departed
					tagCopy := prevTag
					event := TagEvent{
						Type: TagDeparture,
						Tag:  &tagCopy,
					}
					select {
					case p.tagEventChan <- event:
						if p.logCallback != nil {
							p.logCallback(LogLevelInfo, fmt.Sprintf("Tag departed: %X", prevTag.ID))
						}
					default:
						// Channel full, drop event
						if p.logCallback != nil {
							p.logCallback(LogLevelWarning, "Tag event channel full, dropping departure event")
						}
					}
				}
			}

			// Update previous tags
			previousTags = make([]Tag, len(currentTags))
			copy(previousTags, currentTags)
		}
	}
}

// tagsEqual compares two tags for equality based on their IDs
func tagsEqual(a, b *Tag) bool {
	if len(a.ID) != len(b.ID) {
		return false
	}
	for i := range a.ID {
		if a.ID[i] != b.ID[i] {
			return false
		}
	}
	return true
}
