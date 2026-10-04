package kimi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// isolateCredentials points every credential location the module probes at a
// throwaway directory, so the tests never see the developer's real login.
func isolateCredentials(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("APPDATA", tmp)
	t.Setenv("LOCALAPPDATA", tmp)
	for _, name := range credentialEnvVars {
		t.Setenv(name, "")
	}
}

// newClient builds a Client from a config map.
func newClient(t *testing.T, cfg map[string]any) (*Client, string) {
	t.Helper()
	dir := t.TempDir()
	var raw json.RawMessage
	if cfg != nil {
		b, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		raw = b
	}
	cl, err := New(core.Deps{
		DataDir: dir,
		Config:  raw,
		Logf:    func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c, ok := cl.(*Client)
	if !ok {
		t.Fatalf("New returned %T, want *Client", cl)
	}
	return c, dir
}

func userReq(model, text string) *core.ChatRequest {
	return &core.ChatRequest{
		Model:    model,
		Messages: []core.Message{{Role: "user", Content: text}},
	}
}

// ---------------------------------------------------------------------------
// Stub CLI scripts
//
// The real CLI is not installed, so the child-process tests drive a stub script
// instead.  On Windows the stub is a .cmd (which os/exec runs directly); on
// every other platform it is a /bin/sh script.
// ---------------------------------------------------------------------------

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func fixtureFile(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "out.ndjson")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// stubEmitting prints the fixture and exits 0.
func stubEmitting(t *testing.T, fixture string) string {
	t.Helper()
	dir := t.TempDir()
	fp := fixtureFile(t, dir, fixture)
	if runtime.GOOS == "windows" {
		return writeScript(t, dir, "kimi.cmd", "@echo off\r\ntype \""+fp+"\"\r\nexit /b 0\r\n")
	}
	return writeScript(t, dir, "kimi", "#!/bin/sh\n/bin/cat \""+fp+"\"\nexit 0\n")
}

// stubSleeping prints the fixture and then blocks, so cancellation and the
// request timeout have something to kill.  The sleep is a child process with
// both pipes redirected to nul, so killing the script closes our pipes at once.
func stubSleeping(t *testing.T, fixture string) string {
	t.Helper()
	dir := t.TempDir()
	fp := fixtureFile(t, dir, fixture)
	if runtime.GOOS == "windows" {
		return writeScript(t, dir, "kimi.cmd",
			"@echo off\r\ntype \""+fp+"\"\r\nping -n 30 127.0.0.1 >nul 2>nul\r\nexit /b 0\r\n")
	}
	return writeScript(t, dir, "kimi", "#!/bin/sh\n/bin/cat \""+fp+"\"\n/bin/sleep 30\n")
}

// stubFailing writes to stderr and exits non-zero.
func stubFailing(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		return writeScript(t, dir, "kimi.cmd", "@echo off\r\n>&2 echo kimi: not logged in\r\nexit /b 3\r\n")
	}
	return writeScript(t, dir, "kimi", "#!/bin/sh\necho 'kimi: not logged in' >&2\nexit 3\n")
}

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

func TestParseConfig(t *testing.T) {
	dataDir := t.TempDir()
	cases := []struct {
		name       string
		raw        string
		wantModel  string
		wantConc   int
		wantTime   int
		wantMode   string
		wantMedia  string
		wantErr    bool
		wantSkills string
	}{
		{
			name:      "empty object uses every default",
			raw:       `{}`,
			wantModel: "kimi",
			wantConc:  2,
			wantTime:  600,
			wantMode:  "default",
			wantMedia: filepath.Join(dataDir, "media"),
		},
		{
			name:       "explicit values survive",
			raw:        `{"default_model":"kimi-k2","max_concurrency":5,"timeout_seconds":30,"permission_mode":"plan","skills_dir":"/tmp/skills","media_dir":"/tmp/m"}`,
			wantModel:  "kimi-k2",
			wantConc:   5,
			wantTime:   30,
			wantMode:   "plan",
			wantMedia:  "/tmp/m",
			wantSkills: "/tmp/skills",
		},
		{
			name:      "explicit empty permission mode means no flag",
			raw:       `{"permission_mode":""}`,
			wantModel: "kimi",
			wantConc:  2,
			wantTime:  600,
			wantMode:  "",
			wantMedia: filepath.Join(dataDir, "media"),
		},
		{
			name:      "non-positive numbers fall back to defaults",
			raw:       `{"max_concurrency":-1,"timeout_seconds":0}`,
			wantModel: "kimi",
			wantConc:  2,
			wantTime:  600,
			wantMode:  "default",
			wantMedia: filepath.Join(dataDir, "media"),
		},
		{name: "malformed json is an error", raw: `{`, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parseConfig(json.RawMessage(tc.raw), dataDir)
			if tc.wantErr {
				if err == nil {
					t.Fatal("want an error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseConfig: %v", err)
			}
			if cfg.DefaultModel != tc.wantModel {
				t.Errorf("DefaultModel = %q, want %q", cfg.DefaultModel, tc.wantModel)
			}
			if cfg.MaxConcurrency != tc.wantConc {
				t.Errorf("MaxConcurrency = %d, want %d", cfg.MaxConcurrency, tc.wantConc)
			}
			if cfg.TimeoutSeconds != tc.wantTime {
				t.Errorf("TimeoutSeconds = %d, want %d", cfg.TimeoutSeconds, tc.wantTime)
			}
			if got := cfg.permissionMode(); got != tc.wantMode {
				t.Errorf("permissionMode() = %q, want %q", got, tc.wantMode)
			}
			if cfg.MediaDir != tc.wantMedia {
				t.Errorf("MediaDir = %q, want %q", cfg.MediaDir, tc.wantMedia)
			}
			if cfg.SkillsDir != tc.wantSkills {
				t.Errorf("SkillsDir = %q, want %q", cfg.SkillsDir, tc.wantSkills)
			}
		})
	}
}

func TestNilConfigIsUsable(t *testing.T) {
	cfg, err := parseConfig(nil, t.TempDir())
	if err != nil {
		t.Fatalf("parseConfig(nil): %v", err)
	}
	if cfg.DefaultModel != "kimi" || cfg.MaxConcurrency != 2 || cfg.permissionMode() != "default" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestNewRejectsMalformedConfig(t *testing.T) {
	if _, err := New(core.Deps{DataDir: t.TempDir(), Config: json.RawMessage(`{"binary":`)}); err == nil {
		t.Fatal("want an error for a malformed config")
	}
}

// ---------------------------------------------------------------------------
// Command line
// ---------------------------------------------------------------------------

func TestCLIArgs(t *testing.T) {
	cases := []struct {
		name string
		cfg  map[string]any
		want []string
	}{
		{
			name: "default model omits -m and passes no permission flag",
			cfg:  nil,
			want: []string{"-p", "PROMPT", "--output-format", "stream-json"},
		},
		{
			name: "a named model is forwarded",
			cfg:  nil,
			want: []string{"-p", "PROMPT", "--output-format", "stream-json", "-m", "kimi-k2"},
		},
		{
			name: "no skills-dir unless configured",
			cfg:  nil,
			want: []string{"-p", "PROMPT", "--output-format", "stream-json"},
		},
	}

	// The "default model" case above is the only one that may omit -m; the
	// caller passes model="" for it.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateCredentials(t)
			c, _ := newClient(t, tc.cfg)
			model := ""
			if tc.name == "a named model is forwarded" {
				model = "kimi-k2"
			}
			got := c.cliArgs("PROMPT", model)
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Fatalf("cliArgs = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("explicit empty permission mode passes no flag", func(t *testing.T) {
		isolateCredentials(t)
		c, _ := newClient(t, map[string]any{"permission_mode": ""})
		got := c.cliArgs("PROMPT", "kimi")
		want := []string{"-p", "PROMPT", "--output-format", "stream-json"}
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("cliArgs = %q, want %q", got, want)
		}
	})

	t.Run("skills dir and extra args are opt-in", func(t *testing.T) {
		isolateCredentials(t)
		c, _ := newClient(t, map[string]any{
			"skills_dir": "empty-skills",
			"extra_args": []string{"--verbose"},
		})
		got := c.cliArgs("PROMPT", "kimi")
		want := []string{"-p", "PROMPT", "--output-format", "stream-json",
			"--skills-dir", "empty-skills", "--verbose"}
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("cliArgs = %q, want %q", got, want)
		}
	})

	t.Run("a custom permission flag is honoured", func(t *testing.T) {
		isolateCredentials(t)
		c, _ := newClient(t, map[string]any{"permission_flag": "--approval-policy", "permission_mode": "plan"})
		got := c.cliArgs("PROMPT", "kimi")
		want := []string{"-p", "PROMPT", "--output-format", "stream-json", "--approval-policy", "plan"}
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("cliArgs = %q, want %q", got, want)
		}
	})
}

// ---------------------------------------------------------------------------
// Prompt flattening
// ---------------------------------------------------------------------------

const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func TestBuildPrompt(t *testing.T) {
	cases := []struct {
		name       string
		req        *core.ChatRequest
		want       string
		wantSubstr []string
	}{
		{
			name: "single user turn",
			req:  userReq("kimi", "hello"),
			want: "[User] hello",
		},
		{
			name: "system then multi-turn",
			req: &core.ChatRequest{
				Messages: []core.Message{
					{Role: "system", Content: "be terse"},
					{Role: "user", Content: "hi"},
					{Role: "assistant", Content: "hello"},
					{Role: "user", Content: "again"},
				},
			},
			want: "[System] be terse\n[User] hi\n[Assistant] hello\n[User] again",
		},
		{
			name: "assistant tool calls are echoed back",
			req: &core.ChatRequest{
				Messages: []core.Message{
					{Role: "assistant", ToolCalls: []core.ToolCall{{ID: "call_1", Name: "f", Arguments: `{"a":1}`}}},
					{Role: "tool", ToolCallID: "call_1", Content: "42"},
				},
			},
			wantSubstr: []string{
				`[Assistant] ` + "\nTool calls: ",
				`{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}`,
				"[Tool result id=call_1] 42",
			},
		},
		{
			name: "a tool result without an id still renders",
			req: &core.ChatRequest{
				Messages: []core.Message{{Role: "tool", Content: "x"}},
			},
			want: "[Tool result id=unknown] x",
		},
		{
			name: "an unknown role is labelled rather than dropped",
			req: &core.ChatRequest{
				Messages: []core.Message{{Role: "observer", Content: "note"}},
			},
			want: "[Observer] note",
		},
		{
			name: "text parts are joined with a space",
			req: &core.ChatRequest{
				Messages: []core.Message{{Role: "user", Parts: []core.ContentPart{
					{Type: "text", Text: "one"},
					{Type: "text", Text: "two"},
				}}},
			},
			want: "[User] one two",
		},
		{
			name: "a data-url image becomes an @ reference",
			req: &core.ChatRequest{
				Messages: []core.Message{{Role: "user", Parts: []core.ContentPart{
					{Type: "text", Text: "look"},
					{Type: "image_url", ImageURL: "data:image/png;base64," + onePixelPNG},
				}}},
			},
			wantSubstr: []string{"[User] look @", "kimi-media-", ".png"},
		},
		{
			name: "a remote image is refused unless downloads are allowed",
			req: &core.ChatRequest{
				Messages: []core.Message{{Role: "user", Parts: []core.ContentPart{
					{Type: "image_url", ImageURL: "https://example.invalid/a.png"},
				}}},
			},
			want: "[User] [image]",
		},
		{
			name: "a file url is refused",
			req: &core.ChatRequest{
				Messages: []core.Message{{Role: "user", Parts: []core.ContentPart{
					{Type: "image_url", ImageURL: "file:///etc/passwd"},
				}}},
			},
			want: "[User] [image]",
		},
		{
			name: "a malformed data url degrades to the placeholder",
			req: &core.ChatRequest{
				Messages: []core.Message{{Role: "user", Parts: []core.ContentPart{
					{Type: "image_url", ImageURL: "data:image/png;base64,!!!!"},
				}}},
			},
			want: "[User] [image]",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateCredentials(t)
			c, dataDir := newClient(t, nil)
			got, media, err := c.buildPrompt(context.Background(), tc.req)
			if err != nil {
				t.Fatalf("buildPrompt: %v", err)
			}
			defer media.cleanup()

			if tc.want != "" && got != tc.want {
				t.Fatalf("prompt = %q, want %q", got, tc.want)
			}
			for _, sub := range tc.wantSubstr {
				if !strings.Contains(got, sub) {
					t.Errorf("prompt %q does not contain %q", got, sub)
				}
			}
			// Media files must live inside our own DataDir.
			for _, p := range media.paths {
				if !strings.HasPrefix(p, dataDir) {
					t.Errorf("media file %q escaped DataDir %q", p, dataDir)
				}
			}
		})
	}
}

func TestBuildPromptTools(t *testing.T) {
	isolateCredentials(t)
	c, _ := newClient(t, nil)

	req := &core.ChatRequest{
		Messages: []core.Message{{Role: "user", Content: "weather?"}},
		Tools: []core.Tool{{
			Type:        "function",
			Name:        "get_weather",
			Description: "Look up the weather",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
	}
	got, media, err := c.buildPrompt(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer media.cleanup()

	for _, want := range []string{
		"You are acting as an OpenAI-compatible chat API.",
		`{"tool_calls":[{"id":"call_<randomid>"`,
		`The "name" field MUST be one of the following tool names:`,
		"- get_weather",
		"Available tools with schemas:",
		"- get_weather: Look up the weather",
		`  Parameters: {"type":"object","properties":{"city":{"type":"string"}}}`,
		"If no tool is needed",
		"[User] weather?",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt is missing %q\n---\n%s", want, got)
		}
	}
	// The instruction block ends with a blank line, exactly like the reference.
	if !strings.Contains(got, "Do not mention the tools unless you use one.\n\n[User]") {
		t.Errorf("instruction block is not separated by a blank line:\n%s", got)
	}
}

func TestBuildPromptToolChoiceNone(t *testing.T) {
	isolateCredentials(t)
	c, _ := newClient(t, nil)

	req := &core.ChatRequest{
		Messages:   []core.Message{{Role: "user", Content: "hi"}},
		Tools:      []core.Tool{{Type: "function", Name: "f"}},
		ToolChoice: json.RawMessage(`"none"`),
	}
	got, media, err := c.buildPrompt(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer media.cleanup()
	if got != "[User] hi" {
		t.Fatalf("prompt = %q, want the bare user turn", got)
	}
}

func TestBuildPromptToolWithoutDescription(t *testing.T) {
	isolateCredentials(t)
	c, _ := newClient(t, nil)
	req := &core.ChatRequest{
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		Tools:    []core.Tool{{Type: "function", Name: "bare"}},
	}
	got, media, err := c.buildPrompt(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer media.cleanup()
	if !strings.Contains(got, "- bare: No description") {
		t.Errorf("missing description placeholder:\n%s", got)
	}
	if !strings.Contains(got, "  Parameters: {}") {
		t.Errorf("missing empty parameter schema:\n%s", got)
	}
}

func TestBuildPromptWritesMediaIntoDataDir(t *testing.T) {
	isolateCredentials(t)
	c, dataDir := newClient(t, nil)
	req := &core.ChatRequest{
		Messages: []core.Message{{Role: "user", Parts: []core.ContentPart{
			{Type: "image_url", ImageURL: "data:image/png;base64," + onePixelPNG},
		}}},
	}
	got, media, err := c.buildPrompt(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(media.paths) != 1 {
		t.Fatalf("tracked %d media files, want 1", len(media.paths))
	}
	path := media.paths[0]
	if !strings.HasPrefix(path, filepath.Join(dataDir, "media")) {
		t.Fatalf("media path %q is outside %q", path, dataDir)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("media file was not written: %v", err)
	}
	if !strings.Contains(got, "@"+path) {
		t.Fatalf("prompt %q does not reference %q", got, path)
	}

	media.cleanup()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("media file survived cleanup: %v", err)
	}
	media.cleanup() // idempotent
}

func TestBuildPromptNilRequest(t *testing.T) {
	isolateCredentials(t)
	c, _ := newClient(t, nil)
	if _, _, err := c.buildPrompt(context.Background(), nil); err == nil {
		t.Fatal("want an error for a nil request")
	}
}

func TestDecodeDataURL(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantOK  bool
		wantExt string
	}{
		{name: "png", in: "data:image/png;base64," + onePixelPNG, wantOK: true, wantExt: ".png"},
		{name: "jpeg", in: "data:image/jpeg;base64," + onePixelPNG, wantOK: true, wantExt: ".jpg"},
		{name: "no mime", in: "data:;base64," + onePixelPNG, wantOK: true, wantExt: ".png"},
		{name: "not base64", in: "data:image/png,abc"},
		{name: "no comma", in: "data:image/png;base64"},
		{name: "bad payload", in: "data:image/png;base64,!!!!"},
		{name: "empty payload", in: "data:image/png;base64,"},
		{name: "plain url", in: "https://example.com/a.png"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, mime, ok := decodeDataURL(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if len(data) == 0 {
				t.Fatal("decoded no bytes")
			}
			if got := extForMIME(mime); got != tc.wantExt {
				t.Fatalf("ext = %q, want %q", got, tc.wantExt)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// NDJSON decoding
// ---------------------------------------------------------------------------

// newTestStream builds a stream with no child process, so records can be fed in
// directly.
func newTestStream(t *testing.T, c *Client) *stream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &stream{
		ctx:      ctx,
		cancel:   cancel,
		client:   c,
		media:    newMediaSet(t.TempDir()),
		events:   make(chan core.Event, 256),
		finished: make(chan struct{}),
		errTail:  newTailBuffer(1024),
	}
}

func drainEvents(s *stream) []core.Event {
	var out []core.Event
	for {
		select {
		case ev := <-s.events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func TestNdjsonWriterSplitsAcrossWrites(t *testing.T) {
	isolateCredentials(t)
	c, _ := newClient(t, nil)
	s := newTestStream(t, c)
	w := &ndjsonWriter{s: s}

	// One record delivered in three chunks, with the newline in the last one.
	full := `{"role":"assistant","content":"hel` + `lo"}` + "\n"
	for _, chunk := range []string{
		`{"role":"assist`,
		`ant","content":"hel`,
		`lo"}` + "\n",
	} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	s.flushText()

	events := drainEvents(s)
	if len(events) != 1 || events[0].Delta != "hello" {
		t.Fatalf("events = %+v, want one delta \"hello\"", events)
	}
	if full != `{"role":"assistant","content":"hello"}`+"\n" {
		t.Fatal("fixture drift")
	}
}

func TestNdjsonWriterFlushesPartialTrailingLine(t *testing.T) {
	isolateCredentials(t)
	c, _ := newClient(t, nil)
	s := newTestStream(t, c)
	w := &ndjsonWriter{s: s}

	// No trailing newline: the record must survive the final flush.
	if _, err := w.Write([]byte(`{"role":"assistant","content":"tail"}`)); err != nil {
		t.Fatal(err)
	}
	if evs := drainEvents(s); len(evs) != 0 {
		t.Fatalf("emitted %+v before the flush", evs)
	}
	w.flush()
	s.flushText()

	events := drainEvents(s)
	if len(events) != 1 || events[0].Delta != "tail" {
		t.Fatalf("events = %+v, want one delta \"tail\"", events)
	}
}

func TestNdjsonMalformedLinesAreSkipped(t *testing.T) {
	isolateCredentials(t)
	c, _ := newClient(t, nil)
	s := newTestStream(t, c)

	lines := []string{
		"not json at all",
		`{"role":"assistant","content":"ok"}`,
		`{"role":"assistant","content":`,
		"",
		"   ",
		`{"role":"assistant","content":"done"}`,
	}
	for _, line := range lines {
		s.handleLine(line)
	}
	s.flushText()

	var deltas []string
	for _, ev := range drainEvents(s) {
		if ev.Type == core.EventDelta {
			deltas = append(deltas, ev.Delta)
		}
	}
	if strings.Join(deltas, "|") != "ok|done" {
		t.Fatalf("deltas = %v, want [ok done]", deltas)
	}
}

func TestNdjsonRecordShapes(t *testing.T) {
	cases := []struct {
		name        string
		line        string
		wantDeltas  []string
		wantReason  []string
		wantCalls   int
		wantTool    string
		wantErrSub  string
		wantNoEvent bool
	}{
		{
			name:       "reference shape",
			line:       `{"role":"assistant","content":"hi"}`,
			wantDeltas: []string{"hi"},
		},
		{
			name:       "delta object",
			line:       `{"type":"text","delta":{"type":"text_delta","text":"frag"}}`,
			wantDeltas: []string{"frag"},
		},
		{
			name:       "nested message",
			line:       `{"type":"assistant","message":{"role":"assistant","content":"nested"}}`,
			wantDeltas: []string{"nested"},
		},
		{
			name:       "reasoning",
			line:       `{"role":"assistant","reasoning":"thinking"}`,
			wantReason: []string{"thinking"},
		},
		{
			name:      "usage only",
			line:      `{"type":"result","usage":{"prompt_tokens":11,"completion_tokens":7}}`,
			wantCalls: 0,
		},
		{
			name: "user echo is not prose",
			line: `{"role":"user","content":"my own prompt"}`,
		},
		{
			name: "system record is not prose",
			line: `{"type":"system","content":"banner"}`,
		},
		{
			name:       "native tool call",
			line:       `{"role":"assistant","tool_calls":[{"id":"call_9","type":"function","function":{"name":"ping","arguments":"{\"x\":1}"}}]}`,
			wantCalls:  1,
			wantTool:   "ping",
			wantDeltas: nil,
		},
		{
			name:       "error string",
			line:       `{"type":"error","error":"boom"}`,
			wantErrSub: "boom",
		},
		{
			name:       "error object",
			line:       `{"is_error":true,"error":{"message":"rate limited"}}`,
			wantErrSub: "rate limited",
		},
		{
			name:        "empty object",
			line:        `{}`,
			wantNoEvent: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateCredentials(t)
			c, _ := newClient(t, nil)
			s := newTestStream(t, c)

			s.handleLine(tc.line)
			s.flushText()
			events := drainEvents(s)

			if tc.wantNoEvent && len(events) != 0 {
				t.Fatalf("events = %+v, want none", events)
			}

			var deltas, reasons []string
			calls := 0
			var toolName string
			var errText string
			for _, ev := range events {
				switch ev.Type {
				case core.EventDelta:
					if ev.Reasoning != "" {
						reasons = append(reasons, ev.Reasoning)
					}
					if ev.Delta != "" {
						deltas = append(deltas, ev.Delta)
					}
				case core.EventToolCall:
					calls++
					if ev.ToolCall != nil {
						toolName = ev.ToolCall.Name
					}
				case core.EventError:
					if ev.Err != nil {
						errText = ev.Err.Error()
					}
				}
			}

			if strings.Join(deltas, "|") != strings.Join(tc.wantDeltas, "|") {
				t.Errorf("deltas = %v, want %v", deltas, tc.wantDeltas)
			}
			if strings.Join(reasons, "|") != strings.Join(tc.wantReason, "|") {
				t.Errorf("reasoning = %v, want %v", reasons, tc.wantReason)
			}
			if calls != tc.wantCalls {
				t.Errorf("tool calls = %d, want %d", calls, tc.wantCalls)
			}
			if tc.wantTool != "" && toolName != tc.wantTool {
				t.Errorf("tool name = %q, want %q", toolName, tc.wantTool)
			}
			if tc.wantErrSub != "" && !strings.Contains(errText, tc.wantErrSub) {
				t.Errorf("error = %q, want it to contain %q", errText, tc.wantErrSub)
			}
		})
	}
}

func TestToolEnvelopeIsParsedStructurally(t *testing.T) {
	isolateCredentials(t)
	c, _ := newClient(t, nil)

	// The envelope arrives split over several fragments, which is exactly the
	// case a regex-over-the-whole-output approach gets wrong.
	s := newTestStream(t, c)
	for _, frag := range []string{
		`{"tool_calls":[{"id":"call_abc","type":"function","function":`,
		`{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]}`,
	} {
		s.emitText(frag)
	}
	s.flushText()

	var calls []*core.ToolCallDelta
	var deltas []string
	for _, ev := range drainEvents(s) {
		switch ev.Type {
		case core.EventToolCall:
			calls = append(calls, ev.ToolCall)
		case core.EventDelta:
			deltas = append(deltas, ev.Delta)
		}
	}
	if len(deltas) != 0 {
		t.Fatalf("the envelope leaked as content: %v", deltas)
	}
	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(calls))
	}
	got := calls[0]
	if got.ID != "call_abc" || got.Name != "get_weather" {
		t.Errorf("tool call = %+v", got)
	}
	if got.Arguments != `{"city":"Paris"}` {
		t.Errorf("arguments = %q", got.Arguments)
	}
	if s.finishReason() != "tool_calls" {
		t.Errorf("finish reason = %q, want tool_calls", s.finishReason())
	}
}

func TestParseToolEnvelope(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{
			name: "canonical envelope",
			in:   `{"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}`,
			want: true,
		},
		{
			name: "flat name and arguments",
			in:   `{"tool_calls":[{"name":"f","arguments":{"a":1}}]}`,
			want: true,
		},
		{
			name: "surrounded by prose is not an envelope",
			in:   `Sure! {"tool_calls":[{"function":{"name":"f"}}]}`,
		},
		{
			name: "no tool calls",
			in:   `{"tool_calls":[]}`,
		},
		{
			name: "missing a name",
			in:   `{"tool_calls":[{"id":"c1"}]}`,
		},
		{
			name: "plain text",
			in:   `hello`,
		},
		{
			name: "trailing garbage",
			in:   `{"tool_calls":[{"function":{"name":"f"}}]} and more`,
		},
		{
			name: "not json",
			in:   `{oops}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls, ok := parseToolEnvelope(tc.in)
			if ok != tc.want {
				t.Fatalf("ok = %v, want %v", ok, tc.want)
			}
			if ok && len(calls) == 0 {
				t.Fatal("ok with no calls")
			}
		})
	}
}

func TestPlainTextStartingWithBraceIsStillEmitted(t *testing.T) {
	isolateCredentials(t)
	c, _ := newClient(t, nil)
	s := newTestStream(t, c)

	s.emitText(`{not an envelope}`)
	s.flushText()

	events := drainEvents(s)
	if len(events) != 1 || events[0].Type != core.EventDelta || events[0].Delta != `{not an envelope}` {
		t.Fatalf("events = %+v", events)
	}
}

func TestDecodeUsage(t *testing.T) {
	pt, ct, tt, rt, cch := 10, 4, 14, 2, 6
	cases := []struct {
		name string
		in   *usageRecord
		want *core.Usage
	}{
		{name: "nil", in: nil},
		{name: "empty object", in: &usageRecord{}},
		{
			name: "prompt and completion with a derived total",
			in:   &usageRecord{PromptTokens: &pt, CompletionTokens: &ct},
			want: &core.Usage{PromptTokens: 10, CompletionTokens: 4, TotalTokens: 14},
		},
		{
			name: "input/output aliases and an explicit total",
			in:   &usageRecord{InputTokens: &pt, OutputTokens: &ct, TotalTokens: &tt},
			want: &core.Usage{PromptTokens: 10, CompletionTokens: 4, TotalTokens: 14},
		},
		{
			name: "reasoning and cached",
			in:   &usageRecord{PromptTokens: &pt, ReasoningTokens: &rt, CacheReadInputTokens: &cch},
			want: &core.Usage{PromptTokens: 10, CompletionTokens: 0, TotalTokens: 10, ReasoningTokens: 2, CachedTokens: 6},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeUsage(tc.in)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("got %+v, want nil (a fake zero would be dishonest)", got)
				}
				return
			}
			if got == nil {
				t.Fatal("got nil")
			}
			if *got != *tc.want {
				t.Fatalf("got %+v, want %+v", *got, *tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Secret handling
// ---------------------------------------------------------------------------

func TestRedactSecrets(t *testing.T) {
	isolateCredentials(t)
	t.Setenv("KIMI_API_KEY", "supersecretvalue123456")

	cases := []struct {
		name      string
		in        string
		wantHide  string
		wantShown string
	}{
		{name: "env value", in: "auth failed for supersecretvalue123456", wantHide: "supersecretvalue123456"},
		{name: "bearer", in: "HTTP 401: Bearer abcdefghijklmnop", wantHide: "abcdefghijklmnop", wantShown: "Bearer "},
		{name: "sk key", in: "invalid key sk-abcdefghijklmno", wantHide: "sk-abcdefghijklmno"},
		{
			name:     "jwt",
			in:       "token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abcd rejected",
			wantHide: "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abcd",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactSecrets(tc.in)
			if strings.Contains(got, tc.wantHide) {
				t.Fatalf("secret survived redaction: %q", got)
			}
			if tc.wantShown != "" && !strings.Contains(got, tc.wantShown) {
				t.Fatalf("%q lost its context: %q", tc.wantShown, got)
			}
		})
	}
	if redactSecrets("") != "" {
		t.Fatal("empty input should stay empty")
	}
}

// ---------------------------------------------------------------------------
// Catalog and credentials
// ---------------------------------------------------------------------------

func TestModelsFallback(t *testing.T) {
	isolateCredentials(t)
	c, _ := newClient(t, nil)

	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	ids := modelIDs(models)
	if len(ids) == 0 {
		t.Fatal("the fallback catalog is empty")
	}
	if ids[0] != "kimi" {
		t.Errorf("first model = %q, want kimi", ids[0])
	}
	found := false
	for _, id := range ids {
		if id == "kimi-k2" {
			found = true
		}
	}
	if !found {
		t.Errorf("catalog %v is missing kimi-k2", ids)
	}

	// The returned slice must be a copy: a caller must not be able to mutate
	// the catalog through it.
	models[0].ID = "mutated"
	again, _ := c.Models(context.Background())
	if again[0].ID != "kimi" {
		t.Fatal("Models returned the internal slice")
	}
}

func TestModelsFromConfig(t *testing.T) {
	isolateCredentials(t)
	c, _ := newClient(t, map[string]any{"models": []string{"alpha", " ", "beta"}})
	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(modelIDs(models), ","); got != "alpha,beta" {
		t.Fatalf("models = %q, want alpha,beta", got)
	}
}

func TestCredentialFieldDetection(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{name: "access_token", in: `{"access_token":"abc"}`, want: true},
		{name: "camel case", in: `{"accessToken":"abc"}`, want: true},
		{name: "api key", in: `{"api_key":"abc"}`, want: true},
		{name: "empty value", in: `{"access_token":""}`},
		{name: "unrelated", in: `{"hello":"world"}`},
		{name: "not json", in: `nope`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasCredentialField([]byte(tc.in)); got != tc.want {
				t.Fatalf("hasCredentialField = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCredentialExpiry(t *testing.T) {
	want2099 := time.Date(2099, 1, 2, 3, 4, 5, 0, time.UTC)
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "rfc3339", in: `{"expires_at":"2099-01-02T03:04:05Z"}`, want: "2099-01-02T03:04:05Z"},
		{name: "space separated", in: `{"expires_at":"2099-01-02 03:04:05"}`, want: "2099-01-02T03:04:05Z"},
		{name: "unix seconds", in: `{"exp":` + strconv.FormatInt(want2099.Unix(), 10) + `}`, want: "2099-01-02T03:04:05Z"},
		{name: "unix millis", in: `{"exp":` + strconv.FormatInt(want2099.UnixMilli(), 10) + `}`, want: "2099-01-02T03:04:05Z"},
		{name: "absent", in: `{"access_token":"abc"}`},
		{name: "not json", in: `nope`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := credentialExpiry([]byte(tc.in))
			if tc.want == "" {
				if got != "" {
					t.Fatalf("got %q, want \"\"", got)
				}
				return
			}
			gt, err := time.Parse(time.RFC3339, got)
			if err != nil {
				t.Fatalf("not RFC3339: %q", got)
			}
			wt, _ := time.Parse(time.RFC3339, tc.want)
			if !gt.Equal(wt) {
				t.Fatalf("got %s, want %s", gt, wt)
			}
		})
	}
}

func TestStatusReportsAConfiguredCredentialWithoutLeakingIt(t *testing.T) {
	isolateCredentials(t)
	const token = "sk-abcdefghijklmnopqrstuvwxyz"

	credFile := filepath.Join(t.TempDir(), "kimi-code.json")
	if err := os.WriteFile(credFile, []byte(`{"access_token":"`+token+`","expires_at":"2099-01-02T03:04:05Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	bin := stubEmitting(t, `{"role":"assistant","content":"x"}`+"\n")
	c, _ := newClient(t, map[string]any{
		"binary":           bin,
		"credential_files": []string{credFile},
	})

	st := c.Status(context.Background())
	if !st.Ready {
		t.Fatalf("Ready = false, detail = %q", st.Detail)
	}
	if len(st.Accounts) != 1 {
		t.Fatalf("accounts = %+v, want exactly one", st.Accounts)
	}
	acct := st.Accounts[0]
	if acct.State != "ready" {
		t.Errorf("state = %q, want ready", acct.State)
	}
	if acct.ExpiresAt != "2099-01-02T03:04:05Z" {
		t.Errorf("expires_at = %q", acct.ExpiresAt)
	}
	blob, _ := json.Marshal(st)
	if strings.Contains(string(blob), token) {
		t.Fatalf("Status leaked the token: %s", blob)
	}
}

func TestStatusMarksAnExpiredCredential(t *testing.T) {
	isolateCredentials(t)
	credFile := filepath.Join(t.TempDir(), "kimi-code.json")
	if err := os.WriteFile(credFile, []byte(`{"access_token":"abc123456","expires_at":"2000-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := stubEmitting(t, "\n")
	c, _ := newClient(t, map[string]any{"binary": bin, "credential_files": []string{credFile}})

	st := c.Status(context.Background())
	if len(st.Accounts) != 1 || st.Accounts[0].State != "invalid" {
		t.Fatalf("accounts = %+v, want one invalid account", st.Accounts)
	}
}

func TestStatusDegradedWithoutCLI(t *testing.T) {
	isolateCredentials(t)
	t.Setenv("PATH", t.TempDir())

	c, _ := newClient(t, nil)
	st := c.Status(context.Background())

	if st.Ready {
		t.Fatalf("Ready = true without a CLI: %+v", st)
	}
	if st.Name != "kimi" {
		t.Errorf("Name = %q", st.Name)
	}
	if !strings.Contains(st.Detail, "not found") {
		t.Errorf("detail = %q, want it to say the CLI was not found", st.Detail)
	}
	if len(st.Models) == 0 {
		t.Error("Models must still be populated offline")
	}
	if len(st.Accounts) != 1 || st.Accounts[0].State != "unknown" {
		t.Errorf("accounts = %+v, want one unknown account", st.Accounts)
	}
	if st.UpdatedAt.IsZero() {
		t.Error("UpdatedAt is zero")
	}
}

func TestStatusNotLoggedIn(t *testing.T) {
	isolateCredentials(t)
	bin := stubEmitting(t, "\n")
	c, _ := newClient(t, map[string]any{
		"binary":           bin,
		"credential_files": []string{filepath.Join(t.TempDir(), "missing.json")},
	})

	st := c.Status(context.Background())
	if st.Ready {
		t.Fatalf("Ready = true without a login: %+v", st)
	}
	if !strings.Contains(st.Detail, "not logged in") {
		t.Errorf("detail = %q", st.Detail)
	}
}

func TestStatusAssumeLoggedIn(t *testing.T) {
	isolateCredentials(t)
	bin := stubEmitting(t, "\n")
	c, _ := newClient(t, map[string]any{"binary": bin, "assume_logged_in": true})

	st := c.Status(context.Background())
	if !st.Ready {
		t.Fatalf("Ready = false with assume_logged_in: %q", st.Detail)
	}
}

// ---------------------------------------------------------------------------
// Chat: degraded mode
// ---------------------------------------------------------------------------

func TestChatWithoutCLIIsNotConfigured(t *testing.T) {
	isolateCredentials(t)
	t.Setenv("PATH", t.TempDir())
	c, _ := newClient(t, nil)

	_, err := c.Chat(context.Background(), userReq("kimi", "hi"))
	if !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("err = %v, want core.ErrNotConfigured", err)
	}
	if !strings.Contains(err.Error(), "kimi CLI not found") {
		t.Errorf("error does not name the missing prerequisite: %v", err)
	}

	// The failure must be visible on the status line.
	if st := c.Status(context.Background()); !strings.Contains(st.Detail, "last error") {
		t.Errorf("status does not record the failure: %q", st.Detail)
	}
}

func TestChatWithoutLoginIsNotConfigured(t *testing.T) {
	isolateCredentials(t)
	bin := stubEmitting(t, "\n")
	c, _ := newClient(t, map[string]any{
		"binary":           bin,
		"credential_files": []string{filepath.Join(t.TempDir(), "missing.json")},
	})

	_, err := c.Chat(context.Background(), userReq("kimi", "hi"))
	if !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("err = %v, want core.ErrNotConfigured", err)
	}
	if !strings.Contains(err.Error(), "kimi login") {
		t.Errorf("error does not tell the user what to run: %v", err)
	}
}

func TestChatNilRequest(t *testing.T) {
	isolateCredentials(t)
	c, _ := newClient(t, nil)
	if _, err := c.Chat(context.Background(), nil); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("err = %v, want core.ErrUnsupported", err)
	}
}

func TestBinaryConfiguredButMissing(t *testing.T) {
	isolateCredentials(t)
	c, _ := newClient(t, map[string]any{"binary": filepath.Join(t.TempDir(), "nope.exe")})
	if _, err := c.run.binaryPath(); err == nil {
		t.Fatal("want an error for a missing configured binary")
	}
}

func TestBinaryConfiguredAsDirectory(t *testing.T) {
	isolateCredentials(t)
	dir := t.TempDir()
	c, _ := newClient(t, map[string]any{"binary": dir})
	if _, err := c.run.binaryPath(); err == nil {
		t.Fatal("want an error when the configured binary is a directory")
	}
}

// ---------------------------------------------------------------------------
// Chat: a real child process
// ---------------------------------------------------------------------------

// recvAll drains a stream until io.EOF.
func recvAll(t *testing.T, st core.Stream) []core.Event {
	t.Helper()
	var out []core.Event
	for i := 0; i < 1000; i++ {
		ev, err := st.Recv()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		out = append(out, ev)
	}
	t.Fatal("the stream never ended")
	return nil
}

func TestChatStreamsTextAndUsage(t *testing.T) {
	isolateCredentials(t)
	bin := stubEmitting(t, strings.Join([]string{
		`{"role":"assistant","content":"Hello"}`,
		`{"role":"assistant","content":", world"}`,
		`{"type":"result","usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
	}, "\n")+"\n")

	c, _ := newClient(t, map[string]any{
		"binary":            bin,
		"assume_logged_in":  true,
		"timeout_seconds":   30,
		"max_concurrency":   2,
		"allow_image_downl": false,
	})

	st, err := c.Chat(context.Background(), userReq("kimi", "hi"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer st.Close()

	events := recvAll(t, st)

	var text string
	var usage *core.Usage
	done := 0
	var finish string
	for _, ev := range events {
		switch ev.Type {
		case core.EventDelta:
			text += ev.Delta
		case core.EventUsage:
			usage = ev.Usage
		case core.EventDone:
			done++
			finish = ev.Finish
		case core.EventError:
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
	}
	if text != "Hello, world" {
		t.Errorf("text = %q", text)
	}
	if done != 1 || finish != "stop" {
		t.Errorf("done = %d, finish = %q", done, finish)
	}
	if usage == nil || usage.TotalTokens != 5 || usage.PromptTokens != 3 {
		t.Errorf("usage = %+v", usage)
	}

	// io.EOF is sticky and Close is idempotent.
	if _, err := st.Recv(); !errors.Is(err, io.EOF) {
		t.Errorf("second Recv after EOF = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}
}

func TestChatEmitsNoUsageWhenTheCLIDoesNotReportIt(t *testing.T) {
	isolateCredentials(t)
	bin := stubEmitting(t, `{"role":"assistant","content":"hi"}`+"\n")
	c, _ := newClient(t, map[string]any{"binary": bin, "assume_logged_in": true})

	st, err := c.Chat(context.Background(), userReq("kimi", "hi"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for _, ev := range recvAll(t, st) {
		if ev.Type == core.EventUsage {
			t.Fatalf("a usage event was invented: %+v", ev.Usage)
		}
	}
}

func TestChatToolCallFromPromptEnvelope(t *testing.T) {
	isolateCredentials(t)
	bin := stubEmitting(t, `{"role":"assistant","content":"{\"tool_calls\":[{\"id\":\"call_abc\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\\\"Paris\\\"}\"}}]}"}`+"\n")

	c, _ := newClient(t, map[string]any{"binary": bin, "assume_logged_in": true})
	st, err := c.Chat(context.Background(), userReq("kimi", "weather?"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var calls []*core.ToolCallDelta
	var finish string
	var deltas []string
	for _, ev := range recvAll(t, st) {
		switch ev.Type {
		case core.EventToolCall:
			calls = append(calls, ev.ToolCall)
		case core.EventDelta:
			deltas = append(deltas, ev.Delta)
		case core.EventDone:
			finish = ev.Finish
		}
	}
	if len(deltas) != 0 {
		t.Errorf("the envelope leaked as content: %v", deltas)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[0].Name != "get_weather" || calls[0].ID != "call_abc" {
		t.Errorf("call = %+v", calls[0])
	}
	if calls[0].Arguments != `{"city":"Paris"}` {
		t.Errorf("arguments = %q", calls[0].Arguments)
	}
	if finish != "tool_calls" {
		t.Errorf("finish = %q, want tool_calls", finish)
	}
}

func TestChatNonZeroExit(t *testing.T) {
	isolateCredentials(t)
	bin := stubFailing(t)
	c, _ := newClient(t, map[string]any{"binary": bin, "assume_logged_in": true})

	st, err := c.Chat(context.Background(), userReq("kimi", "hi"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var errText string
	done := 0
	for _, ev := range recvAll(t, st) {
		switch ev.Type {
		case core.EventError:
			errText = ev.Err.Error()
		case core.EventDone:
			done++
		}
	}
	if done != 1 {
		t.Errorf("done events = %d, want exactly 1", done)
	}
	if !strings.Contains(errText, "not logged in") {
		t.Errorf("error = %q, want the stderr tail", errText)
	}
}

func TestChatTimeout(t *testing.T) {
	isolateCredentials(t)
	bin := stubSleeping(t, `{"role":"assistant","content":"slow"}`+"\n")
	c, _ := newClient(t, map[string]any{
		"binary":           bin,
		"assume_logged_in": true,
		"timeout_seconds":  1,
	})

	start := time.Now()
	st, err := c.Chat(context.Background(), userReq("kimi", "hi"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var errText string
	for _, ev := range recvAll(t, st) {
		if ev.Type == core.EventError {
			errText = ev.Err.Error()
		}
	}
	if !strings.Contains(errText, "timed out") {
		t.Errorf("error = %q, want a timeout", errText)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("the timeout took %s", elapsed)
	}
}

func TestChatCancellationKillsTheChild(t *testing.T) {
	isolateCredentials(t)
	bin := stubSleeping(t, `{"role":"assistant","content":"partial"}`+"\n")
	c, _ := newClient(t, map[string]any{
		"binary":           bin,
		"assume_logged_in": true,
		"timeout_seconds":  120,
	})

	ctx, cancel := context.WithCancel(context.Background())
	st, err := c.Chat(ctx, userReq("kimi", "hi"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Read the first fragment, then pull the plug.
	ev, err := st.Recv()
	if err != nil {
		t.Fatalf("first Recv: %v", err)
	}
	if ev.Delta != "partial" {
		t.Fatalf("first event = %+v", ev)
	}

	start := time.Now()
	cancel()
	for i := 0; i < 100; i++ {
		if _, err := st.Recv(); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("Recv: %v", err)
		}
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("cancellation took %s", elapsed)
	}
}

func TestChatConcurrencyCap(t *testing.T) {
	isolateCredentials(t)
	bin := stubSleeping(t, `{"role":"assistant","content":"a"}`+"\n")
	c, _ := newClient(t, map[string]any{
		"binary":           bin,
		"assume_logged_in": true,
		"max_concurrency":  1,
		"timeout_seconds":  120,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, err := c.Chat(ctx, userReq("kimi", "one"))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	// The single slot is taken, so a second request must wait and then give up
	// with its own context rather than spawning another process.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer waitCancel()
	_, err = c.Chat(waitCtx, userReq("kimi", "two"))
	if err == nil {
		t.Fatal("the concurrency cap did not apply")
	}
	if !strings.Contains(err.Error(), "max_concurrency") {
		t.Errorf("error = %q, want it to mention max_concurrency", err)
	}

	// Closing the first request frees the slot.
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := c.Chat(ctx, userReq("kimi", "three"))
	if err != nil {
		t.Fatalf("the slot was not released: %v", err)
	}
	third.Close()
}

func TestChatClosesAndReleasesOnEarlyClose(t *testing.T) {
	isolateCredentials(t)
	bin := stubSleeping(t, `{"role":"assistant","content":"a"}`+"\n")
	c, _ := newClient(t, map[string]any{
		"binary":           bin,
		"assume_logged_in": true,
		"max_concurrency":  1,
		"timeout_seconds":  120,
	})

	st, err := c.Chat(context.Background(), userReq("kimi", "hi"))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("Close took %s", elapsed)
	}

	// The slot must be free again.
	if len(c.run.sem) != 0 {
		t.Errorf("the semaphore still holds %d slots", len(c.run.sem))
	}
}

func TestChatRemovesMediaWhenTheStreamEnds(t *testing.T) {
	isolateCredentials(t)
	bin := stubEmitting(t, `{"role":"assistant","content":"ok"}`+"\n")
	c, _ := newClient(t, map[string]any{"binary": bin, "assume_logged_in": true})

	req := &core.ChatRequest{
		Model: "kimi",
		Messages: []core.Message{{Role: "user", Parts: []core.ContentPart{
			{Type: "text", Text: "look"},
			{Type: "image_url", ImageURL: "data:image/png;base64," + onePixelPNG},
		}}},
	}
	st, err := c.Chat(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	recvAll(t, st)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(filepath.Join(c.cfg.dataDir, "media"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return // never created: also fine
		}
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("media files survived the request: %v", entries)
	}
}

func TestChatUsesTheConfiguredModelFlag(t *testing.T) {
	isolateCredentials(t)

	// The stub records its own raw command line, so we can assert what the
	// module actually asked the CLI to do.  (%* cannot be echoed back as
	// content: Windows re-quotes argv and the quotes would break the NDJSON.)
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	var script, scriptName string
	if runtime.GOOS == "windows" {
		scriptName = "kimi.cmd"
		script = "@echo off\r\n" +
			"echo %* > \"" + argsFile + "\"\r\n" +
			"echo {\"role\":\"assistant\",\"content\":\"ok\"}\r\n" +
			"exit /b 0\r\n"
	} else {
		scriptName = "kimi"
		script = "#!/bin/sh\n" +
			"echo \"$*\" > '" + argsFile + "'\n" +
			"echo '{\"role\":\"assistant\",\"content\":\"ok\"}'\n" +
			"exit 0\n"
	}
	bin := writeScript(t, dir, scriptName, script)

	c, _ := newClient(t, map[string]any{"binary": bin, "assume_logged_in": true})
	st, err := c.Chat(context.Background(), userReq("kimi-k2", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var text string
	for _, ev := range recvAll(t, st) {
		if ev.Type == core.EventDelta {
			text += ev.Delta
		}
	}
	if text != "ok" {
		t.Fatalf("text = %q, want ok", text)
	}

	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("the stub recorded no argv: %v", err)
	}
	argv := string(raw)
	for _, want := range []string{
		"--output-format stream-json",
		"-m kimi-k2",
		"[User] hello",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv is missing %q: %s", want, argv)
		}
	}
	if strings.Contains(argv, "--skills-dir") {
		t.Errorf("--skills-dir was passed by default: %s", argv)
	}
	// The real kimi-code CLI rejects every permission flag in prompt mode, so
	// the end-to-end argv must carry none.  See defaultPermissionFlag.
	if strings.Contains(argv, "--permission-mode") || strings.Contains(argv, "--plan") ||
		strings.Contains(argv, "--yolo") || strings.Contains(argv, "--auto") {
		t.Errorf("a permission flag was passed by default: %s", argv)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func TestChildEnvOverrides(t *testing.T) {
	t.Setenv("KIMI_TEST_BASE", "base")
	t.Setenv("KIMI_TEST_OVERRIDE", "old")

	got := childEnv(map[string]string{"KIMI_TEST_OVERRIDE": "new", "KIMI_TEST_EXTRA": "x"})
	seen := map[string]string{}
	for _, kv := range got {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		seen[strings.ToUpper(kv[:i])] = kv[i+1:]
	}
	if seen["KIMI_TEST_BASE"] != "base" {
		t.Errorf("the parent environment was dropped")
	}
	if seen["KIMI_TEST_OVERRIDE"] != "new" {
		t.Errorf("override = %q, want new", seen["KIMI_TEST_OVERRIDE"])
	}
	if seen["KIMI_TEST_EXTRA"] != "x" {
		t.Errorf("extra = %q, want x", seen["KIMI_TEST_EXTRA"])
	}
}

func TestTailBufferKeepsTheEnd(t *testing.T) {
	tb := newTailBuffer(4)
	if _, err := tb.Write([]byte("abcdefg")); err != nil {
		t.Fatal(err)
	}
	if got := tb.String(); got != "defg" {
		t.Fatalf("tail = %q, want defg", got)
	}
}

func TestRandHex(t *testing.T) {
	a, b := randHex(16), randHex(16)
	if len(a) != 16 {
		t.Fatalf("len = %d, want 16", len(a))
	}
	if a == b {
		t.Fatal("randHex returned the same value twice")
	}
	if randHex(0) != "" || randHex(-1) != "" {
		t.Fatal("non-positive n should be empty")
	}
}

func TestMaskedConfigLogNeverPrintsTheBinaryPathSecret(t *testing.T) {
	// New logs the resolved config; make sure a credential-looking binary path
	// is not the thing being logged and that logging itself never panics.
	isolateCredentials(t)
	var logged []string
	dir := t.TempDir()
	cl, err := New(core.Deps{
		DataDir: dir,
		Config:  json.RawMessage(`{"default_model":"kimi"}`),
		Logf:    func(format string, args ...any) { logged = append(logged, format) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if cl == nil {
		t.Fatal("nil client")
	}
	if len(logged) == 0 {
		t.Fatal("New logged nothing")
	}
}
