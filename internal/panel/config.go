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
//
// The words are deliberately compound-specific.  Bare "session" and "auth"
// used to be on this list, and they matched CONFIG BLOCK names, not credential
// fields: session_sticky (a map of booleans and durations) and
// sources.*.auth_cooldown (a cooldown) came back fully "<redacted>", the
// configuration page rendered that placeholder into the inputs, and every
// save then failed the duration validation on a value the operator cannot
// even see (issue #13).  A credential key names the credential itself --
// session_token, auth_token, authorization -- and a cooldown named
// auth_cooldown is not one.
var secretConfigKey = regexp.MustCompile(`(?i)api[_-]?key|apikey|token([^a-z]|$)|secret|passwd|password|cookie|credential|bearer|private[_-]?key|(^|[^a-z])key([^a-z]|$)|keys?$|authorization|session[_-]?(token|key|id)|auth[_-]?(token|key|header)`)

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
	// ModuleSchema is the panel's curated, plain-language description of what
	// each module reads under clients.<name>.  The config file keeps that section
	// as raw JSON because every module decides its own keys; this table is what
	// lets the page render labelled controls instead of an unlabelled blob.
	ModuleSchema map[string][]moduleOption `json:"module_schema"`
	// BuiltinSources names the platforms shipped by this binary.  Config may
	// still define or override them, so the panel receives the effective
	// source table in Config and this marker tells it which rows must not be
	// presented as an operator-added relay.
	BuiltinSources []string `json:"builtin_sources"`
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
		Path:         p.opts.ConfigPath,
		Version:      p.opts.Version,
		Addr:         p.opts.Listen,
		APIKeySet:    p.opts.AuthEnabled,
		CanRestart:   p.opts.Restart != nil,
		Clients:      []string{},
		Registered:   core.Registered(),
		Panel:        panelSettings{PackageDetailLimit: p.opts.PackageDetailLimit},
		ModuleSchema: moduleOptions,
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
	p.exposeBuiltinSources(&resp)
	p.exposeModelGroups(&resp)
	writeJSON(w, http.StatusOK, resp)
}

// exposeBuiltinSources projects shipped platforms into the effective config
// the panel reads, without writing them back to disk.  An operator's entry
// always wins; a missing entry is filled from the shipped default so the
// platform has the same account and scan controls as a normal relay.
func (p *panel) exposeBuiltinSources(resp *configResponse) {
	if resp == nil {
		return
	}
	defaults := core.BuiltinSourceDefaults()
	if len(defaults) == 0 {
		return
	}
	resp.BuiltinSources = make([]string, 0, len(defaults))
	for id := range defaults {
		resp.BuiltinSources = append(resp.BuiltinSources, id)
	}
	sort.Strings(resp.BuiltinSources)
	cfg, ok := resp.Config.(map[string]any)
	if !ok {
		return
	}
	sources, _ := cfg["sources"].(map[string]any)
	if sources == nil {
		sources = make(map[string]any, len(defaults))
		cfg["sources"] = sources
	}
	for id, def := range defaults {
		row, _ := sources[id].(map[string]any)
		if row == nil {
			row = make(map[string]any, 2)
			sources[id] = row
		}
		if _, ok := row["label"]; !ok && def.Label != "" {
			row["label"] = def.Label
		}
		if _, ok := row["base_url"]; !ok {
			row["base_url"] = def.BaseURL
		}
	}
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
			if nameKeyedConfigMap(k) {
				out[k] = redactNameKeyedMap(val)
				continue
			}
			if secretConfigKey.MatchString(k) {
				out[k] = maskSecret(val)
				continue
			}
			if k == "egress" {
				out[k] = redactEgressConfig(val)
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

// redactEgressConfig masks the subscription/proxy URL, which is a bearer
// credential for most providers, while leaving mode and the executable path
// visible so the config page can still explain what is configured.
func redactEgressConfig(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return redactConfig(v)
	}
	out := redactConfig(m).(map[string]any)
	if raw, ok := m["url"]; ok && raw != nil {
		out["url"] = maskSecret(raw)
	}
	return out
}

// nameKeyedConfigMap reports whether a config key names a map whose own keys are
// operator-chosen names or ids rather than field names.  The credential
// heuristic must not run on those inner keys: a platform called "tokenrhythm"
// or a client called "tokenmetrics" is a name, not a secret, and masking it
// would hide the whole policy from the panel.  The values inside each entry are
// still redacted by the ordinary rules.
func nameKeyedConfigMap(key string) bool {
	switch key {
	case "platforms", "clients", "model_groups", "aliases",
		"account_priorities", "account_notes", "platform_priorities":
		return true
	}
	return false
}

// redactNameKeyedMap redacts the values of a name-keyed map while leaving the
// entry names untouched.  A non-map value falls back to the normal walk, so a
// malformed config still cannot smuggle a credential through untouched.
func redactNameKeyedMap(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return redactConfig(v)
	}
	out := make(map[string]any, len(m))
	for name, entry := range m {
		out[name] = redactConfig(entry)
	}
	return out
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
// "set"), and a container has only its STRING members masked: a block that
// carries a credential next to plain settings keeps those settings readable,
// because a boolean or a duration under a credential-ish key is still just a
// boolean or a duration (issue #13).
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
		for k, val := range t {
			out[k] = maskSecret(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = maskSecret(val)
		}
		return out
	default:
		// Numbers, booleans and durations are not credentials; masking them
		// turned session_sticky's ttl into "<redacted>" and wedged the page.
		return t
	}
}
