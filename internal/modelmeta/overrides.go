package modelmeta

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Override is an operator-supplied model metadata override. A zero field means
// "no manual value for this field", so context and output can be restored
// independently.
type Override struct {
	ContextLength   int64 `json:"context_length,omitempty"`
	MaxOutputTokens int64 `json:"max_output_tokens,omitempty"`
}

// IsZero reports whether the override carries no manual value.
func (o Override) IsZero() bool {
	return o.ContextLength <= 0 && o.MaxOutputTokens <= 0
}

type overrideDoc struct {
	Clients map[string]map[string]Override `json:"clients"`
}

// OverrideStore persists operator-supplied model metadata overrides. It is safe
// for concurrent use by the panel and the gateway.
type OverrideStore struct {
	path string

	mu  sync.RWMutex
	doc overrideDoc
}

// OpenOverrideStore opens or creates an override store. An empty path creates
// an in-memory-only store, which keeps tests and callers without a data
// directory simple. A missing file is an empty store; a corrupt file is treated
// as empty and reported through the optional warning function.
func OpenOverrideStore(path string, warnf ...func(string, ...any)) (*OverrideStore, error) {
	s := &OverrideStore{path: strings.TrimSpace(path)}
	s.doc = overrideDoc{Clients: map[string]map[string]Override{}}
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
	var doc overrideDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		if len(warnf) > 0 && warnf[0] != nil {
			warnf[0]("modelmeta: ignoring corrupt override file %s: %v", s.path, err)
		}
		return s, nil
	}
	if doc.Clients == nil {
		doc.Clients = map[string]map[string]Override{}
	}
	s.doc = doc
	return s, nil
}

// Get returns the override for one client/model pair.
func (s *OverrideStore) Get(client, model string) (Override, bool) {
	if s == nil {
		return Override{}, false
	}
	client, model = strings.TrimSpace(client), strings.TrimSpace(model)
	if client == "" || model == "" {
		return Override{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	models := s.doc.Clients[client]
	if models == nil {
		return Override{}, false
	}
	o, ok := models[model]
	if !ok || o.IsZero() {
		return Override{}, false
	}
	return o, true
}

// Set replaces the manual values for one client/model pair. A non-positive
// field removes that field's manual value.
func (s *OverrideStore) Set(client, model string, contextLength, maxOutputTokens int64) error {
	if s == nil {
		return errors.New("modelmeta: nil override store")
	}
	client, model = strings.TrimSpace(client), strings.TrimSpace(model)
	if client == "" || model == "" {
		return errors.New("modelmeta: client and model are required")
	}
	if contextLength < 0 || maxOutputTokens < 0 {
		return errors.New("modelmeta: overrides must not be negative")
	}
	o := Override{ContextLength: contextLength, MaxOutputTokens: maxOutputTokens}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.doc.Clients == nil {
		s.doc.Clients = map[string]map[string]Override{}
	}
	models := s.doc.Clients[client]
	if o.IsZero() {
		if models != nil {
			delete(models, model)
			if len(models) == 0 {
				delete(s.doc.Clients, client)
			}
		}
		return s.writeLocked()
	}
	if models == nil {
		models = map[string]Override{}
		s.doc.Clients[client] = models
	}
	models[model] = o
	return s.writeLocked()
}

// Delete removes all manual values for one client/model pair.
func (s *OverrideStore) Delete(client, model string) error {
	return s.Set(client, model, 0, 0)
}

// Snapshot returns a deep copy of all overrides.
func (s *OverrideStore) Snapshot() map[string]map[string]Override {
	if s == nil {
		return map[string]map[string]Override{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]map[string]Override, len(s.doc.Clients))
	for client, models := range s.doc.Clients {
		if len(models) == 0 {
			continue
		}
		cp := make(map[string]Override, len(models))
		for model, o := range models {
			if !o.IsZero() {
				cp[model] = o
			}
		}
		if len(cp) > 0 {
			out[client] = cp
		}
	}
	return out
}

func (s *OverrideStore) writeLocked() error {
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
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".model_context-*.tmp")
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
		return fmt.Errorf("replace override file: %w", err)
	}
	return nil
}
