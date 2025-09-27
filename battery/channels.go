package battery

// tryUpdateChannel attempts to send a value to a channel, draining any old value if the channel is blocked.
// This ensures the channel always contains the most recent value without blocking the sender.
func tryUpdateChannel[T any](ch chan T, value T) {
	select {
	case ch <- value:
		// Successfully sent
	default:
		// Channel is full, drain old value and send new one
		select {
		case <-ch:
			ch <- value
		default:
			// Channel was drained by receiver between checks, try sending again
			ch <- value
		}
	}
}
