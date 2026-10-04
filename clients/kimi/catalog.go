package kimi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"client2api/internal/core"
	"client2api/internal/modelmeta"
)

// ---------------------------------------------------------------------------
// Model catalog
// ---------------------------------------------------------------------------

// metaProvider is the offline metadata fallback for the built-in catalog. It is
// deliberately built with no cache directory and no models.dev layer, so listing
// models never reads or writes the disk and never blocks on the network.
var metaProvider = modelmeta.New(modelmeta.Options{Client: "kimi"})

// builtinCatalog is the offline fallback catalog.  The CLI is not installed on
// the machine this module targets, so there is nothing to interrogate; these
// ids come from docs/upstream/kimi.md (the reference's two ids) and from the
// model cache of the installed kimi-desktop app (the three k3/k2d6 ids, which
// are unverified against the CLI).  Any id not listed here is still accepted by
// Chat and passed straight through to the CLI as `-m <id>`.
//
// None of these five ids is covered by an authoritative source for a context
// window or an output limit, so no such number is attached here: an empty field
// is correct, a guessed one would be a bug.  The modelmeta call is in place so a
// sourced number would flow through automatically, and it only ever adds
// metadata keys — it never marks an unverified id as verified.
func builtinCatalog() []core.Model {
	return []core.Model{
		builtinEntry("kimi", map[string]any{"source": "builtin", "default": true}),
		builtinEntry("kimi-k2", map[string]any{"source": "builtin"}),
		builtinEntry("k3-agent", map[string]any{"source": "kimi-desktop-model-cache", "verified": false}),
		builtinEntry("k3-agent-ultra", map[string]any{"source": "kimi-desktop-model-cache", "verified": false}),
		builtinEntry("k2d6-agent", map[string]any{"source": "kimi-desktop-model-cache", "verified": false}),
	}
}

// builtinEntry builds one offline catalog entry, filling the metadata fields
// modelmeta can source and leaving the rest absent rather than guessed.
func builtinEntry(id string, extra map[string]any) core.Model {
	if m, ok := metaProvider.Lookup(id); ok {
		modelmeta.FillExtra(extra, m, modelmeta.KeysCanonical)
	}
	return core.Model{ID: id, OwnedBy: ownerKimi, Extra: extra}
}

// catalog resolves the configured model list, falling back to the built-in one.
func catalog(cfg Config) []core.Model {
	if len(cfg.Models) == 0 {
		return builtinCatalog()
	}
	out := make([]core.Model, 0, len(cfg.Models))
	for _, id := range cfg.Models {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		out = append(out, core.Model{ID: id, OwnedBy: ownerKimi, Extra: map[string]any{"source": "config"}})
	}
	if len(out) == 0 {
		return builtinCatalog()
	}
	return out
}

func modelIDs(models []core.Model) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}

// ---------------------------------------------------------------------------
// Credential discovery
//
// The CLI owns its own token: it is refreshed on every prompt and lives in the
// OS keyring (service "kimi-code", key "oauth/kimi-code") or in a credentials
// file.  This module never needs the secret itself — it only needs to know
// whether a login exists, so that Chat can name the missing prerequisite
// instead of failing deep inside the CLI.
//
// The OS keyring cannot be read with the standard library, so a keyring-only
// login is reported as "not found" here.  Config.AssumeLoggedIn exists for that
// case.
// ---------------------------------------------------------------------------

// credentialEnvVars are the environment variables treated as an explicit
// credential.  Never logged, only their presence is reported.
var credentialEnvVars = []string{"KIMI_API_KEY", "KIMI_CODE_TOKEN", "KIMI_TOKEN"}

// credentialFileCandidates is the default probe list, mirroring the paths named
// in docs/upstream/kimi.md plus the obvious per-user locations.
func credentialFileCandidates() []string {
	home, _ := os.UserHomeDir()
	var out []string
	if home != "" {
		out = append(out,
			filepath.Join(home, ".kimi", "credentials", "kimi-code.json"),
			filepath.Join(home, ".kimi-code", "credentials", "kimi-code.json"),
			filepath.Join(home, ".kimi", "credentials.json"),
			filepath.Join(home, ".config", "kimi", "credentials.json"),
		)
	}
	if runtime.GOOS == "windows" {
		if appdata := os.Getenv("APPDATA"); appdata != "" {
			out = append(out,
				filepath.Join(appdata, "kimi", "credentials", "kimi-code.json"),
				filepath.Join(appdata, "kimi-code", "credentials", "kimi-code.json"),
			)
		}
	}
	return out
}

// credentialSource is one credential probe's outcome.
type credentialSource struct {
	Kind      string // "file" | "env"
	Ref       string // the path or variable name
	Found     bool
	ExpiresAt string // RFC3339, when the credential states one
	// Identity is the Kimi account the credential belongs to, read out of the
	// token the file or variable holds.  It never carries the token itself, and
	// it is empty when the credential does not name one.
	Identity string
	// Evidence is a change detector for what was found, and never the
	// credential itself: a file's size and modification time, or the length of
	// an environment variable's value.  The panel records it when an operator
	// deletes a row, and shows that row again once the evidence moves.
	Evidence string
	Note     string
}

// credentialReport is the cached outcome of a login probe.
type credentialReport struct {
	sources  []credentialSource
	searched []string
	checked  time.Time
}

func (r credentialReport) usable() bool {
	for _, s := range r.sources {
		if s.Found {
			return true
		}
	}
	return false
}

// identity is the account every credential in the report agrees on, or "" when
// the report is empty or the credentials do not agree.  The CLI's own store and
// the panel's own login are the same Kimi user, and this is what lets the panel
// say so; two credentials naming *different* users say nothing about the
// account, so the caller keeps them apart.
func (r credentialReport) identity() string {
	found := ""
	for _, s := range r.sources {
		if !s.Found || s.Identity == "" {
			continue
		}
		if found == "" {
			found = s.Identity
			continue
		}
		if found != s.Identity {
			return ""
		}
	}
	return found
}

// accounts renders the report as core.AccountStatus values for the panel.
func (r credentialReport) accounts(bin string, binErr error, cfg Config) []core.AccountStatus {
	var out []core.AccountStatus
	for _, s := range r.sources {
		if !s.Found {
			continue
		}
		state := "ready"
		note := s.Note
		if s.ExpiresAt != "" {
			if exp, err := time.Parse(time.RFC3339, s.ExpiresAt); err == nil && exp.Before(time.Now()) {
				state = "invalid"
				if note == "" {
					note = "credential has expired; the CLI refreshes it on the next prompt"
				}
			}
		}
		out = append(out, core.AccountStatus{
			ID:        s.Kind + ":" + filepath.Base(s.Ref),
			Label:     s.Ref,
			Enabled:   true,
			State:     state,
			ExpiresAt: s.ExpiresAt,
			Note:      note,
			Identity:  s.Identity,
			Extra:     map[string]any{"kind": s.Kind},
		})
	}
	if len(out) > 0 {
		return out
	}

	// No credential evidence at all: report the probe honestly.
	acct := core.AccountStatus{
		ID:      "cli-login",
		Label:   "kimi CLI login",
		Enabled: false,
		State:   "unknown",
		Note:    "no credential file found; the CLI may still hold a token in the OS keyring, which this module cannot read",
	}
	switch {
	case binErr != nil:
		acct.Note = "kimi CLI is not installed, so no login can exist"
	case cfg.AssumeLoggedIn:
		acct.Enabled = true
		acct.State = "ready"
		acct.Note = "assume_logged_in is set; the login check is skipped"
	}
	return []core.AccountStatus{acct}
}

// credentials probes (and caches) the credential locations.
func (c *Client) credentials() credentialReport {
	c.run.credMu.Lock()
	defer c.run.credMu.Unlock()
	if time.Since(c.run.credAt) < binaryCacheTTL && c.run.cred.checked.Unix() > 0 {
		return c.run.cred
	}

	files := c.cfg.CredentialFiles
	if len(files) == 0 {
		files = credentialFileCandidates()
	}
	rep := credentialReport{checked: time.Now()}
	for _, f := range files {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		rep.searched = append(rep.searched, f)
		src := credentialSource{Kind: "file", Ref: f}
		if b, err := os.ReadFile(f); err == nil {
			if hasCredentialField(b) {
				src.Found = true
				src.ExpiresAt = credentialExpiry(b)
				src.Identity = credentialIdentity(b)
				src.Evidence = fileEvidence(f, b)
			} else {
				src.Note = "file exists but holds no recognisable token field"
			}
		}
		rep.sources = append(rep.sources, src)
	}
	for _, name := range credentialEnvVars {
		src := credentialSource{Kind: "env", Ref: name}
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			src.Found = true
			src.Identity = core.JWTIdentity(v)
			src.Evidence = fmt.Sprintf("len=%d", len(v))
			src.Note = "environment credential " + core.MaskSecret(v)
		}
		rep.sources = append(rep.sources, src)
	}

	c.run.cred = rep
	c.run.credAt = time.Now()
	return rep
}

// fileEvidence describes a credential file well enough to notice that it has
// changed, without reading the credential out: its size and modification time.
// Both may be recorded in accounts.json when a deleted row is remembered, so
// neither may ever carry token material.
func fileEvidence(path string, data []byte) string {
	mod := int64(0)
	if fi, err := os.Stat(path); err == nil {
		mod = fi.ModTime().UnixNano()
	}
	return fmt.Sprintf("len=%d,mtime=%d", len(data), mod)
}

// credentialFieldNames are the keys a Kimi credential blob may hold its token
// under.  Every reader walks them in this order, so a blob that carries more
// than one agrees with itself about which is the token.
var credentialFieldNames = []string{"access_token", "accessToken", "token", "api_key", "apiKey", "id_token"}

// hasCredentialField reports whether a credentials JSON blob actually carries a
// token.  The value itself is never read out, logged or returned.
func hasCredentialField(b []byte) bool {
	_, ok := credentialToken(b)
	return ok
}

// credentialToken returns the token a credential blob holds, and whether it
// found one.  Callers must not keep or report it: the whole point of this file
// is that the credential itself never leaves the module.
func credentialToken(b []byte) (string, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return "", false
	}
	for _, key := range credentialFieldNames {
		raw, ok := m[key]
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			continue
		}
		if s = strings.TrimSpace(s); s != "" {
			return s, true
		}
	}
	return "", false
}

// credentialIdentity reads the account id out of a credential blob's token
// without returning the token.  The CLI's store names the Kimi user it belongs
// to, and that is what lets the panel show the CLI's login and the panel's own
// login as two channels of one account rather than two accounts.  A credential
// that is not a JWT — an API key, say — names no account, and then it stays
// unknown.
func credentialIdentity(b []byte) string {
	token, ok := credentialToken(b)
	if !ok {
		return ""
	}
	return core.JWTIdentity(token)
}

// credentialExpiry extracts an expiry timestamp when the file states one, as
// RFC3339, "2006-01-02 15:04:05" or a unix seconds/milliseconds number.  It
// returns "" when the file does not say.
func credentialExpiry(b []byte) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return ""
	}
	for _, key := range []string{"expires_at", "expiresAt", "expiry", "expires", "exp"} {
		raw, ok := m[key]
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
				if t, err := time.Parse(layout, s); err == nil {
					return t.Format(time.RFC3339)
				}
			}
			continue
		}
		var n float64
		if json.Unmarshal(raw, &n) == nil && n > 0 {
			sec := int64(n)
			if n > 1e12 { // milliseconds
				sec = int64(n / 1000)
			}
			return time.Unix(sec, 0).Format(time.RFC3339)
		}
	}
	return ""
}
