package core

import "sync"

// countedStream wraps a module's own stream so a module-level in-flight counter
// is released exactly when the stream closes.
//
// A module that reports PoolStats.InFlight has to know how many upstream calls
// are open right now, and the honest boundary is the same one the gateway uses:
// a request counts from the moment it is admitted until its body is closed.  A
// module whose stream type already carries its own release bookkeeping should
// keep it; this exists so a module without one can opt in without inventing a
// second, subtly different definition of "in flight".
//
// Close is idempotent, and so is the release func it calls, so a double close
// can never decrement twice.
type countedStream struct {
	Stream
	once    sync.Once
	release func()
}

// TrackStream returns a stream whose Close calls release exactly once.  A nil
// stream or a nil release func is returned unchanged, so a caller can wrap
// unconditionally.
func TrackStream(inner Stream, release func()) Stream {
	if inner == nil || release == nil {
		return inner
	}
	return &countedStream{Stream: inner, release: release}
}

// Close implements Stream.  The body is closed first and the slot released
// after, so a caller that immediately opens another request cannot be admitted
// onto a slot the vendor still considers busy.
func (s *countedStream) Close() error {
	err := s.Stream.Close()
	s.once.Do(s.release)
	return err
}
