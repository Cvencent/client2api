package tabbit

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"client2api/internal/fingerprint"
)

const (
	// defaultAPIKey is the sidecar's documented static local bearer
	// (TABBIT_API_KEY / DEFAULT_API_KEY upstream).  It is a well-known local
	// constant, not a user secret, but it is still never logged in full.
	defaultAPIKey = "sk-tabbit-local"

	// defaultModelPrefix is the namespace the sidecar prefixes its ids with.
	defaultModelPrefix = "tabbit/"

	// stateFileName is the optional, user-managed hand-off file inside DataDir
	// (data/tabbit/sidecar.json).
	stateFileName = "sidecar.json"
)

// defaultCandidates are the endpoints probed, in order, when neither the config
// nor the environment names one.  127.0.0.1:50124 is the sidecar's documented
// default.  It is a variable so tests can aim it at a port that is certainly
// closed, which keeps the offline suite hermetic.
var defaultCandidates = []string{"http://127.0.0.1:50124"}

// Account kinds.  A "sidecar" account names a local tabbit2api bridge; a
// "web-token" account carries the Tabbit browser's own session cookie, which
// lets this module talk to the vendor directly (see web.go).  An empty Kind is
// read as "sidecar", which is what every account added before kinds existed is.
const (
	kindSidecar  = "sidecar"
	kindWebToken = "web-token"
	// kindCookie is the Discover() kind of the browser session cookie.  It is
	// not an account kind: the cookie becomes a "web-token" account on import.
	kindCookie = "browser-cookie"
)

// Transport values for Config.Transport.
const (
	transportAuto    = "auto"
	transportWeb     = "web"
	transportSidecar = "sidecar"
)

// installCandidates are the likely install locations of the sidecar executable,
// checked in order when "manage" is true and no command was configured.  They
// are only ever *stat*-ed: this package never reads, copies or vendors anything
// from the upstream checkout.
var installCandidates = []string{
	`%LOCALAPPDATA%\tabbit2api\bin\tabbit2api.cmd`,
	`%LOCALAPPDATA%\tabbit2api\node_modules\.bin\tabbit2api.cmd`,
	`%LOCALAPPDATA%\tabbit2api\src\cli.js`,
	`%APPDATA%\npm\tabbit2api.cmd`,
	`%APPDATA%\npm\tabbit2api`,
	`%ProgramFiles%\tabbit2api\tabbit2api.exe`,
}

// pathNames are looked up on PATH after the install candidates.
var pathNames = []string{"tabbit2api", "tabbit2api.cmd", "tabbit2api.bat", "tabbit2api.exe"}

// Config is the schema of the "clients"."tabbit" object in
// configs/client2api.json.  Every field is optional; the zero value means
// "discover everything, manage nothing".
type Config struct {
	// BaseURL is the sidecar's HTTP root, e.g. "http://127.0.0.1:50124".
	BaseURL string `json:"base_url"`
	// APIKey is the sidecar's local bearer.  Defaults to the documented
	// "sk-tabbit-local".
	APIKey string `json:"api_key"`
	// Manage, when true, lets the module *start* the sidecar executable.  It is
	// off by default: a module must never launch a browser unasked.
	Manage bool `json:"manage"`
	// Command/Args/Workdir describe how to start the sidecar when Manage is
	// true.  When Command is empty it is discovered (installCandidates, PATH).
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Workdir string   `json:"workdir"`
	// Path overrides, for a sidecar that moved its endpoints.
	HealthPath string `json:"health_path"`
	ModelsPath string `json:"models_path"`
	ChatPath   string `json:"chat_path"`
	// WebHost is the browser host the sidecar is *supposed* to drive, e.g.
	// "web.tabbit.com".  Purely informational: it is reported in Status() so
	// host drift (the sidecar hardcodes web.tabbit.ai) is visible.
	WebHost string `json:"web_host"`
	// ModelPrefix is the namespace the sidecar uses.  Defaults to "tabbit/".
	ModelPrefix string `json:"model_prefix"`
	// ExtraModels are ids appended to the built-in fallback catalog.
	ExtraModels []string `json:"extra_models"`
	// ExtraHeaders are added to every upstream request.
	ExtraHeaders map[string]string `json:"extra_headers"`
	// IncludeUsage asks the sidecar for a usage chunk (stream_options).  Nil
	// means true; set it to false if the sidecar rejects stream_options.
	IncludeUsage *bool `json:"include_usage"`

	// Transport chooses how a chat request leaves this process: "sidecar" (the
	// local tabbit2api bridge), "web" (the vendor's own web API, using a
	// web-token account) or "auto" (web when a web credential exists and has
	// not been rejected, otherwise the sidecar).  Default "auto".
	Transport string `json:"transport"`
	// WebBaseURL overrides the vendor's web root.  Default
	// "https://web.tabbit.com".  The web endpoints do not follow the sidecar
	// base_url: they are the vendor's own host.
	WebBaseURL string `json:"web_base_url"`
	// WebToken is a static web credential, used when no account was added
	// through the panel.  Adding an account is preferable: the panel can
	// disable and remove it, and only the store is ever rewritten.
	WebToken string `json:"web_token"`
	// TabbitCLI is the path of the Tabbit browser's own command line, which the
	// 导入凭据 action uses to read the session cookie out of the running
	// browser.  Empty means "discover it" (see webimport.go).
	TabbitCLI string `json:"tabbit_cli"`

	// TLSProfile/TLSProtocol impersonate a browser's TLS ClientHello.  The
	// web transport talks to the vendor's own site with a real session
	// cookie, so a Go-shaped handshake is a detectable tell; "chrome" is the
	// faithful choice.  Empty keeps the stock handshake.
	TLSProfile  string `json:"tls_profile"`
	TLSProtocol string `json:"tls_protocol"`

	Timeouts Timeouts `json:"timeouts"`
}

// Timeouts are the module's budgets, in seconds.  Zero means "use the default".
type Timeouts struct {
	// StatusSeconds bounds the probe Status() may perform (default 0.9).
	StatusSeconds float64 `json:"status_seconds"`
	// HealthSeconds bounds the probe used by Chat/Models (default 1.5).
	HealthSeconds float64 `json:"health_seconds"`
	// ModelsSeconds bounds GET /v1/models (default 10).
	ModelsSeconds float64 `json:"models_seconds"`
	// RequestSeconds bounds one chat request end to end (default 900; 0 = no
	// deadline beyond the caller's context).
	RequestSeconds float64 `json:"request_seconds"`
	// StartSeconds bounds waiting for a managed sidecar to become healthy
	// (default 20).
	StartSeconds float64 `json:"start_seconds"`
	// CacheSeconds is how long a probe result is reused (default 5).
	CacheSeconds float64 `json:"cache_seconds"`
	// ModelsCacheSeconds is how long a fetched catalog is reused (default 60).
	ModelsCacheSeconds float64 `json:"models_cache_seconds"`
}

func secondsOr(v, def float64) time.Duration {
	if v <= 0 {
		v = def
	}
	return time.Duration(v * float64(time.Second))
}

func (t Timeouts) statusBudget() time.Duration  { return secondsOr(t.StatusSeconds, 0.9) }
func (t Timeouts) healthBudget() time.Duration  { return secondsOr(t.HealthSeconds, 1.5) }
func (t Timeouts) modelsBudget() time.Duration  { return secondsOr(t.ModelsSeconds, 10) }
func (t Timeouts) requestBudget() time.Duration { return secondsOr(t.RequestSeconds, 900) }
func (t Timeouts) startBudget() time.Duration   { return secondsOr(t.StartSeconds, 20) }
func (t Timeouts) cacheTTL() time.Duration      { return secondsOr(t.CacheSeconds, 5) }
func (t Timeouts) modelsCacheTTL() time.Duration {
	return secondsOr(t.ModelsCacheSeconds, 60)
}

// parseConfig decodes the module's own config object.  A nil/absent object is
// not an error: it just means "nothing was configured".
func parseConfig(raw json.RawMessage) (Config, error) {
	var cfg Config
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return cfg, nil
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("clients.tabbit: %w", err)
	}
	return cfg, nil
}

// normalize applies the documented defaults.
func (cfg Config) normalize() Config {
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	cfg.WebHost = strings.TrimSpace(cfg.WebHost)
	cfg.Command = strings.TrimSpace(cfg.Command)
	cfg.Workdir = strings.TrimSpace(cfg.Workdir)
	if strings.TrimSpace(cfg.ModelPrefix) == "" {
		cfg.ModelPrefix = defaultModelPrefix
	}
	cfg.ModelPrefix = strings.TrimSpace(cfg.ModelPrefix)
	if !strings.HasSuffix(cfg.ModelPrefix, "/") {
		cfg.ModelPrefix += "/"
	}
	if strings.TrimSpace(cfg.HealthPath) == "" {
		cfg.HealthPath = "/health"
	}
	if strings.TrimSpace(cfg.ModelsPath) == "" {
		cfg.ModelsPath = "/v1/models"
	}
	if strings.TrimSpace(cfg.ChatPath) == "" {
		cfg.ChatPath = "/v1/chat/completions"
	}
	cfg.Transport = strings.ToLower(strings.TrimSpace(cfg.Transport))
	switch cfg.Transport {
	case transportWeb, transportSidecar, transportAuto:
	default:
		cfg.Transport = transportAuto
	}
	cfg.WebBaseURL = strings.TrimRight(strings.TrimSpace(cfg.WebBaseURL), "/")
	cfg.WebToken = strings.TrimSpace(cfg.WebToken)
	cfg.TabbitCLI = strings.TrimSpace(cfg.TabbitCLI)
	cfg.TLSProfile = strings.TrimSpace(cfg.TLSProfile)
	cfg.TLSProtocol = strings.TrimSpace(cfg.TLSProtocol)
	return cfg
}

// tlsProfile / tlsProtocol expose the transport-fingerprint keys to New.
func (cfg Config) tlsProfile() fingerprint.Profile {
	return fingerprint.Profile(cfg.TLSProfile)
}

func (cfg Config) tlsProtocol() fingerprint.Protocol {
	return fingerprint.Protocol(cfg.TLSProtocol)
}

func (cfg Config) includeUsage() bool {
	if cfg.IncludeUsage == nil {
		return true
	}
	return *cfg.IncludeUsage
}

// transport is the normalised transport choice.  An unparsed config (cfgErr)
// leaves the zero value, which reads as "auto".
func (cfg Config) transport() string {
	switch strings.ToLower(strings.TrimSpace(cfg.Transport)) {
	case transportWeb:
		return transportWeb
	case transportSidecar:
		return transportSidecar
	default:
		return transportAuto
	}
}

// ---------------------------------------------------------------------------
// Where the sidecar is
// ---------------------------------------------------------------------------

// location is the resolved sidecar endpoint plus the way to start it.
type location struct {
	baseURL   string
	apiKey    string
	keySource string // config | env | state | builtin
	webHost   string // the browser host the login URL opens ("" means the default)
	webSource string // config | panel
	command   string
	args      []string
	workdir   string
	manage    bool
	source    string // config | env | state | probe | default
}

// explicit reports whether the user (config, environment or the state file)
// named the endpoint, as opposed to us assuming the documented default.
func (l location) explicit() bool { return l.source != "default" && l.baseURL != "" }

// stateFile is the optional hand-off file data/tabbit/sidecar.json.
type stateFile struct {
	BaseURL string   `json:"base_url"`
	APIKey  string   `json:"api_key"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Workdir string   `json:"workdir"`
}

func (c *Client) env(key string) string {
	if c.getenv == nil {
		return ""
	}
	return strings.TrimSpace(c.getenv(key))
}

func (c *Client) readState() stateFile {
	var st stateFile
	if c.deps.DataDir == "" {
		return st
	}
	// A missing or unreadable state file is not an error: it is optional.
	_ = readJSONFile(filepath.Join(c.deps.DataDir, stateFileName), &st)
	return st
}

// locate resolves the sidecar without any network access.
//
// Precedence, highest first: config, environment, data/tabbit/sidecar.json,
// the documented default endpoint.
func (c *Client) locate() location {
	loc := location{
		manage:  c.cfg.Manage,
		command: c.cfg.Command,
		args:    c.cfg.Args,
		workdir: c.cfg.Workdir,
	}
	st := c.readState()
	storedEP, hasStored := c.firstEnabledStored()

	switch {
	case c.cfg.BaseURL != "":
		loc.baseURL, loc.source = c.cfg.BaseURL, "config"
	case c.env("CLIENT2API_TABBIT_BASE_URL") != "":
		loc.baseURL, loc.source = strings.TrimRight(c.env("CLIENT2API_TABBIT_BASE_URL"), "/"), "env"
	case c.env("TABBIT_BASE_URL") != "":
		loc.baseURL, loc.source = strings.TrimRight(c.env("TABBIT_BASE_URL"), "/"), "env"
	case hasStored:
		// An endpoint added through the panel outranks the hand-off file and
		// the default probe: the operator put it there on purpose.
		loc.baseURL, loc.source = storedEP.BaseURL, epOriginPanel
	case strings.TrimSpace(st.BaseURL) != "":
		loc.baseURL, loc.source = strings.TrimRight(strings.TrimSpace(st.BaseURL), "/"), "state"
	default:
		loc.baseURL, loc.source = firstCandidate(), "default"
	}

	switch {
	case c.cfg.APIKey != "":
		loc.apiKey, loc.keySource = c.cfg.APIKey, "config"
	case c.env("CLIENT2API_TABBIT_API_KEY") != "":
		loc.apiKey, loc.keySource = c.env("CLIENT2API_TABBIT_API_KEY"), "env"
	case c.env("TABBIT_API_KEY") != "":
		loc.apiKey, loc.keySource = c.env("TABBIT_API_KEY"), "env"
	case hasStored && strings.TrimSpace(storedEP.APIKey) != "":
		loc.apiKey, loc.keySource = storedEP.APIKey, epOriginPanel
	case strings.TrimSpace(st.APIKey) != "":
		loc.apiKey, loc.keySource = strings.TrimSpace(st.APIKey), "state"
	default:
		loc.apiKey, loc.keySource = defaultAPIKey, "builtin"
	}

	// The browser host follows the same precedence as the endpoint itself: the
	// config wins, then an endpoint added through the panel.  Without this the
	// panel's "Browser host" field would be stored, shown in the account note,
	// and never actually used to build the login URL.
	switch {
	case c.cfg.WebHost != "":
		loc.webHost, loc.webSource = c.cfg.WebHost, "config"
	case hasStored && strings.TrimSpace(storedEP.WebHost) != "":
		loc.webHost, loc.webSource = strings.TrimSpace(storedEP.WebHost), epOriginPanel
	}

	if loc.command == "" {
		switch {
		case strings.TrimSpace(st.Command) != "":
			loc.command, loc.args = strings.TrimSpace(st.Command), st.Args
			if loc.workdir == "" {
				loc.workdir = strings.TrimSpace(st.Workdir)
			}
		case c.env("CLIENT2API_TABBIT_CMD") != "":
			loc.command = c.env("CLIENT2API_TABBIT_CMD")
		case c.env("TABBIT_SIDECAR_CMD") != "":
			loc.command = c.env("TABBIT_SIDECAR_CMD")
		}
	}
	if loc.command == "" && loc.manage {
		if cmd, args, ok := discoverExecutable(); ok {
			loc.command, loc.args = cmd, args
		}
	}
	return loc
}

func firstCandidate() string {
	if len(defaultCandidates) == 0 {
		return ""
	}
	return strings.TrimRight(defaultCandidates[0], "/")
}

// discoverExecutable looks for a sidecar binary in the known install locations
// and on PATH.  It only stats candidates; it never opens or reads them.
func discoverExecutable() (string, []string, bool) {
	for _, cand := range installCandidates {
		p := os.ExpandEnv(cand)
		if strings.TrimSpace(p) == "" {
			continue
		}
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue
		}
		if strings.EqualFold(filepath.Ext(p), ".js") {
			// A Node entry point: run it with whatever node is on PATH.
			if node, err := exec.LookPath("node"); err == nil {
				return node, []string{p}, true
			}
			continue
		}
		return p, nil, true
	}
	for _, name := range pathNames {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil, true
		}
	}
	return "", nil, false
}

// ---------------------------------------------------------------------------
// Model namespace helpers.  The sidecar already prefixes its ids with
// "tabbit/", and the core strips that prefix before calling us, so a request
// for tabbit/priority arrives as "priority" and must go back out prefixed.
// ---------------------------------------------------------------------------

func addModelPrefix(model, prefix string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		// An empty model must stay empty: buildChatBody turns it into
		// core.ErrUnsupported, and "tabbit/" would be a nonsense upstream id.
		return ""
	}
	if prefix == "" || strings.HasPrefix(model, prefix) {
		return model
	}
	return prefix + model
}

func stripModelPrefix(id, prefix string) string {
	id = strings.TrimSpace(id)
	if prefix == "" {
		return id
	}
	return strings.TrimPrefix(id, prefix)
}

// readJSONFile is core.ReadJSON with the error deliberately dropped by callers
// that treat an absent file as "not configured".
func readJSONFile(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
