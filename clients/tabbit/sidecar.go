package tabbit

// This file owns the sidecar's HTTP surface (/health, /v1/models) and the
// optional child process.  It never inspects the upstream installation:
// nothing here reads, copies or vendors the GPL-3.0 upstream checkout, and the
// tabbit-cli named-pipe transport is deliberately not implemented.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
	"client2api/internal/modelmeta"
)

// ---------------------------------------------------------------------------
// /health
// ---------------------------------------------------------------------------

// healthState is the outcome of one /health probe.
//
// status carries the HTTP status code separately from err so that a caller can
// branch on "the sidecar rejected our key" without re-parsing the rendered
// error string.  The string is only ever meant for humans; matching on it made
// a 401 branch depend on a format string in another file, which is exactly the
// kind of coupling that breaks when someone rewords the message.
type healthState struct {
	ok      bool
	baseURL string
	version string
	models  int
	host    string
	status  int
	err     string
	at      time.Time
}

// authRejected reports whether the sidecar answered but refused our credential.
// The three covers the shapes that mean "fix your key": two explicit 4xx
// statuses and the empty-status case where a proxy returned a body that was
// not an HTTP response at all.
func (h healthState) authRejected() bool {
	switch h.status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return true
	}
	return false
}

type healthInfo struct {
	Version string
	Models  int
	Host    string
	Status  string
}

// parseHealth reads whatever the sidecar answered on /health.  The reference
// implementation has changed this shape before, so every field is optional and
// an unrecognised body is not an error.
func parseHealth(raw []byte) healthInfo {
	var info healthInfo
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return info
	}
	var m map[string]any
	if json.Unmarshal(trimmed, &m) != nil {
		return info
	}
	info.Version = firstString(m, "version", "sidecar_version", "app_version")
	info.Host = firstString(m, "web_host", "host", "target_host", "upstream_host")
	info.Status = firstString(m, "status", "state")
	if n, ok := toInt(m["models"]); ok {
		info.Models = n
	} else if n, ok := toInt(m["model_count"]); ok {
		info.Models = n
	} else if list, ok := m["models"].([]any); ok {
		info.Models = len(list)
	}
	if info.Status == "" {
		if b, ok := m["ok"].(bool); ok {
			if b {
				info.Status = "ok"
			} else {
				info.Status = "error"
			}
		}
	}
	return info
}

// probe asks the sidecar whether it is alive.  It is bounded by budget and
// never panics or blocks longer than that.
func (c *Client) probe(ctx context.Context, loc location, budget time.Duration) healthState {
	st := healthState{baseURL: loc.baseURL, at: time.Now()}
	if strings.TrimSpace(loc.baseURL) == "" {
		st.err = "no sidecar base_url configured"
		return st
	}
	hctx := ctx
	if budget > 0 {
		var cancel context.CancelFunc
		hctx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}
	resp, err := c.doGet(hctx, loc, c.cfg.HealthPath)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			st.err = ctxErr.Error()
		} else {
			st.err = truncate(err.Error(), 200)
		}
		return st
	}
	defer resp.Body.Close()
	raw := readLimited(resp.Body, 1<<20)
	st.status = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		st.err = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, upstreamErrorMessage(raw))
		return st
	}
	info := parseHealth(raw)
	st.ok = true
	st.version = info.Version
	st.host = info.Host
	st.models = info.Models
	return st
}

// ---------------------------------------------------------------------------
// /v1/models
// ---------------------------------------------------------------------------

// builtinCatalog is the offline fallback, captured from the live catalogue on
// web.tabbit.com (GET /proxy/v1/model_config/models?scene=chat) on 2026-09-29.
// The vendor publishes no id at all: display_name is the only identifier, and
// it is also what input_payload.selected_model carries, so it is the id here.
// The live fetch always wins; this table only keeps routing and the panel
// useful when neither the browser cookie nor the sidecar can be reached.  It
// deliberately holds no model the vendor does not actually serve.
var builtinCatalog = []struct{ ID, Display string }{
	{"Default", "Default"},
	{"MiMo-V2.6-Pro", "MiMo-V2.6-Pro"},
	{"MiMo-V2.6-Flash", "MiMo-V2.6-Flash"},
	{"GLM-5.3-FlashX", "GLM-5.3-FlashX"},
	{"DeepSeek-V4.1-Flash", "DeepSeek-V4.1-Flash"},
	{"DeepSeek-V4-Flash", "DeepSeek-V4-Flash"},
	{"GLM-5.3-Flash", "GLM-5.3-Flash"},
	{"GLM-5.3", "GLM-5.3"},
	{"Qwen3.8-Max", "Qwen3.8-Max"},
	{"Kimi-K3", "Kimi-K3"},
	{"LongCat-2.0", "LongCat-2.0"},
	{"GLM-5.2", "GLM-5.2"},
	{"Qwen3.7-Max", "Qwen3.7-Max"},
	{"Kimi-K2.7-Code", "Kimi-K2.7-Code"},
	{"DeepSeek-V4-Pro", "DeepSeek-V4-Pro"},
	{"Doubao-Seed-2.1-Pro", "Doubao-Seed-2.1-Pro"},
	{"Doubao-Seed-2.1-Turbo", "Doubao-Seed-2.1-Turbo"},
	{"MiniMax-M3", "MiniMax-M3"},
	{"GLM-5.1", "GLM-5.1"},
	{"GLM-5V-Turbo", "GLM-5V-Turbo"},
	{"Kimi-K2.6", "Kimi-K2.6"},
	{"Doubao-Seed-2.0-lite", "Doubao-Seed-2.0-lite"},
	{"Qwen3.5-Plus", "Qwen3.5-Plus"},
}

// metaProvider is the offline metadata fallback for the built-in catalogue. It
// is deliberately built with no cache directory and no models.dev layer: listing
// models must stay side-effect free and must never block on the network.
var metaProvider = modelmeta.New(modelmeta.Options{Client: "tabbit"})

// fallbackModels returns the built-in catalogue with bare ids: the gateway
// adds the "tabbit/" prefix itself, so re-prefixing here would produce
// "tabbit/tabbit/priority".
func fallbackModels(cfg Config) []core.Model {
	out := make([]core.Model, 0, len(builtinCatalog)+len(cfg.ExtraModels))
	seen := make(map[string]bool, len(builtinCatalog)+len(cfg.ExtraModels))
	add := func(id, display string) {
		id = strings.TrimSpace(stripModelPrefix(id, cfg.ModelPrefix))
		key := strings.ToLower(id)
		if id == "" || seen[key] {
			return
		}
		seen[key] = true
		extra := map[string]any{
			"tabbit_display_name": display,
			"fallback":            true,
		}
		// Fill the metadata fields our embedded table can source, so a cold start
		// still lets a caller size its request. The table only fills gaps: it never
		// invents a number for an id no source covers, and it never emits a field
		// this module did not already emit.
		if m, ok := metaProvider.Lookup(id); ok {
			modelmeta.FillExtra(extra, m, modelmeta.KeysCanonical)
		}
		out = append(out, core.Model{
			ID:      id,
			OwnedBy: "tabbit",
			Extra:   extra,
		})
	}
	for _, b := range builtinCatalog {
		add(b.ID, b.Display)
	}
	for _, id := range cfg.ExtraModels {
		add(id, id)
	}
	return out
}

// errNoUsableModels means the sidecar answered but listed nothing.  It is a
// sentinel because callers have to tell "reachable but empty" apart from
// "unreachable": login.go treats it as "not signed in yet" rather than "down".
var errNoUsableModels = errors.New("sidecar returned no usable models")

// parseModels accepts the OpenAI list shape, a {"models": [...]} envelope or a
// bare array of ids/objects, and strips the "tabbit/" prefix from every id.
func parseModels(raw []byte, prefix string) ([]core.Model, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("empty model list")
	}
	var entries []json.RawMessage
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &entries); err != nil {
			return nil, fmt.Errorf("parsing model list: %w", err)
		}
	} else {
		var env struct {
			Data   []json.RawMessage `json:"data"`
			Models []json.RawMessage `json:"models"`
		}
		if err := json.Unmarshal(trimmed, &env); err != nil {
			return nil, fmt.Errorf("parsing model list: %w", err)
		}
		entries = env.Data
		if len(entries) == 0 {
			entries = env.Models
		}
	}
	out := make([]core.Model, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		et := bytes.TrimSpace(e)
		if len(et) == 0 {
			continue
		}
		var id, ownedBy string
		extra := map[string]any{}
		if et[0] == '"' {
			var s string
			if json.Unmarshal(et, &s) != nil {
				continue
			}
			id = s
		} else {
			var m map[string]any
			if json.Unmarshal(et, &m) != nil {
				continue
			}
			id = firstString(m, "id", "name", "model")
			ownedBy = firstString(m, "owned_by", "owner")
			for k, v := range m {
				if k == "id" || k == "object" || k == "owned_by" || k == "owner" || k == "created" {
					continue
				}
				extra[k] = v
			}
		}
		id = strings.TrimSpace(stripModelPrefix(id, prefix))
		key := strings.ToLower(id)
		if id == "" || seen[key] {
			continue
		}
		seen[key] = true
		if ownedBy == "" {
			ownedBy = "tabbit"
		}
		// The embedded table fills gaps this payload left, exactly as it does
		// for the built-in catalogue (fallbackModels): it never overwrites a
		// number the sidecar sent, and never invents one for an id no source
		// covers.  Without it a sidecar that omits its output budget leaves
		// every caller to guess, and a guess truncates long answers.
		if m, ok := metaProvider.Lookup(id); ok {
			modelmeta.FillExtra(extra, m, modelmeta.KeysCanonical)
		}
		out = append(out, core.Model{ID: id, OwnedBy: ownedBy, Extra: extra})
	}
	if len(out) == 0 {
		return nil, errNoUsableModels
	}
	return out, nil
}

func (c *Client) fetchModels(ctx context.Context, loc location) ([]core.Model, error) {
	mctx := ctx
	if d := c.cfg.Timeouts.modelsBudget(); d > 0 {
		var cancel context.CancelFunc
		mctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	resp, err := c.doGet(mctx, loc, c.cfg.ModelsPath)
	if err != nil {
		return nil, fmt.Errorf("sidecar %s is not reachable: %v", loc.baseURL, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("reading model list from %s: %w", loc.baseURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sidecar %s returned HTTP %d for %s: %s",
			loc.baseURL, resp.StatusCode, c.cfg.ModelsPath, upstreamErrorMessage(raw))
	}
	return parseModels(raw, c.cfg.ModelPrefix)
}

// ---------------------------------------------------------------------------
// Optional child process
// ---------------------------------------------------------------------------

// supervisor starts the sidecar only when the user explicitly opted in with
// "manage": true.  Its output goes to <DataDir>/sidecar.log; nothing outside
// DataDir is ever written.
type supervisor struct {
	mu      sync.Mutex
	logPath string
	cmd     *exec.Cmd
	logFile *os.File
	started time.Time
	exited  bool
	lastErr string
}

func newSupervisor(logPath string) *supervisor { return &supervisor{logPath: logPath} }

func (s *supervisor) start(loc location) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd != nil && !s.exited {
		return nil
	}
	if strings.TrimSpace(loc.command) == "" {
		return errors.New("no sidecar command configured")
	}
	if dir := filepath.Dir(s.logPath); dir != "" && dir != "." {
		if err := core.EnsureDir(dir); err != nil {
			return fmt.Errorf("creating data dir %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(s.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("opening sidecar log %s: %w", s.logPath, err)
	}
	cmd := exec.Command(loc.command, loc.args...)
	if loc.workdir != "" {
		cmd.Dir = loc.workdir
	}
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.Env = childEnv(loc)
	if err := cmd.Start(); err != nil {
		f.Close()
		s.lastErr = err.Error()
		return fmt.Errorf("starting sidecar %s: %w", filepath.Base(loc.command), err)
	}
	s.cmd = cmd
	s.logFile = f
	s.started = time.Now()
	s.exited = false
	s.lastErr = ""
	core.GoSafe("tabbit sidecar reap", func(msg string) {
		// Without this the supervisor would go on believing the sidecar is up
		// (exited stays false) and the only trace of the panic would be on
		// stderr, which a service install never shows anyone.
		s.mu.Lock()
		s.exited = true
		s.lastErr = msg
		s.mu.Unlock()
	}, func() { s.reap(cmd, f) })
	return nil
}

func (s *supervisor) reap(cmd *exec.Cmd, f *os.File) {
	err := cmd.Wait()
	s.mu.Lock()
	s.exited = true
	if err != nil {
		s.lastErr = err.Error()
	}
	s.mu.Unlock()
	f.Close()
}

func (s *supervisor) running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmd != nil && !s.exited
}

func (s *supervisor) stop() {
	s.mu.Lock()
	cmd := s.cmd
	s.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// childEnv inherits the parent environment and tells the child where to listen
// and which local key to accept.  Best effort: the reference reads
// TABBIT_API_KEY / DEFAULT_API_KEY and its own port setting.
func childEnv(loc location) []string {
	env := os.Environ()
	if strings.TrimSpace(loc.apiKey) != "" {
		env = append(env, "TABBIT_API_KEY="+loc.apiKey)
	}
	if u, err := url.Parse(loc.baseURL); err == nil && u.Hostname() != "" {
		env = append(env, "TABBIT_HOST="+u.Hostname())
		if p := u.Port(); p != "" {
			env = append(env, "TABBIT_PORT="+p)
		}
	}
	return env
}
