package battery

// tryUpdateChannel sends a value to a size-1 buffered channel, replacing any existing value.
// This ensures the channel always contains the most recent value without blocking.
//
// Requirements:
//   - Channel must be buffered with size 1
//   - Must have a single sender (this function serializes naturally if called from one goroutine)
//
// The send after drain cannot block because:
//   - If channel was full, we just drained it (now has space)
//   - If channel was empty, it has space
//   - Single sender means no one else can fill it between drain and send
func tryUpdateChannel[T any](ch chan T, value T) {
	// Drain any existing value (non-blocking)
	select {
	case <-ch:
	default:
	}
	// Send new value - safe because channel now has space
	ch <- value
}
