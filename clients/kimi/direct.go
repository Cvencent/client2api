package kimi

// The direct HTTPS path.
//
// Chat used to mean exactly one thing: spawn `kimi -p <prompt>` and parse the
// NDJSON it prints.  That is why the module could not function without the CLI.
// The vendor also serves the same models over an OpenAI-shaped HTTP API:
//
//	POST https://api.kimi.com/coding/v1/chat/completions
//	Authorization: Bearer <access_token>
//
// (Verified live: the endpoint answers 401 with a standard OpenAI error
// envelope when called without a token, and /models, /me and /usages exist
// alongside it.)
//
// So when the panel has produced a token, this file is used and the CLI is not
// consulted at all.  When it has not, Chat falls back to the CLI exactly as
// before.  The CLI path is untouched; this is an additional route, not a
// replacement.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"client2api/internal/core"
)

const (
	// defaultAPIBaseCN and defaultAPIBaseGlobal are the two regional roots the
	// CLI itself uses (see its KIMI_CODE_BASE_URL handling).
	defaultAPIBaseCN     = "https://api.kimi.com/coding/v1"
	defaultAPIBaseGlobal = "https://api.kimi.ai/coding/v1"

	chatCompletionsPath = "/chat/completions"
)

// apiBase picks the coding API root for a token.  The issuer decides the
// region: a token minted by the mainland issuer is not accepted by the global
// API and vice versa, so guessing would produce a confusing 401.
func (c *Client) apiBase(tok storedToken) string {
	if b := trimHost(c.cfg.APIBase); b != "" {
		return b
	}
	if strings.Contains(tok.OAuthHost, "kimi.ai") {
		return defaultAPIBaseGlobal
	}
	return defaultAPIBaseCN
}

// ---------------------------------------------------------------------------
// Request shaping
// ---------------------------------------------------------------------------

type openAIFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type openAITool struct {
	Type     string         `json:"type"`
	Function openAIFunction `json:"function"`
}

type openAIToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content,omitempty"`
	Name       string           `json:"name,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	Reasoning  string           `json:"reasoning_content,omitempty"`
}

// openAIStreamOptions is the vendor's stream_options.  The CLI always asks for
// the usage block, which is the only place a streamed turn reports token counts.
type openAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// openAIThinking is Kimi's private thinking extension.  It is not part of the
// OpenAI schema; the official CLI enables it on every request, and `keep:"all"`
// is what it sends.  See README.md, "Thinking".
type openAIThinking struct {
	Type string `json:"type"`
	Keep string `json:"keep,omitempty"`
}

// openAIRequest is the vendor's request shape.
//
// Field order is load-bearing: encoding/json emits keys in declaration order,
// and the official CLI is an OpenAI SDK client whose key order is observable.
// The declaration below reproduces the captured order
//
//	model, messages, tools, stream, stream_options, prompt_cache_key, thinking,
//	max_completion_tokens
//
// with the fields the CLI never sends -- temperature, top_p, stop, user,
// tool_choice -- slotted in between.  Because each is omitempty, a request that
// does not set them serialises in exactly the captured order.
//
// max_completion_tokens is also deliberate: the CLI sends that spelling, not
// the older max_tokens.
type openAIRequest struct {
	Model               string               `json:"model"`
	Messages            []openAIMessage      `json:"messages"`
	Tools               []openAITool         `json:"tools,omitempty"`
	Stream              bool                 `json:"stream"`
	StreamOptions       *openAIStreamOptions `json:"stream_options,omitempty"`
	PromptCacheKey      string               `json:"prompt_cache_key,omitempty"`
	Thinking            *openAIThinking      `json:"thinking,omitempty"`
	Temperature         *float64             `json:"temperature,omitempty"`
	TopP                *float64             `json:"top_p,omitempty"`
	MaxCompletionTokens *int                 `json:"max_completion_tokens,omitempty"`
	Stop                []string             `json:"stop,omitempty"`
	User                string               `json:"user,omitempty"`
	ToolChoice          json.RawMessage      `json:"tool_choice,omitempty"`
}

// requestShaping carries the per-client knobs that decide how much of the
// vendor's request shape to reproduce.  The zero value emits none of them, which
// is what the tests pin.
type requestShaping struct {
	// cacheKey becomes prompt_cache_key: a stable routing hint the vendor uses
	// to keep a session's turns on the same cache.  Empty omits it.
	cacheKey string
	// thinking adds the vendor's thinking extension.
	thinking bool
	// maxTokens is the ceiling to send when the caller set none.  nil omits
	// the field, which lets the vendor pick its own default -- safer, but one
	// field further from the CLI, which always sends the model's context
	// window here.
	maxTokens *int
}

// buildOpenAIRequest translates the gateway's request into the vendor's shape.
func buildOpenAIRequest(req *core.ChatRequest, defaultModel string, shape requestShaping) openAIRequest {
	out := openAIRequest{
		Model:          firstNonEmpty(strings.TrimSpace(req.Model), defaultModel),
		Stream:         true,
		StreamOptions:  &openAIStreamOptions{IncludeUsage: true},
		PromptCacheKey: shape.cacheKey,
		Temperature:    req.Temperature,
		TopP:           req.TopP,
		Stop:           req.Stop,
		User:           req.User,
		ToolChoice:     req.ToolChoice,
	}
	if shape.thinking {
		// The CLI enables thinking unconditionally, with keep:"all".
		out.Thinking = &openAIThinking{Type: "enabled", Keep: "all"}
	}
	// The caller's ceiling, under its modern name.  The CLI always sends one
	// (it derives it from the model's context window), so an operator who wants
	// byte-fidelity sets max_completion_tokens in the module config; without
	// either, the vendor picks.
	out.MaxCompletionTokens = req.MaxTokens
	if out.MaxCompletionTokens == nil {
		out.MaxCompletionTokens = shape.maxTokens
	}

	for _, m := range req.Messages {
		msg := openAIMessage{
			Role:       m.Role,
			Name:       m.Name,
			ToolCallID: m.ToolCallID,
			Reasoning:  m.Reasoning,
		}
		// A message with structured parts is sent as parts; otherwise the flat
		// Content string is what the caller meant.
		if len(m.Parts) > 0 {
			parts := make([]map[string]any, 0, len(m.Parts))
			for _, p := range m.Parts {
				switch p.Type {
				case "image_url":
					img := map[string]any{"url": p.ImageURL}
					if p.Detail != "" {
						img["detail"] = p.Detail
					}
					parts = append(parts, map[string]any{"type": "image_url", "image_url": img})
				default:
					parts = append(parts, map[string]any{"type": "text", "text": p.Text})
				}
			}
			msg.Content = parts
		} else if m.Content != "" {
			msg.Content = m.Content
		}
		for _, tc := range m.ToolCalls {
			var call openAIToolCall
			call.ID = tc.ID
			call.Type = firstNonEmpty(tc.Type, "function")
			call.Function.Name = tc.Name
			call.Function.Arguments = tc.Arguments
			msg.ToolCalls = append(msg.ToolCalls, call)
		}
		out.Messages = append(out.Messages, msg)
	}

	for _, t := range req.Tools {
		out.Tools = append(out.Tools, openAITool{
			Type: firstNonEmpty(t.Type, "function"),
			Function: openAIFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// Response parsing
// ---------------------------------------------------------------------------

type openAIChunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content          string           `json:"content"`
			ReasoningContent string           `json:"reasoning_content"`
			ToolCalls        []openAIToolCall `json:"tool_calls"`
		} `json:"delta"`
		Message struct {
			Content          string           `json:"content"`
			ReasoningContent string           `json:"reasoning_content"`
			ToolCalls        []openAIToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// ---------------------------------------------------------------------------
// core.Stream over an SSE body
// ---------------------------------------------------------------------------

// directStream reads the vendor's SSE response.  It reads synchronously from
// Recv rather than running a goroutine: there is no child process to supervise
// and no backpressure to model, so a goroutine would only add a way to leak.
type directStream struct {
	client *Client
	body   io.ReadCloser
	reader *bufio.Reader

	pending []core.Event
	done    bool
	// emitted tracks whether any assistant text was produced, so a stream that
	// ends without output can be reported as an error rather than as silence.
	emitted bool
	// tools counts tool calls, mirroring the CLI path's finish reason.
	tools  int
	closed bool
	once   sync.Once
}

func newDirectStream(c *Client, body io.ReadCloser) *directStream {
	return &directStream{client: c, body: body, reader: bufio.NewReaderSize(body, 64*1024)}
}

// Recv implements core.Stream.  It yields delta/tool_call/usage events and
// finally exactly one done event, after which it returns io.EOF.
func (s *directStream) Recv() (core.Event, error) {
	for {
		if len(s.pending) > 0 {
			ev := s.pending[0]
			s.pending = s.pending[1:]
			return ev, nil
		}
		if s.done {
			s.Close()
			return core.Event{}, io.EOF
		}
		s.pump()
	}
}

// Close implements core.Stream.  It is idempotent.
func (s *directStream) Close() error {
	s.once.Do(func() {
		s.closed = true
		if s.body != nil {
			_ = s.body.Close()
		}
	})
	return nil
}

// pump reads one SSE record and appends whatever events it implies.  It never
// blocks beyond one read, so a stalled upstream is bounded by the request
// context rather than by this loop.
func (s *directStream) pump() {
	for {
		line, err := s.reader.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")

		if trimmed == "" {
			if err != nil {
				s.finalize(err)
				return
			}
			continue
		}
		// Comments keep the connection warm; fields other than data: carry no
		// payload for this API.
		if strings.HasPrefix(trimmed, ":") || !strings.HasPrefix(trimmed, "data:") {
			if err != nil {
				s.finalize(err)
				return
			}
			continue
		}

		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "[DONE]" {
			s.finalize(nil)
			return
		}
		s.handleChunk(payload)

		if err != nil {
			s.finalize(err)
			return
		}
		if len(s.pending) > 0 {
			return
		}
	}
}

func (s *directStream) handleChunk(payload string) {
	var chunk openAIChunk
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		// A malformed frame is dropped rather than fatal: the vendor
		// occasionally emits keep-alive-shaped noise, and killing a live
		// stream over it would be worse than losing one frame.
		s.client.deps.Log("kimi: skipping a malformed SSE frame (%d bytes)", len(payload))
		return
	}

	if chunk.Error != nil && chunk.Error.Message != "" {
		s.pending = append(s.pending, core.Event{
			Type: core.EventError,
			Err:  fmt.Errorf("kimi: %s", core.Redact(chunk.Error.Message)),
		})
		return
	}

	if chunk.Usage != nil {
		s.pending = append(s.pending, core.Event{
			Type: core.EventUsage,
			Usage: &core.Usage{
				PromptTokens:     chunk.Usage.PromptTokens,
				CompletionTokens: chunk.Usage.CompletionTokens,
				TotalTokens:      chunk.Usage.TotalTokens,
			},
		})
	}

	for _, choice := range chunk.Choices {
		delta := choice.Delta
		// A non-streaming body arrives as a whole message in one frame.
		if delta.Content == "" && delta.ReasoningContent == "" && len(delta.ToolCalls) == 0 {
			delta.Content = choice.Message.Content
			delta.ReasoningContent = choice.Message.ReasoningContent
			delta.ToolCalls = choice.Message.ToolCalls
		}

		if delta.Content != "" {
			s.emitted = true
			s.pending = append(s.pending, core.Event{Type: core.EventDelta, Delta: delta.Content})
		}
		if delta.ReasoningContent != "" {
			s.pending = append(s.pending, core.Event{Type: core.EventDelta, Reasoning: delta.ReasoningContent})
		}
		for _, tc := range delta.ToolCalls {
			s.tools++
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			s.pending = append(s.pending, core.Event{
				Type: core.EventToolCall,
				ToolCall: &core.ToolCallDelta{
					Index:     idx,
					ID:        tc.ID,
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				},
			})
		}
	}
}

// finalize emits the terminal events exactly once.  err is the read error that
// ended the body, if any.
func (s *directStream) finalize(err error) {
	if s.done {
		return
	}
	s.done = true

	if err != nil && err != io.EOF {
		s.pending = append(s.pending, core.Event{
			Type: core.EventError,
			Err:  fmt.Errorf("kimi: reading the response stream failed: %w", err),
		})
	} else if !s.emitted && s.tools == 0 {
		// The vendor closed the stream without producing anything.  Saying so
		// is better than reporting a successful empty answer.
		s.pending = append(s.pending, core.Event{
			Type: core.EventError,
			Err:  fmt.Errorf("kimi: the upstream returned no content"),
		})
	}

	finish := "stop"
	if s.tools > 0 {
		finish = "tool_calls"
	}
	s.pending = append(s.pending, core.Event{Type: core.EventDone, Finish: finish})
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

// directChat performs one non-streamed-request/streamed-response exchange
// against the vendor's coding API.
func (c *Client) directChat(ctx context.Context, req *core.ChatRequest, tok storedToken) (core.Stream, error) {
	body, err := json.Marshal(buildOpenAIRequest(req, c.cfg.DefaultModel, c.requestShapingFor()))
	if err != nil {
		return nil, fmt.Errorf("kimi: encoding the request failed: %w", err)
	}

	endpoint := c.apiBase(tok) + chatCompletionsPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("kimi: building the request failed: %w", err)
	}
	// The credential is ours; everything else is the CLI's, so the request is
	// not distinguishable from one the official client would send.  See
	// identity.go.
	httpReq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	applyChatHeaders(httpReq.Header, c.identity())

	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("kimi: the request to %s failed: %w", endpoint, err)
	}

	if resp.StatusCode != http.StatusOK {
		detail := readUpstreamDetail(resp)
		// The status is carried on the error, not just inside its text, so the
		// health layer can tell "the vendor refused this credential" (401/403,
		// which no retry fixes) from a transient upstream problem without
		// parsing a message.  The rendered text is unchanged.
		return nil, &upstreamStatusError{Where: endpoint, Status: resp.StatusCode, Detail: detail}
	}

	// The vendor may answer a streaming request with a single JSON document.
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "event-stream") {
		return newDirectJSONStream(c, resp.Body), nil
	}
	return newDirectStream(c, resp.Body), nil
}

func readUpstreamDetail(resp *http.Response) string {
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ""
	}
	return upstreamDetail(body)
}

// directJSONStream wraps a whole-document response so callers see the same
// event sequence as a streamed one.
type directJSONStream struct {
	client  *Client
	body    io.ReadCloser
	pending []core.Event
	once    sync.Once
	read    bool
}

func newDirectJSONStream(c *Client, body io.ReadCloser) *directJSONStream {
	return &directJSONStream{client: c, body: body}
}

func (s *directJSONStream) Recv() (core.Event, error) {
	if !s.read {
		s.read = true
		s.parse()
	}
	if len(s.pending) > 0 {
		ev := s.pending[0]
		s.pending = s.pending[1:]
		return ev, nil
	}
	s.Close()
	return core.Event{}, io.EOF
}

func (s *directJSONStream) Close() error {
	s.once.Do(func() {
		if s.body != nil {
			_ = s.body.Close()
		}
	})
	return nil
}

func (s *directJSONStream) parse() {
	defer func() { _ = s.body.Close() }()
	body, err := io.ReadAll(io.LimitReader(s.body, 8<<20))
	if err != nil {
		s.pending = append(s.pending, core.Event{Type: core.EventError, Err: fmt.Errorf("kimi: reading the response failed: %w", err)})
		s.pending = append(s.pending, core.Event{Type: core.EventDone, Finish: "stop"})
		return
	}

	var chunk openAIChunk
	if err := json.Unmarshal(body, &chunk); err != nil {
		s.pending = append(s.pending, core.Event{Type: core.EventError, Err: fmt.Errorf("kimi: the response was not JSON: %w", err)})
		s.pending = append(s.pending, core.Event{Type: core.EventDone, Finish: "stop"})
		return
	}

	var stream directStream
	stream.client = s.client
	stream.handleChunk(string(body))
	stream.done = true
	if chunk.Usage == nil {
		// handleChunk already queued any usage; nothing extra to do.
		_ = chunk
	}
	s.pending = stream.pending
}
