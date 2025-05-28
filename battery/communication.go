package battery

import (
	"context"
	"fmt"
	"strings"
	"time"

	"battery-service/nfc/hal"
)

// readRegisterWithRetry reads a register with retries and verification
func (r *BatteryReader) readRegisterWithRetry(ctx context.Context, addr uint16) ([]byte, error) {
	var lastErr error
	var data []byte
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
		// Check overall context cancellation at the start of each retry
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

		// Immediately check for context cancellation after HAL operation
		select {
		case <-ctx.Done():
			lastErr = fmt.Errorf("context cancelled during read retry %d: %w", retry+1, ctx.Err())
			goto endReadRetryLoop
		default:
		}

		if lastErr != nil {
			// Check if tag departed
			if nfcErr, ok := lastErr.(*hal.Error); ok && nfcErr.Code == hal.ErrTagDeparted {
				r.logCallback(hal.LogLevelWarning, "Tag departed during read operation")
				r.stateMachine.SendEvent(EventTagDeparted)
				return nil, lastErr
			}
			
			// Check if this is a 0300 recovery error - HAL recovered but needs retry
			if strings.Contains(lastErr.Error(), "recovered from 0300 error") {
				r.logCallback(hal.LogLevelInfo, "HAL recovered from 0300 error, retrying read immediately")
				// Check context before sleep
				select {
				case <-ctx.Done():
					lastErr = fmt.Errorf("context cancelled during recovery delay: %w", ctx.Err())
					goto endReadRetryLoop
				case <-time.After(100 * time.Millisecond):
					continue
				}
			}

			// Check if context timed out externally (if HAL call didn't respect it internally)
			select {
			case <-ctx.Done():
				lastErr = fmt.Errorf("context cancelled after read error on retry %d: %w", retry+1, ctx.Err())
				goto endReadRetryLoop
			default:
			}

			// Check if we lost connection
			if r.hal.GetState() != hal.StatePresent {
				lastErr = fmt.Errorf("lost connection during read: %v", lastErr)
				// Check context before sleep
				select {
				case <-ctx.Done():
					lastErr = fmt.Errorf("context cancelled during connection wait: %w", ctx.Err())
					goto endReadRetryLoop
				case <-time.After(250 * time.Millisecond):
				}
				if r.hal.GetState() == hal.StateDiscovering {
					// Wait a bit more for potential rediscovery, but check context
					select {
					case <-ctx.Done():
						lastErr = fmt.Errorf("context cancelled during discovery wait: %w", ctx.Err())
						goto endReadRetryLoop
					case <-time.After(250 * time.Millisecond):
					}
				}
				continue
			}
			// For other errors, add a shorter delay but check context
			select {
			case <-ctx.Done():
				lastErr = fmt.Errorf("context cancelled during error delay: %w", ctx.Err())
				goto endReadRetryLoop
			case <-time.After(timeCmd):
				continue
			}
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
			time.Sleep(timeCmd) // Use standard delay
			continue
		}

		// If we get here, we have a full response
		// Only add a small delay if this is the first successful read
		if successfulReads == 0 {
			time.Sleep(50 * time.Millisecond) // Short stabilization delay
		}

		// OPTIMIZATION: Skip verification read to speed up polling
		verified = true
		successfulReads++
		break
	}

endReadRetryLoop: // Label to jump to for final error handling

	if !verified {
		err := fmt.Errorf("failed to read register 0x%04x after %d retries: %w", addr, maxReadRetries, lastErr)
		r.logCallback(hal.LogLevelError, err.Error()) // Log the final error
		
		// Don't set communication error fault for context cancelled errors (tag departed)
		if lastErr != nil && !strings.Contains(lastErr.Error(), "context cancel") {
			r.data.Faults.CommunicationError = true
			if updateErr := r.updateRedisStatus(); updateErr != nil {
				r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to update Redis after read communication error: %v", updateErr))
			}
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

	// If the target command is ON and the battery is currently Idle or Asleep,
	// perform HAL recovery to ensure clean NFC session for activation
	if cmd == BatteryCommandOn {
		r.Lock()
		currentState := r.data.State
		r.Unlock()

		if currentState == BatteryStateAsleep || currentState == BatteryStateIdle {
			r.logCallback(hal.LogLevelInfo, fmt.Sprintf("Battery is %s, target is ON. Performing HAL recovery for clean activation.", currentState))

			// Use simple HAL recovery approach
			err := r.simpleHALRecovery()

			if err != nil {
				r.logCallback(hal.LogLevelError, fmt.Sprintf("Failed HAL recovery for %s battery: %v. Proceeding with command might fail.", currentState, err))
				// Original code proceeded even if HAL recreation failed here, so we maintain that behavior.
			} else {
				r.logCallback(hal.LogLevelInfo, fmt.Sprintf("HAL recovery successfully completed for %s battery.", currentState))
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
			// Check context before sleep
			select {
			case <-ctx.Done():
				lastErr = fmt.Errorf("context cancelled during state wait: %w", ctx.Err())
				goto endWriteRetryLoop
			case <-time.After(250 * time.Millisecond):
				continue
			}
		}

		// Create context with timeout for HAL write call
		halWriteCtx, cancelWrite := context.WithTimeout(ctx, timeHALTimeout)
		r.nfcMutex.Lock()
		lastErr = r.hal.WriteBinary(addrCommand, data) // Pass halWriteCtx if HAL supports it
		r.nfcMutex.Unlock()
		cancelWrite()

		// Immediately check for context cancellation after HAL operation
		select {
		case <-ctx.Done():
			lastErr = fmt.Errorf("context cancelled during write retry %d: %w", retry+1, ctx.Err())
			goto endWriteRetryLoop
		default:
		}

		if lastErr == nil {
			// WriteBinary succeeded - ACK was received by the HAL layer
			r.lastCmd = time.Now()
			r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Sent command: %v", cmd))
			// Clear communication error on success
			if r.data.Faults.CommunicationError {
				r.data.Faults.CommunicationError = false
				if updateErr := r.updateRedisStatus(); updateErr != nil {
					r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to update Redis after clearing write communication error: %v", updateErr))
				}
			}
			return nil // Command succeeded
		} else { // lastErr != nil
			// Check if tag departed
			if nfcErr, ok := lastErr.(*hal.Error); ok && nfcErr.Code == hal.ErrTagDeparted {
				r.logCallback(hal.LogLevelWarning, "Tag departed during write operation")
				r.stateMachine.SendEvent(EventTagDeparted)
				return lastErr
			}
			
			// Check if this is a 0300 recovery error - HAL recovered but needs retry
			if strings.Contains(lastErr.Error(), "recovered from 0300 error") {
				r.logCallback(hal.LogLevelInfo, "HAL recovered from 0300 error, retrying write immediately")
				// Check context before sleep
				select {
				case <-ctx.Done():
					lastErr = fmt.Errorf("context cancelled during recovery delay: %w", ctx.Err())
					goto endWriteRetryLoop
				case <-time.After(100 * time.Millisecond):
					continue
				}
			}

			// Check if the timeout was exceeded before the error occurred
			if halWriteCtx.Err() == context.DeadlineExceeded { // Check error directly
				lastErr = fmt.Errorf("hal.WriteBinary timeout or context exceeded before error on retry %d: %w", retry+1, halWriteCtx.Err())
			} // else keep original lastErr or handle other ctx errors if needed

			// Log error with potentially updated lastErr
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Retry %d: WriteBinary failed: %v", retry+1, lastErr))

			// Check if we lost connection
			if r.hal.GetState() != hal.StatePresent {
				lastErr = fmt.Errorf("lost connection during write: %v", lastErr) // Keep updated error if timeout occurred

				// Check context before sleep
				select {
				case <-ctx.Done():
					lastErr = fmt.Errorf("context cancelled during connection wait: %w", ctx.Err())
					goto endWriteRetryLoop
				case <-time.After(250 * time.Millisecond):
				}
				if r.hal.GetState() == hal.StateDiscovering {
					// Wait a bit more for potential rediscovery, but check context
					select {
					case <-ctx.Done():
						lastErr = fmt.Errorf("context cancelled during discovery wait: %w", ctx.Err())
						goto endWriteRetryLoop
					case <-time.After(250 * time.Millisecond):
					}
				}
				continue
			}
		}
	}

endWriteRetryLoop: // Label to jump to for final error handling

	finalErr := fmt.Errorf("failed to send command %v after %d retries: %w", cmd, maxWriteRetries, lastErr)
	r.logCallback(hal.LogLevelError, finalErr.Error())
	
	// Don't set communication error fault for context cancelled errors (tag departed)
	if lastErr != nil && !strings.Contains(lastErr.Error(), "context cancel") {
		r.data.Faults.CommunicationError = true
		if updateErr := r.updateRedisStatus(); updateErr != nil {
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to update Redis after write communication error: %v", updateErr))
		}
	}
	return finalErr // Return the final error
}
