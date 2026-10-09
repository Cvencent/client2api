package panel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"client2api/internal/core"
)

// redactedPlaceholder is the sentinel GET /panel/api/config puts in place of
// every credential.  It is echoed back by the editor for any field the operator
// did not touch, and it must mean "keep what is on disk" — writing the literal
// would destroy a working credential.  Anything the operator *does* type, be it
// a new secret or the empty string, is written verbatim.
const redactedPlaceholder = "<redacted>"

// maxAccountNoteRunes bounds the operator's own per-account label (the phone
// number or e-mail a credential signs in with).  The value is shown verbatim
// in the account table, so an unbounded string would let one careless paste
// bloat the config and wreck the layout.
const maxAccountNoteRunes = 120

// configWriteMu serialises read-modify-write cycles on the config file.  The
// panel is a single instance per process, but the panel is not the only writer
// a user might have open (an editor is), and two concurrent saves would
// otherwise both read the old file and one would win.
var configWriteMu sync.Mutex

// configKeyOrder is the order the writer emits top-level keys in, so a saved
// file keeps looking hand-written rather than being alphabetised.  Keys the
// running binary does not know about are preserved and appended, because
// dropping them would be silent data loss.
var configKeyOrder = []string{"listen", "api_key", "data_dir", "proxy", "aliases", "disabled", "platforms", "schedule", "pool", "cooldown", "prompt", "session_sticky", "features", "panel", "clients"}

// configSaveResponse is the GET payload plus what the save actually did.
type configSaveResponse struct {
	configResponse
	Saved           bool     `json:"saved"`
	RestartRequired bool     `json:"restart_required"`
	Changed         []string `json:"changed"`
}

// configWrite merges a partial document into the config file.
//
// The contract, in full:
//
//   - The body is an object whose keys are the top-level config keys.  Only
//     the keys present are touched; everything else is left alone.
//   - null deletes a key (a single alias, a single module setting, or a whole
//     "clients"."name" entry).
//   - The string "<redacted>" anywhere means "leave the value on disk alone".
//   - Two objects merge key by key, so a patch can change one alias without
//     having to re-send the others.
//   - Anything else replaces the previous value.
//
// The result is validated, then written atomically.  Nothing takes effect
// until the process restarts: every module reads its configuration once, at
// construction, so the response says so rather than pretending otherwise.
func (p *panel) configWrite(w http.ResponseWriter, r *http.Request) {
	var patch map[string]any
	if !decodeJSON(w, r, &patch) {
		return
	}
	changed, merged, err := writeConfigKeys(p.opts.ConfigPath, patch)
	if err != nil {
		// A patch the writer refused, or a request against a process that
		// has no config file, is the caller's problem.  A file that cannot be
		// read or replaced is ours.
		var invalid *configInvalidError
		if errors.Is(err, errNoConfigPath) || errors.Is(err, errEmptyPatch) || errors.As(err, &invalid) {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	resp := configSaveResponse{
		configResponse: configResponse{
			Path:       p.opts.ConfigPath,
			Version:    p.opts.Version,
			Addr:       p.opts.Listen,
			APIKeySet:  p.opts.AuthEnabled,
			CanRestart: p.opts.Restart != nil,
			Clients:    []string{},
			Config:     redactConfig(merged),
		},
		Saved:   true,
		Changed: changed,
		// Only the keys a running process reads once need a restart; telling the
		// operator to restart after a hot-only save would send them to do work
		// the hot reload already did.
		RestartRequired: coldChangesRequireRestart(changed),
	}
	if p.opts.Registry != nil {
		resp.Clients = p.opts.Registry.Names()
	}
	writeJSON(w, http.StatusOK, resp)
}

var (
	// errNoConfigPath is what a save against a process started without a
	// config file has to say: there is nowhere to write.
	errNoConfigPath = errors.New("this process was started without a config path, so there is nothing to save to")
	// errEmptyPatch guards the deep merge, which would otherwise rewrite the
	// file unchanged and report success.
	errEmptyPatch = errors.New("empty patch: send an object with the keys to change")
)

// configInvalidError marks a patch the writer refused on its merits.  The HTTP
// layer turns it into a 400 and everything else into a 500, and a second
// caller in-process (the tray's settings dialog) reports it verbatim.
type configInvalidError struct{ err error }

func (e *configInvalidError) Error() string { return e.err.Error() }

func (e *configInvalidError) Unwrap() error { return e.err }

// WriteConfigKeys merges a partial document into the config file at path and
// reports the top-level keys that actually changed.
//
// This is the panel's own save, exposed so that a second front end -- the
// tray's settings dialog, which has no HTTP client and no session -- cannot
// grow a second read-merge-write cycle beside it.  Two writers that disagree
// about redaction, unknown-key preservation or atomic replacement would
// eventually lose somebody's configuration.
//
// The patch follows the same contract as the HTTP endpoint: only the keys
// present are touched, and nested objects merge key by key.
func WriteConfigKeys(path string, patch map[string]any) ([]string, error) {
	changed, _, err := writeConfigKeys(path, patch)
	return changed, err
}

// writeConfigKeys is the single read-merge-validate-write cycle.  It returns
// the merged document as well as the changed keys so the HTTP layer can
// render its response without reading the file a second time.
func writeConfigKeys(path string, patch map[string]any) ([]string, map[string]any, error) {
	if path == "" {
		return nil, nil, errNoConfigPath
	}
	if len(patch) == 0 {
		return nil, nil, errEmptyPatch
	}
	configWriteMu.Lock()
	defer configWriteMu.Unlock()

	onDisk, err := readConfigMap(path)
	if err != nil {
		return nil, nil, fmt.Errorf("reading config: %w", err)
	}
	merged := mergeConfig(onDisk, patch)
	if err := validateConfig(merged); err != nil {
		return nil, nil, &configInvalidError{err: err}
	}
	out, err := marshalConfigOrdered(merged)
	if err != nil {
		return nil, nil, fmt.Errorf("encoding config: %w", err)
	}
	if err := core.WriteFileAtomic(path, out); err != nil {
		return nil, nil, fmt.Errorf("writing config: %w", err)
	}
	return changedKeys(onDisk, merged), merged, nil
}

// coldChangesRequireRestart reports whether any of the keys a save touched is
// one the live reload cannot apply.
func coldChangesRequireRestart(changed []string) bool {
	for _, k := range changed {
		if coldConfigKeys[k] {
			return true
		}
	}
	return false
}

// readConfigMap loads this panel's config as a generic document.
func (p *panel) readConfigMap() (map[string]any, error) {
	return readConfigMap(p.opts.ConfigPath)
}

// readConfigMap loads a config file as a generic document.  A missing file is
// an empty document rather than an error: the process happily runs with no
// config at all, so the panel must be able to create one.
func readConfigMap(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// mergeConfig applies a patch over a base document without mutating either.
func mergeConfig(base, patch map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(patch))
	for k, v := range base {
		out[k] = v
	}
	for k, pv := range patch {
		if pv == nil {
			delete(out, k)
			continue
		}
		if isRedacted(pv) {
			continue // keep whatever is on disk
		}
		if bm, ok := out[k].(map[string]any); ok {
			if pm, ok := pv.(map[string]any); ok {
				out[k] = mergeConfig(bm, pm)
				continue
			}
		}
		out[k] = pv
	}
	return out
}

// isRedacted reports whether a patch value is the "unchanged" sentinel.  Only a
// scalar can be one: an object that merely contains the sentinel still needs to
// be walked so its sibling keys apply.
func isRedacted(v any) bool {
	s, ok := v.(string)
	return ok && s == redactedPlaceholder
}

// validateConfig type-checks the document the way the process will read it.
// Without this a typo saved from the editor would only surface as a failed
// startup, long after the save looked successful.
func validateConfig(cfg map[string]any) error {
	for _, k := range []string{"listen", "api_key", "data_dir", "proxy"} {
		v, ok := cfg[k]
		if !ok || v == nil {
			continue
		}
		if _, ok := v.(string); !ok {
			return fmt.Errorf("%s must be a string", k)
		}
	}
	if v, ok := cfg["aliases"]; ok && v != nil {
		m, ok := v.(map[string]any)
		if !ok {
			return errors.New("aliases must be an object of alias -> client/model")
		}
		for alias, target := range m {
			s, ok := target.(string)
			if !ok {
				return fmt.Errorf("alias %q must map to a string", alias)
			}
			if strings.TrimSpace(s) == "" {
				return fmt.Errorf("alias %q has an empty target", alias)
			}
			if !strings.Contains(s, "/") {
				return fmt.Errorf("alias %q target %q must be \"client/model\"", alias, s)
			}
		}
	}
	if v, ok := cfg["disabled"]; ok && v != nil {
		arr, ok := v.([]any)
		if !ok {
			return errors.New("disabled must be an array of client names")
		}
		for _, e := range arr {
			if _, ok := e.(string); !ok {
				return errors.New("disabled must contain only client names")
			}
		}
	}
	if v, ok := cfg["panel"]; ok && v != nil {
		m, ok := v.(map[string]any)
		if !ok {
			return errors.New("panel must be an object")
		}
		if n, ok := m["package_detail_limit"]; ok && n != nil {
			f, ok := n.(float64)
			if !ok {
				return errors.New("panel.package_detail_limit must be a number")
			}
			if f != float64(int(f)) {
				return errors.New("panel.package_detail_limit must be a whole number")
			}
		}
	}
	if v, ok := cfg["platforms"]; ok && v != nil {
		m, ok := v.(map[string]any)
		if !ok {
			return errors.New("platforms must be an object keyed by client name")
		}
		for name, entry := range m {
			if entry == nil {
				// A null entry clears the platform's policy, which is the
				// documented default: priority 0, nothing disabled.
				continue
			}
			em, ok := entry.(map[string]any)
			if !ok {
				return fmt.Errorf("platform %q configuration must be an object", name)
			}
			if p, ok := em["priority"]; ok && p != nil {
				f, ok := p.(float64)
				if !ok {
					return fmt.Errorf("platform %q priority must be a number", name)
				}
				if f != float64(int(f)) {
					return fmt.Errorf("platform %q priority must be a whole number", name)
				}
			}
			if d, ok := em["disabled_models"]; ok && d != nil {
				arr, ok := d.([]any)
				if !ok {
					return fmt.Errorf("platform %q disabled_models must be an array of model ids", name)
				}
				for _, e := range arr {
					if _, ok := e.(string); !ok {
						return fmt.Errorf("platform %q disabled_models must contain only model ids", name)
					}
				}
			}
			if m, ok := em["max_in_flight"]; ok && m != nil {
				f, ok := m.(float64)
				if !ok {
					return fmt.Errorf("platform %q max_in_flight must be a number", name)
				}
				if f != float64(int(f)) || f < 0 {
					return fmt.Errorf("platform %q max_in_flight must be a whole number >= 0", name)
				}
			}
			if m, ok := em["max_in_flight_per_account"]; ok && m != nil {
				f, ok := m.(float64)
				if !ok {
					return fmt.Errorf("platform %q max_in_flight_per_account must be a number", name)
				}
				if f != float64(int(f)) || f < 0 {
					return fmt.Errorf("platform %q max_in_flight_per_account must be a whole number >= 0", name)
				}
			}
			// The low-balance guard: 0 parks a known zero balance, a positive
			// number parks at or below it, and -1 turns the guard off.
			if m, ok := em["reserve_credits"]; ok && m != nil {
				f, ok := m.(float64)
				if !ok {
					return fmt.Errorf("platform %q reserve_credits must be a number", name)
				}
				if f != float64(int(f)) || f < -1 {
					return fmt.Errorf("platform %q reserve_credits must be a whole number >= -1 (-1 turns the guard off)", name)
				}
			}
			if aps, ok := em["account_priorities"]; ok && aps != nil {
				m, ok := aps.(map[string]any)
				if !ok {
					return fmt.Errorf("platform %q account_priorities must be an object keyed by account id", name)
				}
				for id, raw := range m {
					if raw == nil {
						continue
					}
					f, ok := raw.(float64)
					if !ok || f != float64(int(f)) {
						return fmt.Errorf("platform %q account priority for %q must be a whole number", name, id)
					}
				}
			}
			if notes, ok := em["account_notes"]; ok && notes != nil {
				m, ok := notes.(map[string]any)
				if !ok {
					return fmt.Errorf("platform %q account_notes must be an object keyed by account id", name)
				}
				for id, raw := range m {
					if raw == nil {
						continue
					}
					s, ok := raw.(string)
					if !ok {
						return fmt.Errorf("platform %q account note for %q must be a string", name, id)
					}
					if utf8.RuneCountInString(s) > maxAccountNoteRunes {
						return fmt.Errorf("platform %q account note for %q is longer than %d characters", name, id, maxAccountNoteRunes)
					}
				}
			}
		}
	}
	if v, ok := cfg["clients"]; ok && v != nil {
		m, ok := v.(map[string]any)
		if !ok {
			return errors.New("clients must be an object keyed by client name")
		}
		for name, c := range m {
			if c == nil {
				// A null module entry means "use this module's defaults",
				// which is exactly what an empty object means too.
				continue
			}
			if _, ok := c.(map[string]any); !ok {
				return fmt.Errorf("client %q configuration must be an object", name)
			}
		}
	}
	if v, ok := cfg["schedule"]; ok && v != nil {
		if err := validateScheduleConfig(v); err != nil {
			return err
		}
	}
	return nil
}

// marshalConfigOrdered renders the document with the top-level keys in their
// canonical order.  A plain map would be alphabetised, which turns every save
// into a whole-file diff.
// validateScheduleConfig type-checks the automation timetable before it is
// written.  The scheduler normalizes duplicates and sorts hours, but a bad
// hour or a wrong shape has to be refused here: the write happens first and
// the reload afterwards, so a saved-but-unloadable file would leave the
// process running yesterday's timetable with no way to tell.
func validateScheduleConfig(v any) error {
	m, ok := v.(map[string]any)
	if !ok {
		return errors.New("schedule must be an object")
	}
	for _, key := range []string{
		"enabled",
		"checkin_enabled", "travel_enabled", "activity_enabled",
		"keepalive_enabled", "blackcat_enabled", "growth_enabled",
		"balance_refresh_enabled",
		"daily_balance_enabled",
	} {
		if b, ok := m[key]; ok && b != nil {
			if _, ok := b.(bool); !ok {
				return fmt.Errorf("schedule.%s must be a boolean", key)
			}
		}
	}
	for _, key := range []string{
		"checkin_hours", "travel_hours", "activity_hours",
		"keepalive_hours", "blackcat_hours", "growth_hours",
		"daily_balance_hours",
	} {
		if h, ok := m[key]; ok && h != nil {
			if err := validateScheduleHours("schedule."+key, h); err != nil {
				return err
			}
		}
	}
	if n, ok := m["balance_refresh_minutes"]; ok && n != nil {
		f, ok := n.(float64)
		if !ok || f != float64(int(f)) {
			return errors.New("schedule.balance_refresh_minutes must be a whole number")
		}
	}
	c, ok := m["clients"]
	if !ok || c == nil {
		return nil
	}
	cm, ok := c.(map[string]any)
	if !ok {
		return errors.New("schedule.clients must be an object keyed by client name")
	}
	for client, entry := range cm {
		if entry == nil {
			continue // null clears every override for this platform
		}
		em, ok := entry.(map[string]any)
		if !ok {
			return fmt.Errorf("schedule.clients.%s must be an object keyed by batch name", client)
		}
		for batch, ov := range em {
			if ov == nil {
				continue // null clears this one override
			}
			om, ok := ov.(map[string]any)
			if !ok {
				return fmt.Errorf("schedule.clients.%s.%s must be an object", client, batch)
			}
			if b, ok := om["enabled"]; ok && b != nil {
				if _, ok := b.(bool); !ok {
					return fmt.Errorf("schedule.clients.%s.%s.enabled must be a boolean", client, batch)
				}
			}
			if h, ok := om["hours"]; ok && h != nil {
				if err := validateScheduleHours("schedule.clients."+client+"."+batch+".hours", h); err != nil {
					return err
				}
			}
			if a, ok := om["accounts"]; ok && a != nil {
				am, ok := a.(map[string]any)
				if !ok {
					return fmt.Errorf("schedule.clients.%s.%s.accounts must be an object", client, batch)
				}
				if mode, ok := am["mode"]; ok && mode != nil {
					s, ok := mode.(string)
					if !ok || (s != "platform" && s != "include" && s != "exclude") {
						return fmt.Errorf("schedule.clients.%s.%s.accounts.mode must be platform, include or exclude", client, batch)
					}
				}
				for _, key := range []string{"include", "exclude"} {
					if raw, ok := am[key]; ok && raw != nil {
						arr, ok := raw.([]any)
						if !ok {
							return fmt.Errorf("schedule.clients.%s.%s.accounts.%s must be an array of account ids", client, batch, key)
						}
						for _, item := range arr {
							if _, ok := item.(string); !ok {
								return fmt.Errorf("schedule.clients.%s.%s.accounts.%s must contain only strings", client, batch, key)
							}
						}
					}
				}
			}
		}
	}
	return nil
}

// validateScheduleHours checks one hour list: 0-23, whole numbers, nothing
// else.  Duplicates and order are the scheduler's business, not the writer's.
func validateScheduleHours(name string, v any) error {
	arr, ok := v.([]any)
	if !ok {
		return fmt.Errorf("%s must be an array of hours 0-23", name)
	}
	for _, e := range arr {
		f, ok := e.(float64)
		if !ok || f != float64(int(f)) || f < 0 || f > 23 {
			return fmt.Errorf("%s must contain only whole hours 0-23", name)
		}
	}
	return nil
}

func marshalConfigOrdered(cfg map[string]any) ([]byte, error) {
	keys := make([]string, 0, len(cfg))
	seen := make(map[string]bool, len(cfg))
	for _, k := range configKeyOrder {
		if _, ok := cfg[k]; ok {
			keys = append(keys, k)
			seen[k] = true
		}
	}
	rest := make([]string, 0, len(cfg))
	for k := range cfg {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	keys = append(keys, rest...)

	var buf bytes.Buffer
	buf.WriteString("{\n")
	for i, k := range keys {
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		var vb bytes.Buffer
		enc := json.NewEncoder(&vb)
		enc.SetEscapeHTML(false)
		enc.SetIndent("  ", "  ")
		if err := enc.Encode(cfg[k]); err != nil {
			return nil, err
		}
		buf.WriteString("  ")
		buf.Write(kb)
		buf.WriteString(": ")
		buf.Write(bytes.TrimRight(vb.Bytes(), "\n"))
		if i < len(keys)-1 {
			buf.WriteByte(',')
		}
		buf.WriteByte('\n')
	}
	buf.WriteString("}\n")
	return buf.Bytes(), nil
}

// changedKeys lists the top-level keys whose stored value differs after the
// merge, so the panel can say what it wrote instead of a bare "saved".
func changedKeys(before, after map[string]any) []string {
	keys := make(map[string]bool, len(before)+len(after))
	for k := range before {
		keys[k] = true
	}
	for k := range after {
		keys[k] = true
	}
	out := make([]string, 0, len(keys))
	for k := range keys {
		b, bok := before[k]
		a, aok := after[k]
		if bok != aok || !jsonEqual(b, a) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// jsonEqual compares two decoded JSON values by their canonical encoding.
func jsonEqual(a, b any) bool {
	ab, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}
