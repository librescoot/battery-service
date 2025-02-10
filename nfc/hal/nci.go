package hal

import (
	"fmt"
)

// NCI message types and bit positions
const (
	nciMsgTypeBit    = 5
	nciMsgTypeData   = 0
	nciMsgTypeCommand = 1
	nciMsgTypeResponse = 2
	nciMsgTypeNotification = 3
)

// NCI Groups
const (
	nciGroupCore uint8 = 0x00
	nciGroupRF   uint8 = 0x01
	nciGroupProp uint8 = 0x0F
	nciGroupStatus uint8 = 0x08  // Status and diagnostics group
)

// NCI Core commands (OID)
const (
	nciCoreReset uint8 = 0x00
	nciCoreInit  uint8 = 0x01
	nciCoreSetConfig uint8 = 0x02
	nciCoreGetConfig uint8 = 0x03
	nciCoreSetPowerModeOID uint8 = 0x09
	nciCoreConnCredits uint8 = 0x06
	nciCoreGenericError uint8 = 0x07
	nciCoreInterfaceError uint8 = 0x08
)

// NCI RF commands (OID)
const (
	nciRFDiscoverMapOID uint8 = 0x00
	nciRFDiscoverOID uint8 = 0x03
	nciRFDiscoverSelectOID uint8 = 0x04
	nciRFIntfActivatedOID uint8 = 0x05
	nciRFDeactivateOID uint8 = 0x06
	nciRFT2TReadOID uint8 = 0x10
	nciRFT2TWriteOID uint8 = 0x11
	nciRFGetTransitionOID uint8 = 0x20
)

// NCI proprietary commands (OID)
const (
	nciProprietaryActOID uint8 = 0x02
	nciProprietarySetPowerModeOID uint8 = 0x00 // PN7150 proprietary power mode command
	nciProprietaryRFGetTransitionOID uint8 = 0x14 // PN7150 proprietary RF transition command
)

// NCI RF protocols and interfaces
const (
	nciRFProtocolT2T uint8 = 0x02     // Type 2 Tag (MIFARE Ultralight)
	nciRFProtocolISODEP uint8 = 0x04  // ISO14443-4
	nciRFInterfaceFrame uint8 = 0x01
	nciRFInterfaceISODEP uint8 = 0x02
)

// NCI status codes
const (
	nciStatusOK uint8 = 0x00
	nciStatusSemanticError uint8 = 0x06
)

// NCI parameter IDs
const (
	nciParamIDTotalDuration uint16 = 0x0000
	nciParamIDClockSelCfg uint16 = 0xA003
	nciParamIDRFTransitionCfg uint16 = 0xA00D
	nciParamIDPMUCfg uint16 = 0xA00E
	nciParamIDTagDetectorCfg uint16 = 0xA040
)

// NCI RF technologies
const (
	nciRFTechNFCAPassivePoll uint8 = 0x00
)

// NCI RF deactivation types
const (
	nciRFDeactivationTypeIdle  uint8 = 0x00
	nciRFDeactivationTypeSleep uint8 = 0x01
)

// Error types
var (
	errParamsIncorrect = fmt.Errorf("parameters incorrect")
	errUnexpectedReset = fmt.Errorf("unexpected reset")
)

// NCI Packet Header
type nciHeader struct {
	MT_PBF_GID uint8  // Message Type (5 bits) | Packet Boundary Flag (1 bit) | Group ID (2 bits)
	OID_NAD    uint8  // Opcode ID (6 bits) | NAD (2 bits)
	Length     uint8
}

// buildNCIHeader creates an NCI header
func buildNCIHeader(mt, gid, oid uint8, length uint8) nciHeader {
	return nciHeader{
		MT_PBF_GID: ((mt & 0x03) << 5) | (gid & 0x0F), // [MT:5][PBF:1][GID:2]
		OID_NAD:    oid & 0x3F,
		Length:     length,
	}
}

// buildNCIPacket creates a complete NCI packet
func buildNCIPacket(header nciHeader, payload []byte) []byte {
	packet := make([]byte, 3+len(payload))
	packet[0] = header.MT_PBF_GID
	packet[1] = header.OID_NAD
	packet[2] = header.Length
	copy(packet[3:], payload)
	return packet
}

// parseNCIHeader parses an NCI header from raw bytes
func parseNCIHeader(data []byte) (nciHeader, error) {
	if len(data) < 3 {
		return nciHeader{}, fmt.Errorf("insufficient data for NCI header")
	}
	return nciHeader{
		MT_PBF_GID: data[0],
		OID_NAD:    data[1],
		Length:     data[2],
	}, nil
}

// NCI Core Reset Commands
func buildCoreReset() []byte {
	header := buildNCIHeader(nciMsgTypeCommand, nciGroupCore, nciCoreReset, 1)
	return buildNCIPacket(header, []byte{0x01})
}

// NCI Core Init Commands
func buildCoreInit() []byte {
	header := buildNCIHeader(nciMsgTypeCommand, nciGroupCore, nciCoreInit, 0)
	return buildNCIPacket(header, nil)
}

// NCI Core Set Config Command
func buildCoreSetConfig(paramID uint16, value []byte) []byte {
	payload := []byte{
		0x01,                    // Number of parameters
		byte(paramID >> 8),     // Parameter ID high byte
		byte(paramID & 0xFF),   // Parameter ID low byte
		byte(len(value)),       // Parameter length
	}
	payload = append(payload, value...)
	header := buildNCIHeader(nciMsgTypeCommand, nciGroupCore, nciCoreSetConfig, uint8(len(payload)))
	return buildNCIPacket(header, payload)
}

// NCI RF Discovery Commands
func buildRFDiscoverCmd(pollPeriod uint) []byte {
	payload := []byte{
		0x01,                         // Number of technologies
		nciRFTechNFCAPassivePoll,    // RF Technology = NFC-A passive poll mode (0x00)
		0x01,                        // Frequency = 1 (ignored by PN7150)
	}
	header := buildNCIHeader(nciMsgTypeCommand, nciGroupRF, nciRFDiscoverOID, uint8(len(payload)))
	return buildNCIPacket(header, payload)
}

// NCI RF Discovery Map Command
func buildRFDiscoverMapCmd() []byte {
	payload := []byte{
		0x02,                    // Number of mappings (2 for both T2T and ISO_DEP)
		nciRFProtocolT2T,       // First mapping: T2T protocol
		0x01,                   // Mode = Poll
		nciRFInterfaceFrame,    // Frame interface for T2T
		nciRFProtocolISODEP,    // Second mapping: ISO_DEP protocol
		0x01,                   // Mode = Poll
		nciRFInterfaceISODEP,   // ISO_DEP interface
	}
	header := buildNCIHeader(nciMsgTypeCommand, nciGroupRF, nciRFDiscoverMapOID, uint8(len(payload)))
	return buildNCIPacket(header, payload)
}

// NCI RF Deactivate Command
func buildRFDeactivateCmd() []byte {
	header := buildNCIHeader(nciMsgTypeCommand, nciGroupRF, nciRFDeactivateOID, 1)
	return buildNCIPacket(header, []byte{0x00}) // Idle mode
}

// T2T Read Command
func buildRFT2TReadCmd(blockAddr byte) []byte {
	header := buildNCIHeader(nciMsgTypeCommand, nciGroupRF, nciRFT2TReadOID, 1)
	return buildNCIPacket(header, []byte{blockAddr})
}

// T2T Write Command
func buildRFT2TWriteCmd(blockAddr byte, data []byte) []byte {
	header := buildNCIHeader(nciMsgTypeCommand, nciGroupRF, nciRFT2TWriteOID, 5)
	payload := make([]byte, 5)
	payload[0] = blockAddr
	copy(payload[1:], data)
	return buildNCIPacket(header, payload)
}

// Response parsing functions
type nciResponse struct {
	Status  uint8
	Payload []byte
}

func parseNCIResponse(data []byte) (*nciResponse, error) {
	header, err := parseNCIHeader(data)
	if err != nil {
		return nil, err
	}

	if len(data) < int(3+header.Length) {
		return nil, fmt.Errorf("incomplete NCI response")
	}

	// For command responses, first byte of payload is status
	mt := (header.MT_PBF_GID >> nciMsgTypeBit) & 0x03
	if mt == nciMsgTypeResponse {
		if header.Length < 1 {
			return nil, fmt.Errorf("invalid response length")
		}
		return &nciResponse{
			Status:  data[3],
			Payload: data[4:3+header.Length],
		}, nil
	}

	// For notifications, no status byte
	return &nciResponse{
		Status:  nciStatusOK,
		Payload: data[3:3+header.Length],
	}, nil
}

// Helper function to check if a response indicates success
func isSuccessResponse(resp *nciResponse) bool {
	return resp.Status == 0x00
}

// RFProtocol represents the NFC protocol type
type RFProtocol uint8

// RF Protocol constants
const (
	RFProtocolT2T    RFProtocol = 0x02 // Type 2 Tag (MIFARE Ultralight)
	RFProtocolISODEP RFProtocol = 0x04 // ISO14443-4
)

// Helper function to extract tag info from RF_INTF_ACTIVATED notification
func parseRFIntfActivatedNtf(data []byte) (*Tag, error) {
	if len(data) < 10 { // Minimum length for RF_INTF_ACTIVATED_NTF
		return nil, fmt.Errorf("invalid RF_INTF_ACTIVATED_NTF length")
	}

	rfProtocol := RFProtocol(data[5])
	rfTechnology := data[6]
	
	// Check for supported protocols
	switch rfProtocol {
	case RFProtocolT2T:
		// For T2T, we don't need to check interface
	case RFProtocolISODEP:
		// For ISO_DEP, we don't need to check interface
	default:
		return nil, fmt.Errorf("unsupported protocol: %02x", rfProtocol)
	}

	if rfTechnology == nciRFTechNFCAPassivePoll {
		uidLen := data[9]
		if len(data) < int(10+uidLen) {
			return nil, fmt.Errorf("invalid UID length")
		}
		return &Tag{
			RFProtocol: rfProtocol,
			ID:         data[10:10+uidLen],
		}, nil
	}

	return nil, fmt.Errorf("unsupported RF technology: %02x", rfTechnology)
} 