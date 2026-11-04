package codearts

import (
	"strings"
	"time"
)

// workingset.go tracks each account's *session working set*: the number of
// distinct vendor sessions the account has served inside the decay window.
// The vendor caps an account at three concurrent sessions (TM.00001041) and
// keeps a finished session warm on its side for a while, so an account the
// pool sees as idle (in-flight 0) can still be full -- and the pool cannot
// know it without counting (issue #189: four 01041s in one night, all while
// stickiness was on, because stickiness only protects *returning* turns; a
// brand-new conversation is still LRU-dropped onto an account holding three
// warm sessions).
//
// The count is a proxy signal, deliberately conservative in what it claims:
// it grows only when a request is actually served, and the 01041 refusal
// pins it to full (the vendor said so) with a decay clock instead of a guess.
// A sticky continuation of a session already in the set is not a new session
// and never counts against the cap.

const (
	// defaultWorkingSetCap mirrors the vendor's own message: 并发会话数已达上限(3个).
	defaultWorkingSetCap = 3
	// defaultSessionDecay is how long a served session keeps counting.  The
	// vendor's real decay is unknown and not queryable; thirty minutes mirrors
	// the affinity window the module already runs, and a stale count can only
	// demote (never exclude), so over-counting errs toward fewer vendor
	// refusals at the cost of some load spread.
	defaultSessionDecay = 30 * time.Minute
	// defaultFullDecay is how long a 01041 refusal keeps an account pinned to
	// full.  Sessions the vendor holds decay on the vendor's clock; ten
	// minutes is the conservative end of the expectation set in the issue.
	defaultFullDecay = 10 * time.Minute
)

// workingSet is one account's view of its own sessions.
type workingSet struct {
	// sessions maps the vendor session id to when it was last served.  A
	// session stays counted until decay removes it.
	sessions map[string]time.Time
	// fullUntil is set by a real 01041: the vendor said the cap is hit, which
	// is stronger evidence than any local count.
	fullUntil time.Time
}

// sessionWorkingSet tracks every account's sessions under one mutex.
type sessionWorkingSet struct {
	sessions map[string]*workingSet
}

func newSessionWorkingSet() *sessionWorkingSet {
	return &sessionWorkingSet{sessions: map[string]*workingSet{}}
}

// wsFor returns (creating if needed) the set for one account.
func (s *sessionWorkingSet) wsFor(accountID string) *workingSet {
	ws, ok := s.sessions[accountID]
	if !ok {
		ws = &workingSet{sessions: map[string]time.Time{}}
		s.sessions[accountID] = ws
	}
	return ws
}

// recordSession notes that accountID served sessionID.  A repeated session id
// (a sticky continuation) replaces its timestamp instead of growing the set:
// the vendor counts *distinct* sessions.
func (s *sessionWorkingSet) recordSession(accountID, sessionID string, now time.Time) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	ws := s.wsFor(accountID)
	ws.sessions[sessionID] = now
	s.gc(now)
}

// isFull reports whether the account counts as holding the cap right now:
// either the vendor said so (a 01041 still inside its decay clock) or the
// locally counted distinct sessions reach the cap.
func (s *sessionWorkingSet) isFull(accountID string, now time.Time) bool {
	ws, ok := s.sessions[accountID]
	if !ok {
		return false
	}
	if now.Before(ws.fullUntil) {
		return true
	}
	return s.distinct(accountID, now) >= defaultWorkingSetCap
}

// distinct counts the sessions inside the decay window.
func (s *sessionWorkingSet) distinct(accountID string, now time.Time) int {
	ws, ok := s.sessions[accountID]
	if !ok {
		return 0
	}
	n := 0
	for id, served := range ws.sessions {
		if now.Sub(served) >= defaultSessionDecay {
			delete(ws.sessions, id)
			continue
		}
		n++
	}
	return n
}

// wouldExceed reports whether serving sessionID would push the account past
// the cap: false when the session is already in the set (a continuation), or
// when the counted set has room.
func (s *sessionWorkingSet) wouldExceed(accountID, sessionID string, now time.Time) bool {
	ws, ok := s.sessions[accountID]
	if !ok {
		return false
	}
	if now.Before(ws.fullUntil) {
		return true
	}
	if _, continued := ws.sessions[strings.TrimSpace(sessionID)]; continued {
		return false
	}
	return s.distinct(accountID, now) >= defaultWorkingSetCap
}

// pinFull records a real 01041: the vendor just said the account holds the
// cap, which outranks whatever the local count believed.
func (s *sessionWorkingSet) pinFull(accountID string, now time.Time) {
	s.wsFor(accountID).fullUntil = now.Add(defaultFullDecay)
}

// gc trims expired entries so the map cannot grow without bound.
func (s *sessionWorkingSet) gc(now time.Time) {
	for accountID, ws := range s.sessions {
		for id, served := range ws.sessions {
			if now.Sub(served) >= defaultSessionDecay {
				delete(ws.sessions, id)
			}
		}
		if len(ws.sessions) == 0 && now.After(ws.fullUntil) {
			delete(s.sessions, accountID)
		}
	}
}

// isSessionCapRefusal reports whether a queue-family refusal is specifically
// the session-cap one (TM.00001041, "并发会话数已达上限").  The broader queue
// family (peak-usage wording, TPM ceilings) says nothing about sessions; only
// the cap refusal is evidence for pinning the working set full.
func isSessionCapRefusal(message string) bool {
	return strings.Contains(message, "TM.00001041") || strings.Contains(message, "并发会话数已达上限")
}
