package panel

import (
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"sort"

	"client2api/internal/core"
)

// secretConfigKey matches the config keys whose VALUES are credentials.  It
// exists because core.RedactAny redacts each value string in isolation: a bare
// secret sitting under a key named "api_key" is not caught by any of core's
// patterns, so the key itself has to be the signal.
var secretConfigKey = regexp.MustCompile(`(?i)token|secret|passwd|password|api[_-]?key|apikey|auth|cookie|credential|session|bearer|private|(^|[^a-z])keys?([^a-z]|$)`)

type configResponse struct {
	Path      string `json:"path"`
	Version   string `json:"version"`
	Addr      string `json:"addr"`
	APIKeySet bool   `json:"api_key_set"`
	// CanRestart reports whether POST /panel/api/restart is wired.  The page
	// hides the button when it is not, instead of offering a click that can
	// only fail.
	CanRestart bool     `json:"can_restart"`
	Clients    []string `json:"clients"`
	Registered []string `json:"registered"`
	// ModelGroupDefaults lists the built-in GPT-6+ routing groups that are
	// currently active. Config.model_groups carries the effective groups the
	// editor should show; this list lets the UI mark the built-in rows apart
	// from operator-declared ones without duplicating the defaults in JS.
	ModelGroupDefaults []string `json:"model_group_defaults"`
	// Config is the raw config re-serialised after redaction.  It is `any`
	// rather than a map so that "unknown path" renders as null, exactly as the
	// contract says, instead of an empty object the panel would index into.
	Config any `json:"config"`
	// Panel is the dashboard's own presentation config, already resolved
	// against its defaults.  It is reported next to Config rather than left to
	// be dug out of it because Config is the file as written: an operator who
	// never set package_detail_limit would find nothing there, and the page
	// would then have to keep its own copy of the default -- which is exactly
	// how the two drift apart.  The page reads this field, and only this field.
	Panel panelSettings `json:"panel"`
}

// panelSettings is the resolved dashboard presentation config.  Every field is
// effective, never "unset": the caller resolves defaults before constructing
// the panel, so a page that reads this needs no fallback logic of its own.
type panelSettings struct {
	PackageDetailLimit int `json:"package_detail_limit"`
}

// handleConfig reads and writes the configuration file.  The file holds
// account tokens, so nothing is ever served raw: GET redacts it, and the write
// path treats the redaction sentinel as "leave what is on disk alone" so a save
// can never blank a credential the editor never showed.
func (p *panel) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		p.configRead(w, r)
	case http.MethodPatch, http.MethodPost, http.MethodPut:
		p.configWrite(w, r)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "use GET to read or PATCH to save")
	}
}

// configRead reports the configuration as it is on disk, redacted.
func (p *panel) configRead(w http.ResponseWriter, r *http.Request) {
	resp := configResponse{
		Path:       p.opts.ConfigPath,
		Version:    p.opts.Version,
		Addr:       p.opts.Listen,
		APIKeySet:  p.opts.AuthEnabled,
		CanRestart: p.opts.Restart != nil,
		Clients:    []string{},
		Registered: core.Registered(),
		Panel:      panelSettings{PackageDetailLimit: p.opts.PackageDetailLimit},
	}
	if p.opts.Registry != nil {
		resp.Clients = p.opts.Registry.Names()
	}

	// No path was threaded through from cmd: say so instead of guessing.
	if p.opts.ConfigPath == "" {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	raw, err := os.ReadFile(p.opts.ConfigPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "reading config: "+err.Error())
		return
	}

	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		writeErr(w, http.StatusInternalServerError, "config is not valid JSON: "+err.Error())
		return
	}
	resp.Config = redactConfig(parsed)
	p.exposeModelGroups(&resp)
	writeJSON(w, http.StatusOK, resp)
}

// exposeModelGroups puts the effective routing groups into the config payload.
// The file remains the authority on operator overrides, but a built-in GPT-6
// group is absent from that file until it is edited. Showing the registry's
// effective view is what makes the built-in names appear in the editor and
// keeps their displayed members and priorities identical to runtime routing.
func (p *panel) exposeModelGroups(resp *configResponse) {
	if p == nil || p.opts.Registry == nil || resp == nil {
		return
	}
	groups := p.opts.Registry.ModelGroups()
	projected := make(map[string]any, len(groups))
	defaults := make([]string, 0, len(groups))
	for name, group := range groups {
		members := make([]string, 0, len(group.Members))
		for _, member := range group.Members {
			members = append(members, member.Client+"/"+member.Model)
		}
		entry := map[string]any{"members": members}
		if len(group.PlatformPriorities) > 0 {
			priorities := make(map[string]int, len(group.PlatformPriorities))
			for platform, priority := range group.PlatformPriorities {
				priorities[platform] = priority
			}
			entry["platform_priorities"] = priorities
		}
		if group.Builtin {
			entry["builtin"] = true
			defaults = append(defaults, name)
		}
		projected[name] = entry
	}
	sort.Strings(defaults)
	resp.ModelGroupDefaults = defaults
	if cfg, ok := resp.Config.(map[string]any); ok {
		cfg["model_groups"] = projected
	}
}

// redactConfig walks a decoded JSON document, masking the value of any key
// that names a credential and passing everything else through core.Redact.
// It rebuilds rather than mutating, so the caller's document is untouched.
func redactConfig(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if k == "sources" {
				out[k] = redactSourceDefinitions(val)
				continue
			}
			if secretConfigKey.MatchString(k) {
				out[k] = maskSecret(val)
				continue
			}
			out[k] = redactConfig(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = redactConfig(val)
		}
		return out
	case string:
		return core.Redact(t)
	default:
		return v
	}
}

func redactSourceDefinitions(v any) any {
	sources, ok := v.(map[string]any)
	if !ok {
		return redactConfig(v)
	}
	out := make(map[string]any, len(sources))
	for id, entry := range sources {
		fields, ok := entry.(map[string]any)
		if !ok {
			out[id] = redactConfig(entry)
			continue
		}
		row := make(map[string]any, len(fields))
		for key, value := range fields {
			switch key {
			case "label", "base_url", "disabled", "max_tokens_field":
				row[key] = core.RedactAny(value)
			default:
				if secretConfigKey.MatchString(key) {
					row[key] = maskSecret(value)
				} else {
					row[key] = redactConfig(value)
				}
			}
		}
		out[id] = row
	}
	return out
}

// maskSecret blanks a credential value while keeping the shape the panel needs:
// an empty string stays empty (so "not set" is still distinguishable from
// "set"), and a container keeps its keys with every value masked.
func maskSecret(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if t == "" {
			return ""
		}
		return "<redacted>"
	case map[string]any:
		out := make(map[string]any, len(t))
		for k := range t {
			out[k] = "<redacted>"
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = "<redacted>"
		}
		return out
	default:
		return "<redacted>"
	}
}
