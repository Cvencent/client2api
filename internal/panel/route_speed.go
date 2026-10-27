package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"client2api/internal/core"
)

const (
	routeSpeedMaxTokens = 256
	routeSpeedTimeout   = 60 * time.Second
)

// routeSpeedResult is one platform's live probe.  A failed probe is still a
// result: the operator needs a row per platform even when one upstream is down.
type routeSpeedResult struct {
	Position         int     `json:"position"`
	Platform         string  `json:"platform"`
	ModelID          string  `json:"model_id"`
	OK               bool    `json:"ok"`
	AccountID        string  `json:"account_id,omitempty"`
	FirstEventMS     int64   `json:"first_event_ms"`
	ElapsedMS        int64   `json:"elapsed_ms"`
	CompletionTokens int     `json:"completion_tokens,omitempty"`
	TokensPerSecond  float64 `json:"tokens_per_second,omitempty"`
	ContextLength    int64   `json:"context_length,omitempty"`
	ContextSource    string  `json:"context_source,omitempty"`
	Error            string  `json:"error,omitempty"`
}

type routeSpeedCompletion struct {
	index  int
	result routeSpeedResult
}

type routeSpeedEvent struct {
	Model  string            `json:"model"`
	Result *routeSpeedResult `json:"result,omitempty"`
	Done   bool              `json:"done,omitempty"`
}

// handleRouteSpeed runs one tiny live request through every step in the
// static route chain. The probes are concurrent: a browser asks for SSE and
// receives each platform in completion order, while older JSON callers still
// receive the complete set in route order. The gateway itself never routes this
// request: calling the selected module directly is what guarantees the row
// belongs to the platform the operator is measuring, and each module still
// chooses its own available account through its normal pool.
func (p *panel) handleRouteSpeed(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req routePlanRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	plan, err := p.resolveRoutePlan(req)
	if err != nil {
		status := http.StatusNotFound
		if strings.Contains(err.Error(), "model is required") {
			status = http.StatusBadRequest
		} else if strings.Contains(err.Error(), "no registry") {
			status = http.StatusServiceUnavailable
		}
		writeErr(w, status, err.Error())
		return
	}

	streaming := strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/event-stream")
	var flusher http.Flusher
	if streaming {
		var ok bool
		flusher, ok = w.(http.Flusher)
		if !ok {
			writeErr(w, http.StatusInternalServerError, "streaming is unavailable")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
	}

	completions := make(chan routeSpeedCompletion, len(plan.Steps))
	for i, step := range plan.Steps {
		index, probe := i, step
		go func() {
			completions <- routeSpeedCompletion{
				index:  index,
				result: p.speedRouteStep(r.Context(), probe),
			}
		}()
	}
	if streaming {
		flusher.Flush()
	}

	results := make([]routeSpeedResult, len(plan.Steps))
	for i := 0; i < len(plan.Steps); i++ {
		completion := <-completions
		results[completion.index] = completion.result
		if streaming {
			if err := writeRouteSpeedEvent(w, routeSpeedEvent{
				Model:  plan.Model,
				Result: &completion.result,
			}); err != nil {
				return
			}
			flusher.Flush()
		}
	}

	if streaming {
		_ = writeRouteSpeedEvent(w, routeSpeedEvent{Model: plan.Model, Done: true})
		flusher.Flush()
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"model":   plan.Model,
		"results": results,
	})
}

func writeRouteSpeedEvent(w io.Writer, event routeSpeedEvent) error {
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", body)
	return err
}

func (p *panel) speedRouteStep(parent context.Context, step core.RoutePlanStep) routeSpeedResult {
	result := routeSpeedResult{
		Position:      step.Position,
		Platform:      step.Platform,
		ModelID:       step.ModelID,
		ContextLength: step.ContextLength,
		ContextSource: step.ContextSource,
	}
	if p == nil || p.opts.Registry == nil {
		result.Error = "no registry"
		return result
	}
	client, ok := p.opts.Registry.Get(step.Platform)
	if !ok {
		result.Error = "platform is not registered: " + step.Platform
		return result
	}

	ctx, cancel := context.WithTimeout(parent, routeSpeedTimeout)
	defer cancel()

	// now is the probe's time source.  Production reads the wall clock; a test
	// can install a scripted one so the window arithmetic is asserted exactly
	// instead of depending on where the wall clock falls inside a timer tick.
	now := p.clock()

	maxTokens := routeSpeedMaxTokens
	var servedBy string
	chatReq := &core.ChatRequest{
		Model:     step.ModelID,
		Messages:  []core.Message{{Role: "user", Content: "Count from 1 to 30, digits only, separated by spaces. No other words."}},
		MaxTokens: &maxTokens,
		Stream:    true,
		ServedBy:  &servedBy,
		FreeModel: step.Free,
	}
	started := now()
	stream, err := client.Chat(ctx, chatReq)
	if err != nil {
		result.Error = core.Redact(err.Error())
		result.ElapsedMS = elapsedMS(now, started)
		return result
	}
	if stream == nil {
		result.Error = "platform returned no stream"
		result.ElapsedMS = elapsedMS(now, started)
		return result
	}
	defer stream.Close()

	var (
		firstEvent bool
		usage      core.Usage
		usageSeen  bool
		// lastContent is when the final content frame arrived; the rate is
		// measured over request start -> lastContent.
		//
		// It is deliberately not "first content -> last content".  That span
		// looks like the purest decode measurement, but completion tokens reach
		// this probe through a buffering SSE reader: a vendor that flushes its
		// answer in a few bursts delivers ~100 tokens in frames two or three
		// milliseconds apart.  Dividing by that span reported 400-4400 tok/s for
		// vendors that generate at 100-200 tok/s (DeepSeek-V4.1-Flash), which is
		// a flush artifact rather than a speed, and it made the platform ranking
		// follow buffer sizes instead of throughput.  A vendor's decode cadence
		// is only observable when it happens to stream one token per frame,
		// which no relay guarantees; the whole response window always is
		// observable, it is the throughput the caller actually experiences, and
		// it agrees with the usage page's tok/s (completion tokens over latency).
		lastContent time.Time
	)
	for {
		ev, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			result.Error = core.Redact(recvErr.Error())
			result.ElapsedMS = elapsedMS(now, started)
			return result
		}
		if ev.Type == core.EventError {
			if ev.Err != nil {
				result.Error = core.Redact(ev.Err.Error())
			} else {
				result.Error = "upstream stream error"
			}
			result.ElapsedMS = elapsedMS(now, started)
			return result
		}
		isContent := ev.Type == core.EventDelta || ev.Type == core.EventToolCall || strings.TrimSpace(ev.Reasoning) != ""
		if isContent {
			stamp := now()
			if !firstEvent {
				firstEvent = true
				result.FirstEventMS = elapsedMS(now, started)
			}
			lastContent = stamp
		}
		if ev.Type == core.EventUsage && ev.Usage != nil {
			usage = *ev.Usage
			usageSeen = true
		}
	}

	result.OK = true
	result.AccountID = servedBy
	result.ElapsedMS = elapsedMS(now, started)
	if usageSeen {
		result.CompletionTokens = usage.CompletionTokens
		if usage.CompletionTokens > 0 && firstEvent {
			// Queueing and prompt processing stay in the denominator because
			// they are part of what the caller waits for; only the closing
			// frames sent after the last token are excluded.  A rate is
			// reported only when a content frame actually arrived: with no
			// observable span there is nothing to divide by, so the field stays
			// unset and the UI shows a dash instead of a number invented from a
			// 1ms floor.
			if window := lastContent.Sub(started); window > 0 {
				result.TokensPerSecond = float64(usage.CompletionTokens) / window.Seconds()
			}
		}
	}
	return result
}

func elapsedMS(now func() time.Time, started time.Time) int64 {
	if started.IsZero() {
		return 0
	}
	return now().Sub(started).Milliseconds()
}
