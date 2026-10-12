// Package doubaoime implements the Doubao Input Method (豆包输入法) speech
// transcription platform. It speaks the private Android ASR protocol and
// exposes only the OpenAI-compatible /v1/audio/transcriptions capability.
package doubaoime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"client2api/internal/core"
)

const credentialsFile = "doubao_credentials.json"

type opusEncoderFunc func(context.Context, []int16) ([][]byte, error)

// Client owns the generated device identity, cached token, Opus encoder and
// WebSocket protocol.
type Client struct {
	deps core.Deps
	cfg  config

	credentialsPath string
	credMu          sync.Mutex
	credentials     cachedCredentials

	// encode is an injection seam for tests. Production leaves it nil and uses
	// the configured or discovered ffmpeg executable.
	encode     opusEncoderFunc
	ffmpegPath string
}

// New builds the module. A missing ffmpeg or missing credentials is a runtime
// condition reported by Status, not a construction failure.
func New(deps core.Deps) (core.Client, error) {
	cfg, err := parseConfig(deps.Config)
	if err != nil {
		deps.Log("doubaoime: invalid config, using defaults: %v", err)
		cfg = config{}
	}
	applyEnv(&cfg)

	if deps.DataDir != "" {
		if err := core.EnsureDir(deps.DataDir); err != nil {
			return nil, fmt.Errorf("doubaoime: creating data dir: %w", err)
		}
	}

	c := &Client{
		deps:       deps,
		cfg:        cfg,
		ffmpegPath: findFFmpeg(cfg.ffmpegPath()),
	}
	if deps.DataDir != "" {
		c.credentialsPath = filepath.Join(deps.DataDir, credentialsFile)
		var creds cachedCredentials
		if err := core.ReadJSON(c.credentialsPath, &creds); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				deps.Log("doubaoime: loading credentials: %v", err)
			}
		} else {
			c.credentials = creds
		}
	}
	return c, nil
}

func init() { core.Register("doubaoime", New) }

func (c *Client) Name() string { return "doubaoime" }

// Models is empty on purpose: speech models belong to the transcription
// endpoint and must not enter chat routing or Auto/<model> aggregation.
func (c *Client) Models(context.Context) ([]core.Model, error) { return nil, nil }

func (c *Client) Chat(context.Context, *core.ChatRequest) (core.Stream, error) {
	return nil, core.ErrUnsupported
}

func (c *Client) TranscriptionModels(context.Context) []core.Model {
	ids := c.cfg.modelIDs()
	out := make([]core.Model, 0, len(ids))
	for _, id := range ids {
		out = append(out, core.Model{ID: id, OwnedBy: "doubaoime"})
	}
	return out
}

func (c *Client) Status(context.Context) core.Status {
	st := core.Status{
		Name:      c.Name(),
		Label:     "Doubao IME transcription",
		Models:    c.cfg.modelIDs(),
		UpdatedAt: time.Now(),
	}
	creds := c.credentialsSnapshot()
	switch {
	case !c.canEncode():
		st.Ready = false
		st.Detail = "Doubao IME transcription needs ffmpeg for Opus encoding; install ffmpeg or set ffmpeg_path"
	case creds.valid():
		st.Ready = true
		st.Detail = "Doubao IME device credentials are ready; POST /v1/audio/transcriptions"
	default:
		st.Ready = true
		st.Detail = "Doubao IME is ready; the first transcription request will register a device automatically"
	}
	return st
}

func (c *Client) Health() core.Health {
	ready := c.canEncode()
	h := core.Health{Servable: ready, Total: 1}
	if ready {
		h.Ready = 1
	} else {
		h.Disabled = 1
		h.Note = "ffmpeg is not available for Opus encoding"
	}
	return h
}

func (c *Client) canEncode() bool {
	return c.encode != nil || c.ffmpegPath != ""
}

func (c *Client) credentialsSnapshot() cachedCredentials {
	c.credMu.Lock()
	defer c.credMu.Unlock()
	return c.credentials
}

var (
	_ core.Client                = (*Client)(nil)
	_ core.TranscriptionProvider = (*Client)(nil)
	_ core.HealthProvider        = (*Client)(nil)
)
