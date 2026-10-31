package panel

import (
	"fmt"
	"sync"

	"client2api/internal/core"
)

// storeErrTracker is the shared "a disk write failed and someone should hear
// about it" tail for the panel's persisted stores (chat history, check-in
// state, balance cache, task-board cache).  Before this existed the four
// stores discarded every core.WriteFileAtomic error, so a read-only directory,
// a full disk or an anti-virus lock made the page show data that would vanish
// on the next restart with no trace anywhere (issue #34).
//
// The tracker keeps two channels open at once:
//
//   - LastError() feeds the /panel/api/overview payload, so the UI can badge
//     the affected cards the way the alerts page already does;
//   - the log hook (wired from the panel's log ring) records the failure the
//     moment it happens, not after a restart surfaces an empty store.
//
// The zero value is usable and stays silent: stores built without a log hook
// still remember the error, they just do not ring the bell.
type storeErrTracker struct {
	mu   sync.Mutex
	last error
	logf func(format string, args ...any)
}

// setLog wires the log-ring reporter.  Call it once at construction.
func (t *storeErrTracker) setLog(logf func(format string, args ...any)) {
	t.mu.Lock()
	t.logf = logf
	t.mu.Unlock()
}

// failed records one store failure.  what names the store's subject ("chat
// history"), err is the write or read error.
func (t *storeErrTracker) failed(what string, err error) {
	if err == nil {
		return
	}
	t.mu.Lock()
	t.last = err
	logf := t.logf
	t.mu.Unlock()
	if logf != nil {
		logf("[panel] %s: disk write failed (%v); the change lives in memory only and will be lost on restart", what, core.Redact(err.Error()))
	}
}

// LastError returns the most recent failure as display text, or "" when the
// store has never failed.  The panel ships it in overview as last_error.
func (t *storeErrTracker) LastError() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last == nil {
		return ""
	}
	return core.Redact(t.last.Error())
}

// wireStoreLogs points every persisted store's failure reporter at the panel's
// log ring, so a write that cannot land shows up in the live log the moment it
// happens (issue #34) instead of only after a restart surfaces an empty store.
// Stores built without a ring (tests, embedders) keep silent trackers.
func (p *panel) wireStoreLogs() {
	if p == nil || p.opts.Logs == nil {
		return
	}
	logf := func(format string, args ...any) {
		p.opts.Logs.Add(fmt.Sprintf(format, args...))
	}
	if p.chatHistory != nil {
		p.chatHistory.errs.setLog(logf)
	}
	if p.checkins != nil {
		p.checkins.errs.setLog(logf)
	}
	if p.taskBoardCache != nil {
		p.taskBoardCache.errs.setLog(logf)
	}
}

// storeErrors collects the persisted stores' last disk failures for the
// overview payload (issue #34).  Nil entries are omitted, so the field
// disappears entirely while every write has been landing.
func (p *panel) storeErrors() map[string]string {
	if p == nil {
		return nil
	}
	out := map[string]string{}
	if p.chatHistory != nil {
		if e := p.chatHistory.LastError(); e != "" {
			out["chat history"] = e
		}
	}
	if p.checkins != nil {
		if e := p.checkins.LastError(); e != "" {
			out["check-in state"] = e
		}
	}
	if p.taskBoardCache != nil {
		if e := p.taskBoardCache.LastError(); e != "" {
			out["task board cache"] = e
		}
	}
	if p.balanceCache != nil {
		if e := p.balanceCache.LastError(); e != "" {
			out["balance cache"] = e
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
