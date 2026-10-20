package qoder

import (
	"context"
	"errors"
	"io"

	"client2api/internal/core"
)

// clistream.go adapts one CLI lease to the gateway's stream contract.
//
// The lease already emits core.Events; what this wrapper adds is the two things
// every other transport in this module does and the CLI path must not skip:
//
//   - the in-flight counter and the per-account slot are released exactly once,
//     when the stream is closed or when it ends, whichever comes first; and
//   - an error that names no account is attributed to the one this stream came
//     from, so the gateway's usage ledger and its failover both see the truth.

type cliStream struct {
	ctx     context.Context
	cancel  context.CancelFunc
	release func()
	inner   core.Stream
	account string
	done    bool
}

func newCLIStream(parent context.Context, cancel context.CancelFunc, release func(), inner core.Stream, account string) *cliStream {
	if parent == nil {
		parent = context.Background()
	}
	return &cliStream{
		ctx:     parent,
		cancel:  cancel,
		release: release,
		inner:   inner,
		account: account,
	}
}

// Recv yields the next event.  A terminal error is attributed to this stream's
// account, and the stream's own resources are released as soon as it ends, so a
// caller that reads the whole stream never has to remember Close.
func (s *cliStream) Recv() (core.Event, error) {
	if s.inner == nil {
		return core.Event{}, io.EOF
	}
	event, err := s.inner.Recv()
	switch {
	case errors.Is(err, io.EOF):
		s.Close()
		return core.Event{}, io.EOF
	case err != nil:
		s.Close()
		return core.Event{}, s.attribute(err)
	case event.Type == core.EventError && event.Err != nil:
		s.Close()
		event.Err = s.attribute(event.Err)
		return event, nil
	}
	return event, nil
}

// Close is idempotent and safe to call from any goroutine.
func (s *cliStream) Close() error {
	if s.done {
		return nil
	}
	s.done = true
	if s.inner != nil {
		_ = s.inner.Close()
	}
	if s.cancel != nil {
		s.cancel()
	}
	if s.release != nil {
		s.release()
		s.release = nil
	}
	return nil
}

// attribute stamps the account on an error that does not name one.  A failure
// the CLI layer already classified is left exactly as it is.
func (s *cliStream) attribute(err error) error {
	if err == nil || s.account == "" {
		return err
	}
	if _, ok := core.AsFailure(err); ok {
		return err
	}
	if core.ErrorAccountID(err) != "" {
		return err
	}
	return core.Fail(clientName, s.account, core.FailureUpstream, 502, err)
}
