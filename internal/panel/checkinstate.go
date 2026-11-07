package panel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// DefaultCheckinStateFileName is the relative path under the data directory.
const DefaultCheckinStateFileName = "panel/checkin_state.json"

var checkinDayLocation = time.FixedZone("CST", 8*60*60)

type checkinStateEntry struct {
	Day     string `json:"day"`
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
	At      string `json:"at,omitempty"`
	// Data carries the vendor's per-run detail (campaign/claimable counts for
	// qoder, and anything else a module chooses to record).  Without it the
	// campaign list the vendor reported at 06:00 is unreplayable: the App can
	// show a claimable activity the module's campaign API never returned, and
	// the one-line message cannot say which side lied (issue #236).
	Data map[string]any `json:"data,omitempty"`
}

type checkinStateFile struct {
	Version int                                                `json:"version"`
	Clients map[string]map[string]map[string]checkinStateEntry `json:"clients"`
}

// CheckinStore remembers definitive check-in outcomes for the current
// Shanghai calendar day.  It is shared by the panel and the scheduler so a
// scheduled run and an operator click agree on which buttons are already
// settled.
type CheckinStore struct {
	mu        sync.Mutex
	persistMu sync.Mutex
	path      string
	now       func() time.Time
	// errs remembers the last disk failure: checkin_state.json is the only
	// record that "this account already settled today", so a write that
	// silently vanished used to surface only as a duplicate-claim risk after
	// the next restart (issue #34).
	errs    storeErrTracker
	clients map[string]map[string]map[string]checkinStateEntry
}

// NewCheckinStore opens the persistent state file.  A missing or corrupt file
// is treated as empty: this is display state, never a reason to fail startup.
func NewCheckinStore(path string, now func() time.Time) *CheckinStore {
	if now == nil {
		now = time.Now
	}
	s := &CheckinStore{
		path:    strings.TrimSpace(path),
		now:     now,
		clients: map[string]map[string]map[string]checkinStateEntry{},
	}
	s.load()
	return s
}

func (s *CheckinStore) load() {
	if s == nil || s.path == "" {
		return
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var doc checkinStateFile
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Clients == nil {
		return
	}
	s.clients = doc.Clients
}

func (s *CheckinStore) today() string {
	return s.now().In(checkinDayLocation).Format("2006-01-02")
}

// Record stores one definitive result.  Failures without a vendor verdict
// (transport, auth, rate limit) are deliberately ignored.
func (s *CheckinStore) Record(platform, account, action string, res core.CheckinResult) {
	if s == nil {
		return
	}
	platform = strings.TrimSpace(platform)
	account = strings.TrimSpace(account)
	action = strings.TrimSpace(action)
	if action == "" {
		action = strings.TrimSpace(res.Action)
	}
	if account == "" {
		account = strings.TrimSpace(res.AccountID)
	}
	if action == "" {
		action = "default"
	}
	if platform == "" || account == "" {
		return
	}

	state := ""
	switch {
	case res.Skipped:
		state = core.CheckinStateUnavailable
	case res.OK:
		state = core.CheckinStateDone
	default:
		return
	}

	message := strings.TrimSpace(res.Message)
	if message == "" {
		message = strings.TrimSpace(res.Error)
	}
	message = core.Redact(message)
	at := strings.TrimSpace(res.At)
	if at == "" {
		at = s.now().Format(time.RFC3339)
	}

	s.mu.Lock()
	byAccount := s.clients[platform]
	if byAccount == nil {
		byAccount = map[string]map[string]checkinStateEntry{}
		s.clients[platform] = byAccount
	}
	byAction := byAccount[account]
	if byAction == nil {
		byAction = map[string]checkinStateEntry{}
		byAccount[account] = byAction
	}
	byAction[action] = checkinStateEntry{
		Day:     s.today(),
		State:   state,
		Message: message,
		At:      at,
		Data:    res.Data,
	}
	s.mu.Unlock()

	s.persist()
}

// Lookup returns today's status for one platform/account/action.
func (s *CheckinStore) Lookup(platform, account, action string) (core.CheckinStatus, bool) {
	if s == nil {
		return core.CheckinStatus{}, false
	}
	platform = strings.TrimSpace(platform)
	account = strings.TrimSpace(account)
	action = strings.TrimSpace(action)
	if action == "" {
		action = "default"
	}
	if platform == "" || account == "" {
		return core.CheckinStatus{}, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	byAccount := s.clients[platform]
	if byAccount == nil {
		return core.CheckinStatus{}, false
	}
	byAction := byAccount[account]
	if byAction == nil {
		return core.CheckinStatus{}, false
	}
	entry, ok := byAction[action]
	if !ok || entry.Day != s.today() {
		return core.CheckinStatus{}, false
	}
	return core.CheckinStatus{State: entry.State, Message: entry.Message, At: entry.At}, true
}

// Snapshot returns today's statuses for every account under one platform.
func (s *CheckinStore) Snapshot(platform string) map[string]map[string]core.CheckinStatus {
	out := map[string]map[string]core.CheckinStatus{}
	if s == nil {
		return out
	}
	platform = strings.TrimSpace(platform)
	if platform == "" {
		return out
	}
	today := s.today()

	for account, byAction := range s.clients[platform] {
		for action, entry := range byAction {
			if entry.Day != today {
				continue
			}
			if out[account] == nil {
				out[account] = map[string]core.CheckinStatus{}
			}
			out[account][action] = core.CheckinStatus{State: entry.State, Message: entry.Message, At: entry.At, Data: entry.Data}
		}
	}
	return out
}

func (s *CheckinStore) persist() {
	if s == nil || s.path == "" {
		return
	}
	s.persistMu.Lock()
	defer s.persistMu.Unlock()

	today := s.today()
	s.mu.Lock()
	doc := checkinStateFile{
		Version: 1,
		Clients: map[string]map[string]map[string]checkinStateEntry{},
	}
	for platform, byAccount := range s.clients {
		for account, byAction := range byAccount {
			for action, entry := range byAction {
				if entry.Day != today {
					continue
				}
				if doc.Clients[platform] == nil {
					doc.Clients[platform] = map[string]map[string]checkinStateEntry{}
				}
				if doc.Clients[platform][account] == nil {
					doc.Clients[platform][account] = map[string]checkinStateEntry{}
				}
				doc.Clients[platform][account][action] = entry
			}
		}
	}
	s.mu.Unlock()

	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		s.errs.failed("check-in state", err)
		return
	}
	if err := core.WriteFileAtomic(s.path, append(raw, '\n')); err != nil {
		s.errs.failed("check-in state", err)
	}
}

// LastError exposes the most recent disk failure to the overview payload
// (issue #34); "" when every write has landed.
func (s *CheckinStore) LastError() string { return s.errs.LastError() }

func (p *panel) initCheckins() {
	if p == nil {
		return
	}
	if p.opts.Checkins != nil {
		p.checkins = p.opts.Checkins
		return
	}
	path := ""
	if strings.TrimSpace(p.opts.DataDir) != "" {
		path = filepath.Join(p.opts.DataDir, filepath.FromSlash(DefaultCheckinStateFileName))
	}
	p.checkins = NewCheckinStore(path, time.Now)
}

func (p *panel) decorateAccountCheckins(platform string, list []core.AccountRecord) {
	if p == nil || p.checkins == nil || len(list) == 0 {
		return
	}
	snapshot := p.checkins.Snapshot(platform)
	for i := range list {
		byAction := snapshot[list[i].ID]
		if len(byAction) == 0 {
			continue
		}
		if list[i].Checkins == nil {
			list[i].Checkins = map[string]core.CheckinStatus{}
		}
		for action, status := range byAction {
			list[i].Checkins[action] = status
		}
	}
}

func (p *panel) recordCheckinResult(platform, account, action string, res core.CheckinResult) {
	if p == nil || p.checkins == nil {
		return
	}
	p.checkins.Record(platform, account, action, res)
}
