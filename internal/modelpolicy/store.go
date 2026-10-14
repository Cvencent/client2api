// Package modelpolicy persists the operator's per-account model blacklists.
//
// The panel writes one when an operator unchecks a model in the account's
// model dialog; the gateway reads it on every account acquisition and refuses
// that account for that model.  It exists because the enforcement point is the
// shared account hook, not the module: a platform whose module keeps no model
// policy of its own (everything except WorkBuddy today) still honours the
// operator's choice without every module having to learn about it.
//
// WorkBuddy keeps its own realm-aware blacklist and is not written here; the
// two never fight over one account because the panel prefers the module's own
// manager whenever it implements core.AccountModelPolicyManager.
package modelpolicy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type storeDoc struct {
	// Clients maps platform -> account id -> disabled model ids.  Model ids are
	// stored trimmed and lower-cased so a lookup never depends on how the
	// operator or the vendor spelled one.
	Clients map[string]map[string][]string `json:"clients,omitempty"`
}

// Store persists per-account model blacklists.  It is safe for concurrent use
// by the panel (which writes it) and the gateway (which reads it per request).
type Store struct {
	path string

	mu  sync.RWMutex
	doc storeDoc
}

// Open opens or creates a policy store.  An empty path creates an in-memory
// store, which keeps tests and embedders without a data directory simple.  A
// missing file is an empty store; a corrupt file is treated as empty and
// reported through the optional warning function, because losing the policy is
// recoverable (the operator re-checks the boxes) while refusing to boot is not.
func Open(path string, warnf ...func(string, ...any)) (*Store, error) {
	s := &Store{path: strings.TrimSpace(path)}
	s.doc = storeDoc{Clients: map[string]map[string][]string{}}
	if s.path == "" {
		return s, nil
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return s, nil
	}
	var doc storeDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		if len(warnf) > 0 && warnf[0] != nil {
			warnf[0]("modelpolicy: ignoring corrupt policy file %s: %v", s.path, err)
		}
		return s, nil
	}
	next := storeDoc{Clients: map[string]map[string][]string{}}
	for platform, accounts := range doc.Clients {
		platform = strings.TrimSpace(platform)
		if platform == "" {
			continue
		}
		for account, models := range accounts {
			account = strings.TrimSpace(account)
			if account == "" {
				continue
			}
			out := normalizeModels(models)
			if len(out) == 0 {
				continue
			}
			if next.Clients[platform] == nil {
				next.Clients[platform] = map[string][]string{}
			}
			next.Clients[platform][account] = out
		}
	}
	s.doc = next
	return s, nil
}

// Disabled returns a copy of the blacklist for one account, or nil when the
// account is fully open.
func (s *Store) Disabled(platform, account string) []string {
	if s == nil {
		return nil
	}
	platform, account = strings.TrimSpace(platform), strings.TrimSpace(account)
	if platform == "" || account == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	models := s.doc.Clients[platform][account]
	if len(models) == 0 {
		return nil
	}
	return append([]string(nil), models...)
}

// IsDisabled reports whether one model is switched off for one account.  An
// empty platform, account or model is never disabled: a module that did not
// name an account, or a request that carries no model, must not be gated by
// policy it cannot be matched against.
func (s *Store) IsDisabled(platform, account, model string) bool {
	if s == nil {
		return false
	}
	platform, account = strings.TrimSpace(platform), strings.TrimSpace(account)
	model = strings.ToLower(strings.TrimSpace(model))
	if platform == "" || account == "" || model == "" {
		return false
	}
	// openaicompat publishes its models as "<provider>/<model>" in the request
	// path but lists the bare "<model>" in the account record and the dialog,
	// so the operator's blacklist holds the bare id while req.Model is
	// qualified.  Check both spellings, but only the qualification by this very
	// account: matching an arbitrary prefix would let one provider's blacklist
	// gate another provider's model.
	bare := model
	if prefix := strings.ToLower(account) + "/"; strings.HasPrefix(model, prefix) {
		bare = strings.TrimPrefix(model, prefix)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, m := range s.doc.Clients[platform][account] {
		if m == model || (bare != model && m == bare) {
			return true
		}
	}
	return false
}

// Set replaces one account's blacklist and persists it.  An empty list drops
// the entry, so an account the operator never touched leaves no trace in the
// file -- and "back to fully open" is expressed by absence, exactly like the
// module-side policy WorkBuddy stores.
func (s *Store) Set(platform, account string, disabled []string) error {
	if s == nil {
		return errors.New("modelpolicy: nil store")
	}
	platform, account = strings.TrimSpace(platform), strings.TrimSpace(account)
	if platform == "" {
		return errors.New("modelpolicy: platform is required")
	}
	if account == "" {
		return errors.New("modelpolicy: account id is required")
	}
	models := normalizeModels(disabled)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.doc.Clients == nil {
		s.doc.Clients = map[string]map[string][]string{}
	}
	if len(models) == 0 {
		if accounts := s.doc.Clients[platform]; accounts != nil {
			delete(accounts, account)
			if len(accounts) == 0 {
				delete(s.doc.Clients, platform)
			}
		}
		return s.writeLocked()
	}
	accounts := s.doc.Clients[platform]
	if accounts == nil {
		accounts = map[string][]string{}
		s.doc.Clients[platform] = accounts
	}
	accounts[account] = models
	return s.writeLocked()
}

// Snapshot returns a deep copy of the whole document, for the bundle export.
func (s *Store) Snapshot() map[string]map[string][]string {
	if s == nil {
		return map[string]map[string][]string{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]map[string][]string, len(s.doc.Clients))
	for platform, accounts := range s.doc.Clients {
		cp := make(map[string][]string, len(accounts))
		for account, models := range accounts {
			if len(models) > 0 {
				cp[account] = append([]string(nil), models...)
			}
		}
		if len(cp) > 0 {
			out[platform] = cp
		}
	}
	return out
}

// Replace swaps the whole document for the supplied snapshot and persists it.
// It is the bundle restore path: unlike Set it also drops entries the incoming
// document does not carry, so an imported bundle is authoritative.
func (s *Store) Replace(snapshot map[string]map[string][]string) error {
	if s == nil {
		return errors.New("modelpolicy: nil store")
	}
	next := storeDoc{Clients: map[string]map[string][]string{}}
	for platform, accounts := range snapshot {
		platform = strings.TrimSpace(platform)
		if platform == "" {
			continue
		}
		for account, models := range accounts {
			account = strings.TrimSpace(account)
			if account == "" {
				continue
			}
			out := normalizeModels(models)
			if len(out) == 0 {
				continue
			}
			if next.Clients[platform] == nil {
				next.Clients[platform] = map[string][]string{}
			}
			next.Clients[platform][account] = out
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.doc = next
	return s.writeLocked()
}

// normalizeModels trims, lower-cases, dedupes and sorts a blacklist so the
// file is stable under hand edits and comparisons are exact.
func normalizeModels(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, m := range in {
		m = strings.ToLower(strings.TrimSpace(m))
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

func (s *Store) writeLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s.doc, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".model_policies-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace model policy file: %w", err)
	}
	return nil
}
