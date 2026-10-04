package workbuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Desktop expert market + the expert summon/use event sequences.
//
// Ported from the reference internal/upstream/desktop.go (MIT), expert section
// (:370-582).  This is the linchpin for the expert chores: the reference
// established that the vendor only scores an `expert_actual_use` join event
// whose requestId is the requestId the SERVER issued, so a locally invented
// UUID scores nothing.  DesktopChatWithExpert therefore spends one real (and
// very cheap) /v2/chat/completions call purely to capture that id out of the
// SSE stream before any expert event is reported.
//
// desktopChatSequence / reportDesktopEvent already exist in tasks.go: those own
// the ordinary desktop event chain, and this file reuses them rather than
// opening a second pathway to /v2/report.

// DesktopEvent is one desktop report event.  It is an alias, not a defined type,
// so the sequences below can be handed straight to reportDesktopEvent.
type DesktopEvent = map[string]any

// expertListPath is the market catalogue route on the chat host.
const expertListPath = "/portal/operation-platform/market/expert/list"

// expertScanLimit caps how much SSE we are willing to read while hunting for the
// server requestId.  The id is in the first frames; a stream that never carries
// one is a broken stream, not a huge one.
const expertScanLimit = 1 << 20

// MarketExpert is one entry of the vendor's expert market.
type MarketExpert struct {
	ExpertID      string `json:"expert_id"`
	ExpertType    string `json:"expert_type"`
	DisplayNameZH string `json:"display_name_zh"`
	ProfessionZH  string `json:"profession_zh"`
	Version       string `json:"version"`
	Categories    []any  `json:"categories"`
}

// idRegex accepts the server-issued request id shapes (32 hex chars, optionally
// prefixed by cmb-).  Anything else was made up locally and will not score.
var idRegex = regexp.MustCompile(`^(cmb-)?[0-9a-f]{32}$`)

// MarketExpertList reads the market catalogue.  expertType is optional; empty
// means "every type".
func (c *Client) MarketExpertList(ctx context.Context, a *Auth, expertType string) ([]MarketExpert, error) {
	payload := map[string]any{
		"page":       1,
		"page_size":  20,
		"sort_by":    "reco_rank",
		"sort_order": "desc",
	}
	if strings.TrimSpace(expertType) != "" {
		payload["expert_type"] = expertType
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	base := c.up.chatBase(a)
	env, err := c.taskJSON(ctx, http.MethodPost, base+expertListPath, body, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", desktopUserAgent)
		req.Header.Set("X-Domain", base)
		req.Header.Set("X-Product", "SaaS")
		if uid := a.UIDValue(); uid != "" {
			req.Header.Set("X-User-Id", uid)
		}
	})
	if err != nil {
		return nil, err
	}
	if err := envelopeError(http.StatusOK, env); err != nil {
		return nil, err
	}
	var out struct {
		Experts []MarketExpert `json:"experts"`
	}
	if err := decodePayload(env.Data, &out); err != nil {
		return nil, fmt.Errorf("expert list parse: %w", err)
	}
	return out.Experts, nil
}

// DesktopChatWithExpert runs one throwaway desktop chat whose only purpose is to
// capture the server-issued requestId.  It returns the conversation id it
// invented and the server's request id; every expert event must carry the
// latter.
func (c *Client) DesktopChatWithExpert(ctx context.Context, a *Auth, expertID string) (conversationID, requestID string, err error) {
	conversationID = fmt.Sprintf("wb2api-conv-%d", time.Now().UnixNano())

	body, err := json.Marshal(map[string]any{
		"model": "fast-model",
		"messages": []map[string]any{
			{"role": "system", "content": "You are a helpful assistant. 当前处于中文环境，使用简体中文回答。"},
			{"role": "user", "content": "1+1等于几？直接回答。"},
		},
		"agent":          "cli",
		"temperature":    1,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	})
	if err != nil {
		return "", "", err
	}

	if ctx == nil {
		ctx = context.Background()
	}
	// The stream must not be cut off mid-frame, so this uses the no-timeout chat
	// client and caps the attempt with its own deadline instead.
	reqCtx, cancel := context.WithTimeout(ctx, desktopExpertChatTimeout)
	defer cancel()

	base := c.up.chatBase(a)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, base+chatCompletionsPath, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", desktopUserAgent)
	req.Header.Set("X-Domain", base)
	req.Header.Set("X-Product", "SaaS")
	if uid := a.UIDValue(); uid != "" {
		req.Header.Set("X-User-Id", uid)
	}
	req.Header.Set("X-Conversation-ID", conversationID)
	req.Header.Set("X-Request-ID", fmt.Sprintf("%d", time.Now().UnixNano()))
	req.Header.Set("X-Agent-Intent", "craft")
	req.Header.Set("X-Agent-Type", "main")
	req.Header.Set("X-IDE-Name", "WorkBuddy")
	req.Header.Set("X-IDE-Type", "WorkBuddy")
	req.Header.Set("X-IDE-Version", desktopVersion)
	req.Header.Set("x-codebuddy-request", "1")
	if strings.TrimSpace(expertID) != "" {
		req.Header.Set("X-Expert-Id", expertID)
	}

	resp, err := c.up.chatHTTP().Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw := make([]byte, 8192)
		n, _ := resp.Body.Read(raw)
		return "", "", fmt.Errorf("chat http %d: %s", resp.StatusCode, truncate(string(raw[:n]), 200))
	}

	buf := make([]byte, 0, 8192)
	chunk := make([]byte, 8192)
	for {
		n, readErr := resp.Body.Read(chunk)
		if n > 0 && len(buf) < expertScanLimit {
			buf = append(buf, chunk[:n]...)
			if id, ok := scanServerRequestID(buf); ok {
				return conversationID, id, nil
			}
		}
		if readErr != nil {
			break
		}
	}
	return conversationID, "", errors.New("expert chat: the SSE stream carried no server-issued requestId")
}

// desktopExpertChatTimeout bounds the throwaway stream.  It is a backstop, not a
// performance target: the reference waits for the first id-bearing frame too.
const desktopExpertChatTimeout = 60 * time.Second

// scanServerRequestID finds the first `"id":"<value>"` whose value looks like a
// server id.  A locally-invented id elsewhere in the frame is skipped.
func scanServerRequestID(buf []byte) (string, bool) {
	const marker = `"id":"`
	off := 0
	for {
		i := bytes.Index(buf[off:], []byte(marker))
		if i < 0 {
			return "", false
		}
		start := off + i + len(marker)
		end := bytes.IndexByte(buf[start:], '"')
		if end < 0 {
			return "", false
		}
		cand := string(buf[start : start+end])
		if idRegex.MatchString(cand) {
			return cand, true
		}
		off = start + end
	}
}

// expertCategory is the first category as a string, or the catch-all bucket.
func expertCategory(e MarketExpert) string {
	if len(e.Categories) > 0 {
		if s, ok := e.Categories[0].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return "expert-all"
}

// expertVersion is the expert's version, or the reference's 1.0.0 default.
func expertVersion(e MarketExpert) string {
	if v := strings.TrimSpace(e.Version); v != "" {
		return v
	}
	return "1.0.0"
}

// DesktopExpertSummonSequence is the three-event summon chain: click the expert
// card, click 立即召唤, and the summoned confirmation.
func DesktopExpertSummonSequence(e MarketExpert) []DesktopEvent {
	cat := expertCategory(e)
	ver := expertVersion(e)
	return []DesktopEvent{
		{
			"eventCode":   "web_element_click",
			"source":      e.ExpertID,
			"type":        cat,
			"version":     ver,
			"elementId":   "expert_summon_click",
			"elementName": "立即召唤",
			"pageURL":     "/C:/Program%20Files/WorkBuddy/resources/app.asar/renderer/index.html",
		},
		{
			"eventCode":   "expert_summon_click",
			"id":          e.ExpertID,
			"name":        e.DisplayNameZH,
			"expertTitle": e.ProfessionZH,
			"type":        "expert-all",
			"position":    0,
			"expertType":  e.ExpertType,
			"version":     ver,
			"mode":        "LOCAL",
		},
		{
			"eventCode":   "expert_summoned",
			"id":          e.ExpertID,
			"name":        e.DisplayNameZH,
			"expertTitle": e.ProfessionZH,
			"type":        "expert-all",
		},
	}
}

// DesktopExpertActualUseEvent is the expert_actual_use event for a craft-mode
// expert session.
func DesktopExpertActualUseEvent(e MarketExpert, conversationID, requestID string) DesktopEvent {
	ev := desktopExpertActualUse(e, conversationID, requestID)
	ev["mode"] = "craft"
	return ev
}

// DesktopExpertActualUseLocal is the same event in LOCAL mode, which is the mode
// the connector-bound experts (e.g. Expert_lighthouse) report in.
func DesktopExpertActualUseLocal(e MarketExpert, conversationID, requestID string) DesktopEvent {
	ev := desktopExpertActualUse(e, conversationID, requestID)
	ev["mode"] = "LOCAL"
	return ev
}

// desktopExpertActualUse builds the shared body of the two events above.
func desktopExpertActualUse(e MarketExpert, conversationID, requestID string) DesktopEvent {
	return DesktopEvent{
		"eventCode":      "expert_actual_use",
		"id":             e.ExpertID,
		"name":           e.DisplayNameZH,
		"expertTitle":    e.ProfessionZH,
		"type":           expertCategory(e),
		"expertType":     e.ExpertType,
		"source":         "builtin",
		"version":        expertVersion(e),
		"cost":           9000,
		"characterCount": 14,
		"conversationId": conversationID,
		"requestId":      requestID,
		"messageId":      "msg-" + tailOf(requestID, 8),
	}
}
