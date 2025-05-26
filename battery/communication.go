package battery

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"battery-service/nfc/hal"
)

// readRegisterWithRetry reads a register with retries and verification
func (r *BatteryReader) readRegisterWithRetry(ctx context.Context, addr uint16) ([]byte, error) {
	var lastErr error
	var data, checkData []byte
	var verified bool
	var successfulReads int

	// Check if the reader is temporarily powered down
	r.Lock()
	isPoweredDown := r.isPoweredDown
	r.Unlock()

	// If powered down, power up the HAL first
	if isPoweredDown {
		r.logCallback(hal.LogLevelInfo, "Powering up HAL before reading register")
		r.Lock()
		r.isPoweredDown = false
		r.Unlock()

		// Use simple HAL recovery approach
		if err := r.simpleHALRecovery(); err != nil {
			r.logCallback(hal.LogLevelError, fmt.Sprintf("Failed HAL recovery before reading register: %v", err))
			return nil, fmt.Errorf("failed HAL recovery: %w", err)
		}
	}

	// Reset communication error flag at the start of the attempt sequence
	r.data.Faults.CommunicationError = false

	for retry := 0; retry < maxReadRetries; retry++ {
		// Check overall context cancellation
		select {
		case <-ctx.Done():
			lastErr = fmt.Errorf("context cancelled before read retry %d: %w", retry+1, ctx.Err())
			goto endReadRetryLoop
		default:
		}

		// First read attempt
		r.nfcMutex.Lock()
		data, lastErr = r.hal.ReadBinary(addr)
		r.nfcMutex.Unlock()

		if lastErr != nil {
			// Check if context timed out externally (if HAL call didn't respect it internally)
			select {
			case <-ctx.Done():
				if ctx.Err() == context.DeadlineExceeded {
					lastErr = fmt.Errorf("context deadline exceeded during read on retry %d: %w", retry+1, ctx.Err())
				} else {
					lastErr = fmt.Errorf("context error during read on retry %d: %w", retry+1, ctx.Err())
				}
			default:
			}

			// Check if we lost connection
			if r.hal.GetState() != hal.StatePresent {
				lastErr = fmt.Errorf("lost connection during read: %v", lastErr)
				// Add a longer delay and attempt to recover the connection
				time.Sleep(timeCmdFirstOpened)
				if r.hal.GetState() == hal.StateDiscovering {
					// Wait for potential rediscovery
					time.Sleep(timeCmdSlow) // Replaced timeCmdFirstAsleep
				}
				continue
			}
			// For other errors, add a shorter delay
			time.Sleep(timeCmd)
			continue
		}

		// Check for 0300 response payload from HAL, indicating deep NFC issue
		if len(data) == 2 && data[0] == 0x03 && data[1] == 0x00 {
			r.logCallback(hal.LogLevelWarning, "Received 0300 payload from HAL, performing recovery")

			// Use simple HAL recovery approach
			if err := r.simpleHALRecovery(); err != nil {
				r.logCallback(hal.LogLevelError, fmt.Sprintf("Failed HAL recovery after 0300 payload: %v", err))
				return nil, fmt.Errorf("received 0300 payload and recovery failed: %w", err)
			}

			r.logCallback(hal.LogLevelInfo, "HAL recovery completed after 0300 payload. Signaling for read retry.")
			return nil, errHALRecreatedRetryRead // Signal caller to retry the whole read operation
		}

		// Check for truncated response
		if len(data) < 16 {
			// If we get a truncated response, add a longer delay
			lastErr = fmt.Errorf("truncated response: got %d bytes, expected 16", len(data))
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Retry %d: %v", retry+1, lastErr))
			time.Sleep(timeCmdSlow) // Use slower delay for truncated responses
			continue
		}

		// If we get here, we have a full response
		// Add a small delay before verification read to let the tag stabilize
		time.Sleep(timeCmd)

		// Verify state is still good before verification read
		if r.hal.GetState() != hal.StatePresent {
			lastErr = fmt.Errorf("lost connection before verification read")
			time.Sleep(timeCmdSlow)
			continue
		}

		// Verification read
		halCtxVerify, cancelVerify := context.WithTimeout(ctx, timeHALTimeout)
		r.nfcMutex.Lock()
		checkData, lastErr = r.hal.ReadBinary(addr)
		r.nfcMutex.Unlock()
		cancelVerify()

		if lastErr != nil {
			// Check if context timed out externally
			select {
			case <-halCtxVerify.Done():
				if halCtxVerify.Err() == context.DeadlineExceeded {
					lastErr = fmt.Errorf("hal.ReadBinary verification timeout on retry %d: %w", retry+1, halCtxVerify.Err())
				}
			default:
			}

			// Check if we lost connection
			if r.hal.GetState() != hal.StatePresent {
				lastErr = fmt.Errorf("lost connection during verification read: %v", lastErr)
				time.Sleep(timeCmdSlow)
				continue
			}
			// For other errors, add a shorter delay
			time.Sleep(timeCmd)
			continue
		}

		// Check for truncated verification response
		if len(checkData) < 16 {
			lastErr = fmt.Errorf("truncated verification response: got %d bytes, expected 16", len(checkData))
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Retry %d: %v", retry+1, lastErr))
			time.Sleep(timeCmd)
			continue
		}

		// Track successful reads even if they don't match
		successfulReads++

		// Find the first non-FF byte in both responses
		firstNonFFData := -1
		firstNonFFCheck := -1
		for i := 0; i < min(len(data), len(checkData)); i++ {
			if firstNonFFData == -1 && data[i] != 0xFF {
				firstNonFFData = i
			}
			if firstNonFFCheck == -1 && checkData[i] != 0xFF {
				firstNonFFCheck = i
			}
			if firstNonFFData != -1 && firstNonFFCheck != -1 {
				break
			}
		}

		// If either response is all FFs, try again
		if firstNonFFData == -1 || firstNonFFCheck == -1 {
			lastErr = fmt.Errorf("received all FF response")
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Retry %d: %v", retry+1, lastErr))
			time.Sleep(timeCmdSlow)
			continue
		}

		// Compare the non-FF parts
		if bytes.Equal(data[firstNonFFData:], checkData[firstNonFFCheck:]) {
			verified = true
			// Keep the data starting from first non-FF byte
			data = data[firstNonFFData:]
			break
		}

		// If both responses start with the same meaningful data but differ only in trailing FFs,
		// consider them matching
		meaningfulLen := 0
		for i := 0; i < min(len(data), len(checkData)); i++ {
			if data[i] == 0xFF && checkData[i] == 0xFF {
				break
			}
			if data[i] != checkData[i] {
				meaningfulLen = 0
				break
			}
			meaningfulLen = i + 1
		}
		if meaningfulLen > 0 {
			verified = true
			data = data[:meaningfulLen]
			break
		}

		// If we've had multiple successful reads but they don't match,
		// keep the longer response if one is truncated
		if successfulReads >= 2 {
			if len(data) == 16 && len(checkData) < 16 {
				verified = true
				break
			}
			if len(checkData) == 16 && len(data) < 16 {
				data = checkData
				verified = true
				break
			}
		}

		// Data mismatch, log the difference and try again
		r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Data mismatch at 0x%04x: %X != %X", addr, data, checkData))
		lastErr = fmt.Errorf("data verification failed")
		time.Sleep(timeCmd)
	}

endReadRetryLoop: // Label to jump to for final error handling

	if !verified {
		err := fmt.Errorf("failed to read register 0x%04x after %d retries: %w", addr, maxReadRetries, lastErr)
		r.logCallback(hal.LogLevelError, err.Error()) // Log the final error
		// Set communication error fault if verification failed after retries
		r.data.Faults.CommunicationError = true
		if updateErr := r.updateRedisStatus(); updateErr != nil {
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to update Redis after read communication error: %v", updateErr))
		}
		return nil, err
	}

	// Check for zero data
	allZero := true
	for _, b := range data {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		r.logCallback(hal.LogLevelWarning, fmt.Sprintf("received zero data at register 0x%04x", addr))
		r.data.EmptyOr0Data++
		return nil, fmt.Errorf("received zero data at register 0x%04x", addr)
	}

	// Clear communication error flag on success
	if r.data.Faults.CommunicationError {
		r.data.Faults.CommunicationError = false
		if updateErr := r.updateRedisStatus(); updateErr != nil {
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to update Redis after clearing read communication error: %v", updateErr))
		}
	}

	return data, nil
}

// sendCommand sends a command to the battery with retries
func (r *BatteryReader) sendCommand(ctx context.Context, cmd BatteryCommand) error {
	// Check if the reader is temporarily powered down
	r.Lock()
	isPoweredDown := r.isPoweredDown
	r.Unlock()

	// If powered down, power up the HAL first (unless we're being called directly from the polling code)
	if isPoweredDown {
		r.logCallback(hal.LogLevelInfo, "Powering up HAL before sending command")
		r.Lock()
		r.isPoweredDown = false
		r.Unlock()

		// Use simple HAL recovery approach
		if err := r.simpleHALRecovery(); err != nil {
			r.logCallback(hal.LogLevelError, fmt.Sprintf("Failed HAL recovery before command: %v", err))
			return fmt.Errorf("failed HAL recovery: %w", err)
		}
	}

	// If the target command is ON and the battery is currently Asleep,
	// send the preliminary sequence: InsertedInScooter, then UserClosedSeatbox.
	if cmd == BatteryCommandOn {
		r.Lock()
		currentState := r.data.State
		r.Unlock()

		if currentState == BatteryStateAsleep {
			r.logCallback(hal.LogLevelInfo, "Battery is Asleep, target is ON. Performing HAL recovery.")

			// Use simple HAL recovery approach
			err := r.simpleHALRecovery()

			if err != nil {
				r.logCallback(hal.LogLevelError, fmt.Sprintf("Failed HAL recovery for Asleep battery: %v. Proceeding with command might fail.", err))
				// Original code proceeded even if HAL recreation failed here, so we maintain that behavior.
			} else {
				r.logCallback(hal.LogLevelInfo, "HAL recovery successfully completed for Asleep battery.")
			}

			time.Sleep(timeCmd) // Brief pause after HAL recovery attempt
			r.logCallback(hal.LogLevelInfo, "Proceeding with original ON command attempt.")
		}
	}

	// Ensure minimum time between commands (this applies to the pre-sequence commands too due to recursive calls)
	if time.Since(r.lastCmd) < timeCmd {
		sleepCtx, cancelSleep := context.WithTimeout(ctx, timeCmd-time.Since(r.lastCmd))
		select {
		case <-sleepCtx.Done():
			cancelSleep()
			if sleepCtx.Err() != context.DeadlineExceeded {
				return fmt.Errorf("context cancelled during command delay: %w", sleepCtx.Err())
			}
			// If timeout expired, just continue, minimum time passed
		case <-time.After(timeCmd - time.Since(r.lastCmd)):
			// Normal sleep completion
		}
		cancelSleep()
	}

	data := make([]byte, 4)
	data[0] = byte(cmd)
	data[1] = byte(cmd >> 8)
	data[2] = byte(cmd >> 16)
	data[3] = byte(cmd >> 24)

	var lastErr error
	var successfulWrites int

	// Reset communication error flag at the start of the attempt sequence
	r.data.Faults.CommunicationError = false

	for retry := 0; retry < maxWriteRetries; retry++ {
		// Check overall context cancellation
		select {
		case <-ctx.Done():
			lastErr = fmt.Errorf("context cancelled before write retry %d: %w", retry+1, ctx.Err())
			goto endWriteRetryLoop
		default:
		}

		// Verify state before attempting write
		if r.hal.GetState() != hal.StatePresent {
			lastErr = fmt.Errorf("invalid state for writing: %s", r.hal.GetState())
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Retry %d: %v", retry+1, lastErr))
			time.Sleep(timeCmdSlow) // Use slower delay for state issues
			continue
		}

		// Create context with timeout for HAL write call
		halWriteCtx, cancelWrite := context.WithTimeout(ctx, timeHALTimeout)
		r.nfcMutex.Lock()
		lastErr = r.hal.WriteBinary(addrCommand, data) // Pass halWriteCtx if HAL supports it
		r.nfcMutex.Unlock()
		cancelWrite()

		if lastErr == nil {
			successfulWrites++

			// On first successful write, give a short delay before reading response
			if successfulWrites == 1 {
				sleepCtx, cancelSleep := context.WithTimeout(ctx, timeCmd)
				select {
				case <-sleepCtx.Done():
					cancelSleep()
					lastErr = fmt.Errorf("context cancelled during write-read delay: %w", sleepCtx.Err())
					continue // Go to next retry
				case <-time.After(timeCmd):
					// Normal sleep completion
				}
				cancelSleep()
			}

			// Create context with timeout for HAL read call
			halReadCtx, cancelRead := context.WithTimeout(ctx, timeHALTimeout)
			r.nfcMutex.Lock()
			readData, err := r.hal.ReadBinary(addrCommand) // Pass halReadCtx if HAL supports it
			r.nfcMutex.Unlock()
			cancelRead()

			if err != nil {
				// Check if context timed out externally
				select {
				case <-halReadCtx.Done():
					if halReadCtx.Err() == context.DeadlineExceeded {
						lastErr = fmt.Errorf("hal.ReadBinary response timeout on retry %d: %w", retry+1, halReadCtx.Err())
					} else {
						lastErr = fmt.Errorf("context error during read response on retry %d: %w", retry+1, halReadCtx.Err())
					}
				default:
					lastErr = fmt.Errorf("failed to read response: %w", err)
				}
				r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Retry %d: %v", retry+1, lastErr))
				time.Sleep(timeCmd)
				continue
			}

			// Skip CREDIT notifications (0x60 0x06)
			if len(readData) >= 2 && readData[0] == 0x60 && readData[1] == 0x06 {
				// Read the actual response after the CREDIT notification
				halReadCreditCtx, cancelReadCredit := context.WithTimeout(ctx, timeHALTimeout)
				r.nfcMutex.Lock()
				readData, err = r.hal.ReadBinary(addrCommand) // Pass halReadCreditCtx if HAL supports it
				r.nfcMutex.Unlock()
				cancelReadCredit()

				if err != nil {
					// Check if context timed out externally
					select {
					case <-halReadCreditCtx.Done():
						if halReadCreditCtx.Err() == context.DeadlineExceeded {
							lastErr = fmt.Errorf("hal.ReadBinary after CREDIT timeout on retry %d: %w", retry+1, halReadCreditCtx.Err())
						} else {
							lastErr = fmt.Errorf("context error during read after CREDIT on retry %d: %w", retry+1, halReadCreditCtx.Err())
						}
					default:
						lastErr = fmt.Errorf("failed to read response after CREDIT: %w", err)
					}
					r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Retry %d: %v", retry+1, lastErr))
					time.Sleep(timeCmd)
					continue
				}
			}

			// Check for ACK response (0x0A00)
			if len(readData) >= 2 && readData[0] == 0x0A && readData[1] == 0x00 {
				r.lastCmd = time.Now()
				r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Sent command: %v (ACK received)", cmd))
				// Clear communication error on success
				if r.data.Faults.CommunicationError {
					r.data.Faults.CommunicationError = false
					if updateErr := r.updateRedisStatus(); updateErr != nil {
						r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to update Redis after clearing write communication error: %v", updateErr))
					}
				}
				return nil // Command succeeded
			}

			// Check for RF frame corruption (all 0xFF)
			allFF := true
			for _, b := range readData {
				if b != 0xFF {
					allFF = false
					break
				}
			}
			if allFF {
				lastErr = fmt.Errorf("RF frame corruption detected")
				r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Retry %d: %v", retry+1, lastErr))
				time.Sleep(timeCmdSlow) // Use slower delay for RF issues
				continue
			}

			// If we've had multiple successful writes, consider it good enough
			if successfulWrites >= 2 {
				r.lastCmd = time.Now()
				r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Sent command: %v (accepted after %d successful writes)", cmd, successfulWrites))
				// Clear communication error on success (even if no ACK, write went through)
				if r.data.Faults.CommunicationError {
					r.data.Faults.CommunicationError = false
					if updateErr := r.updateRedisStatus(); updateErr != nil {
						r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to update Redis after clearing write communication error: %v", updateErr))
					}
				}
				return nil // Command succeeded (conditionally)
			}

			// No ACK and not enough successful writes, try again
			lastErr = fmt.Errorf("no ACK received")
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Retry %d: %v", retry+1, lastErr))
			time.Sleep(timeCmd)
		} else { // lastErr != nil
			// Check if the timeout was exceeded before the error occurred
			if halWriteCtx.Err() == context.DeadlineExceeded { // Check error directly
				lastErr = fmt.Errorf("hal.WriteBinary timeout or context exceeded before error on retry %d: %w", retry+1, halWriteCtx.Err())
			} // else keep original lastErr or handle other ctx errors if needed

			// Log error with potentially updated lastErr
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Retry %d: WriteBinary failed: %v", retry+1, lastErr))

			// Check if we lost connection
			if r.hal.GetState() != hal.StatePresent {
				lastErr = fmt.Errorf("lost connection during write: %v", lastErr) // Keep updated error if timeout occurred

				// Add a longer delay and attempt to recover the connection
				time.Sleep(timeCmdFirstOpened)
				if r.hal.GetState() == hal.StateDiscovering {
					// Wait for potential rediscovery
					time.Sleep(timeCmdSlow) // Replaced timeCmdFirstAsleep
				}
				continue
			}
		}
	}

endWriteRetryLoop: // Label to jump to for final error handling

	finalErr := fmt.Errorf("failed to send command %v after %d retries: %w", cmd, maxWriteRetries, lastErr)
	r.logCallback(hal.LogLevelError, finalErr.Error())
	// Set communication error fault if command failed after retries
	r.data.Faults.CommunicationError = true
	if updateErr := r.updateRedisStatus(); updateErr != nil {
		r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to update Redis after write communication error: %v", updateErr))
	}
	return finalErr // Return the final error
}
