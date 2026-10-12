package doubaoime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"client2api/internal/core"
)

func TestConfigExampleParses(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("config.example.json: %v", err)
	}
	if cfg.ffmpegPath() == "" {
		t.Fatal("example should name ffmpeg or leave auto-discovery documented")
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg, err := parseConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.registerURL(); got != defaultRegisterURL {
		t.Fatalf("register URL = %q", got)
	}
	if got := cfg.settingsURL(); got != defaultSettingsURL {
		t.Fatalf("settings URL = %q", got)
	}
	if got := cfg.websocketURL(); got != defaultWebSocketURL {
		t.Fatalf("websocket URL = %q", got)
	}
	if got := cfg.requestTimeout(); got != 8*time.Second {
		t.Fatalf("request timeout = %v", got)
	}
	if got := cfg.resultTimeout(); got != 30*time.Second {
		t.Fatalf("result timeout = %v", got)
	}
}

func TestClientRegistersAndAdvertisesSpeechModel(t *testing.T) {
	c, err := New(core.Deps{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if c.Name() != "doubaoime" {
		t.Fatalf("name = %q", c.Name())
	}
	models := c.(*Client).TranscriptionModels(t.Context())
	if len(models) != 1 || models[0].ID != "whisper-1" {
		t.Fatalf("models = %+v", models)
	}
}
