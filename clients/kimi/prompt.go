package kimi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Prompt flattening
//
// The CLI takes ONE prompt string (-p), not a message array, so the whole
// core.ChatRequest has to be flattened.  The shape follows the MIT reference
// (see README.md "Provenance") line for line, because the reference is the only
// evidence we have of what the CLI accepts:
//
//	[System] ...
//	[User] ...
//	[Assistant] ...
//	Tool calls: [...]
//	[Tool result id=...] ...
//
// Images become "@/abs/path" references to files written under MediaDir.
// ---------------------------------------------------------------------------

const (
	// maxImageBytes caps a single decoded/fetched image.  A chat gateway is
	// reachable from anything on loopback, so an unbounded data: URL is a
	// trivial way to exhaust memory.
	maxImageBytes = 20 << 20
	// imagePlaceholder replaces an image we could not materialise, exactly as
	// the reference does.
	imagePlaceholder = "[image]"
)

// mimeToExt mirrors the reference's MIME_TO_EXT table.
var mimeToExt = map[string]string{
	"image/png":     ".png",
	"image/jpeg":    ".jpg",
	"image/jpg":     ".jpg",
	"image/gif":     ".gif",
	"image/webp":    ".webp",
	"image/heic":    ".heic",
	"image/heif":    ".heif",
	"image/svg+xml": ".svg",
	"image/bmp":     ".bmp",
}

// mediaSet tracks the files one request created so they can be removed when the
// stream is closed.  Cleanup is idempotent.
type mediaSet struct {
	dir   string
	mu    sync.Mutex
	paths []string
	once  sync.Once
}

func newMediaSet(dir string) *mediaSet { return &mediaSet{dir: dir} }

func (m *mediaSet) track(path string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.paths = append(m.paths, path)
	m.mu.Unlock()
}

func (m *mediaSet) cleanup() {
	if m == nil {
		return
	}
	m.once.Do(func() {
		m.mu.Lock()
		paths := m.paths
		m.paths = nil
		m.mu.Unlock()
		for _, p := range paths {
			if strings.TrimSpace(p) == "" {
				continue
			}
			// Best effort: a file we cannot delete is not worth failing a
			// request over, and MediaDir lives inside our own DataDir.
			_ = os.Remove(p)
		}
	})
}

// buildPrompt flattens req into the single string handed to the CLI and returns
// the media set the caller must clean up.
func (c *Client) buildPrompt(ctx context.Context, req *core.ChatRequest) (string, *mediaSet, error) {
	if req == nil {
		return "", nil, fmt.Errorf("kimi: nil request: %w", core.ErrUnsupported)
	}
	ms := newMediaSet(c.cfg.MediaDir)

	var lines []string
	if tools := toolsForPrompt(req); len(tools) > 0 {
		lines = append(lines, toolInstructions(tools)...)
	}
	for _, m := range req.Messages {
		lines = append(lines, c.formatMessage(ctx, m, ms))
	}
	return strings.Join(lines, "\n"), ms, nil
}

// toolsForPrompt filters the tool list down to what can be injected into the
// prompt.  tool_choice:"none" is honoured by simply not injecting anything: the
// CLI has no native tool support to disable.
func toolsForPrompt(req *core.ChatRequest) []core.Tool {
	if req == nil || len(req.Tools) == 0 {
		return nil
	}
	if toolChoiceNone(req.ToolChoice) {
		return nil
	}
	out := make([]core.Tool, 0, len(req.Tools))
	for _, t := range req.Tools {
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		out = append(out, t)
	}
	return out
}

func toolChoiceNone(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == `"none"` || s == "none"
}

// toolInstructions reproduces the reference's injection block verbatim.  The
// model is told to answer with a single JSON object when it wants a tool; the
// text below is the contract this module parses back (see ndjson.go).
func toolInstructions(tools []core.Tool) []string {
	lines := []string{
		"You are acting as an OpenAI-compatible chat API. You MUST ONLY use the tools listed below. Do NOT use any built-in or external tools that are not in this list.",
		"When you need to call a tool, you MUST respond with a single JSON object in this exact format (no markdown, no extra text):",
		`{"tool_calls":[{"id":"call_<randomid>","type":"function","function":{"name":"<tool_name>","arguments":"<json_string_arguments>"}}]}`,
		`The "name" field MUST be one of the following tool names:`,
	}
	for _, t := range tools {
		lines = append(lines, "- "+t.Name)
	}
	lines = append(lines, "Available tools with schemas:")
	for _, t := range tools {
		desc := strings.TrimSpace(t.Description)
		if desc == "" {
			desc = "No description"
		}
		params := strings.TrimSpace(string(t.Parameters))
		if params == "" {
			params = "{}"
		}
		lines = append(lines,
			"- "+t.Name+": "+desc,
			"  Parameters: "+params,
		)
	}
	lines = append(lines,
		"If no tool is needed, or if the user asks for a tool not in the list, reply normally in plain text. Do not mention the tools unless you use one.",
		"",
	)
	return lines
}

// formatMessage renders one message as a prompt line (which may itself contain
// newlines, e.g. an assistant turn that carried tool calls).
func (c *Client) formatMessage(ctx context.Context, m core.Message, ms *mediaSet) string {
	content := c.contentForPrompt(ctx, m, ms)
	switch strings.ToLower(strings.TrimSpace(m.Role)) {
	case "system", "developer":
		return "[System] " + content
	case "user":
		return "[User] " + content
	case "assistant":
		out := "[Assistant] " + content
		if len(m.ToolCalls) > 0 {
			if b, err := json.Marshal(openAIToolCalls(m.ToolCalls)); err == nil {
				out += "\nTool calls: " + string(b)
			}
		}
		return out
	case "tool", "function":
		id := strings.TrimSpace(m.ToolCallID)
		if id == "" {
			id = strings.TrimSpace(m.Name)
		}
		if id == "" {
			id = "unknown"
		}
		return fmt.Sprintf("[Tool result id=%s] %s", id, content)
	default:
		return "[" + roleLabel(m.Role) + "] " + content
	}
}

// contentForPrompt renders a message's content, materialising any images.
func (c *Client) contentForPrompt(ctx context.Context, m core.Message, ms *mediaSet) string {
	if len(m.Parts) == 0 {
		return m.Content
	}
	rendered := make([]string, 0, len(m.Parts))
	for _, p := range m.Parts {
		switch strings.ToLower(strings.TrimSpace(p.Type)) {
		case "image_url", "image":
			rendered = append(rendered, c.materializeImage(ctx, ms, p.ImageURL))
		case "text", "":
			if p.Text != "" {
				rendered = append(rendered, p.Text)
			}
		default:
			if p.Text != "" {
				rendered = append(rendered, p.Text)
			}
		}
	}
	if len(rendered) == 0 {
		return m.Content
	}
	return strings.Join(rendered, " ")
}

// ---------------------------------------------------------------------------
// Images
// ---------------------------------------------------------------------------

// materializeImage writes an image to MediaDir and returns the "@/path"
// reference the CLI understands, or imagePlaceholder when the image cannot be
// represented.  It never returns an error: a broken image degrades to a
// placeholder, exactly as the reference does.
func (c *Client) materializeImage(ctx context.Context, ms *mediaSet, rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return imagePlaceholder
	}

	if data, mime, ok := decodeDataURL(rawURL); ok {
		if p, err := c.writeMedia(ms, data, extForMIME(mime)); err == nil {
			return "@" + p
		}
		return imagePlaceholder
	}

	if strings.HasPrefix(rawURL, "http://") || strings.HasPrefix(rawURL, "https://") {
		// Fetching a caller-supplied URL is server-side request forgery unless
		// the operator explicitly asks for it (config allow_image_download).
		if !c.cfg.AllowImageDownload {
			return imagePlaceholder
		}
		data, contentType, err := c.fetchImage(ctx, rawURL)
		if err != nil {
			return imagePlaceholder
		}
		if p, err := c.writeMedia(ms, data, extForMIME(contentType)); err == nil {
			return "@" + p
		}
		return imagePlaceholder
	}

	// Anything else (file://, relative paths, unknown schemes) is refused:
	// resolving it would let a caller read arbitrary files on this host.
	return imagePlaceholder
}

// decodeDataURL parses a base64 data: URL.  Only base64 payloads are accepted;
// a non-base64 or oversized payload is rejected rather than guessed at.
func decodeDataURL(s string) ([]byte, string, bool) {
	if !strings.HasPrefix(s, "data:") {
		return nil, "", false
	}
	comma := strings.IndexByte(s, ',')
	if comma < 0 {
		return nil, "", false
	}
	meta := s[len("data:"):comma]
	if !strings.Contains(strings.ToLower(meta), "base64") {
		return nil, "", false
	}
	mime := strings.TrimSuffix(meta, ";base64")
	mime = strings.TrimSuffix(mime, ";")
	mime = strings.TrimSpace(strings.ToLower(mime))

	payload := strings.TrimSpace(s[comma+1:])
	// base64.StdEncoding is strict about padding; fall back to RawStdEncoding
	// for the common unpadded variant.
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(payload)
		if err != nil {
			return nil, "", false
		}
	}
	if len(data) == 0 || len(data) > maxImageBytes {
		return nil, "", false
	}
	return data, mime, true
}

func (c *Client) fetchImage(ctx context.Context, rawURL string) ([]byte, string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, "", fmt.Errorf("image fetch: unexpected status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) == 0 || len(data) > maxImageBytes {
		return nil, "", fmt.Errorf("image fetch: payload is %d bytes", len(data))
	}
	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = strings.TrimSpace(contentType[:i])
	}
	return data, contentType, nil
}

// defaultPromptClient is the last-resort client used when the module was built
// with neither a vendor profile nor an injected shared client.  It is a package
// singleton on purpose: http.Client is only a config wrapper, and building one
// per call gives it a brand-new Transport every time, so every prompt fetch
// would open a fresh connection and none would be reused.  The tabbit module
// keeps its own fallback the same way.
var defaultPromptClient = &http.Client{Timeout: 30 * time.Second}

// httpClient returns the vendor client, the injected shared client, or a
// bounded default -- in that order.
//
// vendorClient is only non-nil when the config named a tls_profile, in which
// case it carries the handshake imitation the operator asked for.  Never a
// client without a timeout in the last resort: a hanging fetch would pin a
// concurrency slot forever.  (The injected client has none, which is why the
// callers that can block pass a context deadline.)
func (c *Client) httpClient() *http.Client {
	if c.vendorClient != nil {
		return c.vendorClient
	}
	if c.deps.HTTPClient != nil {
		return c.deps.HTTPClient
	}
	return defaultPromptClient
}

func extForMIME(mime string) string {
	if ext, ok := mimeToExt[strings.ToLower(strings.TrimSpace(mime))]; ok {
		return ext
	}
	return ".png"
}

// writeMedia writes one image into MediaDir via core.WriteFileAtomic and records
// it for cleanup.  MediaDir is always inside Deps.DataDir.
func (c *Client) writeMedia(ms *mediaSet, data []byte, ext string) (string, error) {
	dir := c.cfg.MediaDir
	if err := core.EnsureDir(dir); err != nil {
		return "", err
	}
	name := "kimi-media-" + randHex(16) + ext
	path := filepath.Join(dir, name)
	if err := core.WriteFileAtomic(path, data); err != nil {
		return "", err
	}
	ms.track(path)
	return path, nil
}

func randHex(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail in practice; fall back to a fixed-width
		// value rather than panicking or returning "".
		return strings.Repeat("0", n)
	}
	return hex.EncodeToString(b)[:n]
}

func roleLabel(role string) string {
	role = strings.TrimSpace(role)
	if role == "" {
		return "Message"
	}
	return strings.ToUpper(role[:1]) + role[1:]
}

// openAIToolCalls renders core tool calls in the OpenAI wire shape, which is
// what the reference echoes back into the prompt for a multi-turn conversation.
type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func openAIToolCalls(calls []core.ToolCall) []oaiToolCall {
	out := make([]oaiToolCall, 0, len(calls))
	for _, tc := range calls {
		var o oaiToolCall
		o.ID = tc.ID
		if o.ID == "" {
			o.ID = "call_" + randHex(12)
		}
		o.Type = tc.Type
		if o.Type == "" {
			o.Type = "function"
		}
		o.Function.Name = tc.Name
		o.Function.Arguments = tc.Arguments
		if strings.TrimSpace(o.Function.Arguments) == "" {
			o.Function.Arguments = "{}"
		}
		out = append(out, o)
	}
	return out
}
