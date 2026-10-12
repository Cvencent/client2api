package doubaoime

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultRegisterURL  = "https://log.snssdk.com/service/2/device_register/"
	defaultSettingsURL  = "https://is.snssdk.com/service/settings/v3/"
	defaultWebSocketURL = "wss://frontier-audio-ime-ws.doubao.com/ocean/api/v1/ws"
	defaultUserAgent    = "com.bytedance.android.doubaoime/100102018 (Linux; U; Android 16; en_US; Pixel 7 Pro; Build/BP2A.250605.031.A2; Cronet/TTNetVersion:94cf429a 2025-11-17 QuicVersion:1f89f732 2025-05-08)"
)

type config struct {
	RegisterURL  string   `json:"register_url"`
	SettingsURL  string   `json:"settings_url"`
	WebSocketURL string   `json:"websocket_url"`
	UserAgent    string   `json:"user_agent"`
	FFmpegPath   string   `json:"ffmpeg_path"`
	Models       []string `json:"models"`

	RequestTimeout durationField `json:"request_timeout"`
	StartTimeout   durationField `json:"start_timeout"`
	ResultTimeout  durationField `json:"result_timeout"`
	TotalTimeout   durationField `json:"total_timeout"`
}

func parseConfig(raw json.RawMessage) (config, error) {
	var cfg config
	if len(raw) == 0 || string(raw) == "null" {
		return cfg, nil
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func applyEnv(cfg *config) {
	envString(&cfg.RegisterURL, "CLIENT2API_DOUBAOIME_REGISTER_URL")
	envString(&cfg.SettingsURL, "CLIENT2API_DOUBAOIME_SETTINGS_URL")
	envString(&cfg.WebSocketURL, "CLIENT2API_DOUBAOIME_WEBSOCKET_URL")
	envString(&cfg.UserAgent, "CLIENT2API_DOUBAOIME_USER_AGENT")
	envString(&cfg.FFmpegPath, "CLIENT2API_DOUBAOIME_FFMPEG_PATH")
}

func envString(dst *string, name string) {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		*dst = v
	}
}

func (c config) registerURL() string {
	return firstNonEmpty(strings.TrimSpace(c.RegisterURL), defaultRegisterURL)
}

func (c config) settingsURL() string {
	return firstNonEmpty(strings.TrimSpace(c.SettingsURL), defaultSettingsURL)
}

func (c config) websocketURL() string {
	return firstNonEmpty(strings.TrimSpace(c.WebSocketURL), defaultWebSocketURL)
}

func (c config) userAgent() string {
	return firstNonEmpty(strings.TrimSpace(c.UserAgent), defaultUserAgent)
}

func (c config) ffmpegPath() string {
	return strings.TrimSpace(c.FFmpegPath)
}

func (c config) requestTimeout() time.Duration {
	return durationOr(string(c.RequestTimeout), 8*time.Second)
}
func (c config) startTimeout() time.Duration {
	return durationOr(string(c.StartTimeout), 8*time.Second)
}
func (c config) resultTimeout() time.Duration {
	return durationOr(string(c.ResultTimeout), 30*time.Second)
}
func (c config) totalTimeout() time.Duration {
	return durationOr(string(c.TotalTimeout), 55*time.Second)
}

func (c config) modelIDs() []string {
	if len(c.Models) == 0 {
		return []string{"whisper-1"}
	}
	out := make([]string, 0, len(c.Models))
	seen := map[string]bool{}
	for _, model := range c.Models {
		model = strings.TrimSpace(model)
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		out = append(out, model)
	}
	if len(out) == 0 {
		return []string{"whisper-1"}
	}
	return out
}

type durationField string

func (d *durationField) UnmarshalJSON(raw []byte) error {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		*d = durationField(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		*d = durationField(n.String())
		return nil
	}
	return &durationError{raw: string(raw)}
}

type durationError struct{ raw string }

func (e *durationError) Error() string {
	return "doubaoime: " + e.raw + " is neither a duration string nor a number of seconds"
}

func durationOr(raw string, def time.Duration) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	if seconds, err := strconv.ParseFloat(raw, 64); err == nil {
		if d := time.Duration(seconds * float64(time.Second)); d > 0 {
			return d
		}
	}
	return def
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
