package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Responses API (/v1/responses)
// ---------------------------------------------------------------------------
//
// Codex CLI dropped Chat Completions support: its WireApi enum has exactly one
// variant, Responses, and the default (and only) wire_api a provider may name is
// "responses".  Pointing Codex at a Chat-Completions-only endpoint therefore
// fails, and pointing it at codex++ means that relay has to translate Responses
// back into Chat -- which it refuses to do whenever the request carries encrypted
// agent content, answering 400 before the request ever reaches a vendor.
//
// This file is the native surface that removes the relay from that path.  It
// speaks Responses on the inside and translates to the same core.ChatRequest the
// Chat endpoint builds, so every module, every pool and the whole failover
// machinery are reused unchanged.
//
// The one design decision worth stating plainly: an item whose content is
// encrypted is not rejected.  A relay cannot translate ciphertext it does not
// hold the key for, so its only honest answer is a 400 -- but this gateway is
// not trying to interpret the bytes.  It carries them through: an encrypted part
// is replaced by the one fact the model can act on (that a delegated agent
// message exists, with its plaintext envelope) and the ciphertext is dropped
// rather than forwarded or invented.  A decryption service is deliberately not
// built: the gateway cannot read the payload, so any "plaintext" it produced
// would be a fabrication.

// ---------------------------------------------------------------------------
// inbound wire types
// ---------------------------------------------------------------------------

// responsesRequest is the subset of the request client2api understands.  As in
// openai.go, unknown fields are ignored on purpose: a new Responses option must
// never turn a working request into a 400.
type responsesRequest struct {
	Model string `json:"model"`
	// Input is either a plain string (one user turn) or an array of typed items.
	Input json.RawMessage `json:"input,omitempty"`
	// Instructions is the system-level preamble.  It becomes the leading system
	// message, because a module's upstream speaks roles rather than a separate
	// instruction channel.
	Instructions string          `json:"instructions,omitempty"`
	Tools        []responsesTool `json:"tools,omitempty"`
	ToolChoice   json.RawMessage `json:"tool_choice,omitempty"`
	Temperature  *float64        `json:"temperature,omitempty"`
	TopP         *float64        `json:"top_p,omitempty"`
	// MaxOutputTokens is Responses' spelling of max_tokens.
	MaxOutputTokens *int `json:"max_output_tokens,omitempty"`
	Stream          bool `json:"stream,omitempty"`
	// Reasoning carries the thinking level as {"effort":…}.
	Reasoning effortField `json:"reasoning,omitempty"`
	// reasoning_effort is not part of the Responses schema, but Codex and the
	// relays in front of it have both been seen to send it flat.  Accepting it
	// costs nothing and avoids silently dropping a level the caller meant.
	ReasoningEffort effortField `json:"reasoning_effort,omitempty"`
	User            string      `json:"user,omitempty"`
	// Store and PreviousResponseID are part of the stateful Responses API that
	// this gateway does not implement: it is stateless, and the caller replays
	// its history in Input.  They are accepted and ignored rather than rejected,
	// so a client that sets them still works.
	Store              *bool          `json:"store,omitempty"`
	PreviousResponseID string         `json:"previous_response_id,omitempty"`
	Options            map[string]any `json:"client2api,omitempty"`
	// ConversationID / Metadata mirror the Chat endpoint so a client that names
	// its conversation keeps its account stickiness.
	ConversationID      string          `json:"conversation_id,omitempty"`
	ConversationIDCamel string          `json:"conversationId,omitempty"`
	Metadata            json.RawMessage `json:"metadata,omitempty"`
}

// responsesTool is a tool declaration.  Responses flattens what Chat nests under
// function: the name, description and parameters sit directly beside type.
type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	// Function carries the Chat-nested shape too, because some clients and
	// relays send a Responses request with Chat-shaped tools inside it. Reading
	// both means neither has to be detected and special-cased.
	Function *struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function,omitempty"`
}

// responsesInputItem is one element of the Input array.  Type is the
// discriminator; only the fields relevant to that type are populated.
//
// Every type Codex emits is represented: a message, a function call and its
// output, a reasoning echo, and the agent_message plus compaction items that the
// encrypted-content path produces.  A type this gateway does not know is still
// decoded without error (it lands in the zero value with only Type set) and is
// skipped during translation rather than failing the request, so a newer Codex
// cannot break an older gateway.
type responsesInputItem struct {
	Type string `json:"type"`
	// Role is the message author: user / assistant / system / developer.
	Role string `json:"role,omitempty"`
	// Content is polymorphic: a plain string, or an array of typed parts.
	Content json.RawMessage `json:"content,omitempty"`
	// ID echoes the provider id for items replayed from a prior response.
	ID string `json:"id,omitempty"`

	// CallID / Name / Arguments / Output describe a function call and its result.
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`

	// Summary is the reasoning summary on a reasoning item, carried as parts.
	Summary []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"summary,omitempty"`

	// EncryptedContent is the opaque blob on a reasoning or compaction item.
	// It is carried through as bytes and never parsed: the key is not ours.
	EncryptedContent string `json:"encrypted_content,omitempty"`

	// Author / Recipient describe an agent_message, the delegated-agent envelope
	// whose payload sits in Content as an encrypted_content part.
	Author    string `json:"author,omitempty"`
	Recipient string `json:"recipient,omitempty"`

	// Status is present on items replayed from a prior response.
	Status string `json:"status,omitempty"`
}

// ---------------------------------------------------------------------------
// outbound wire types
// ---------------------------------------------------------------------------

// responseObject is the Responses envelope.  Only the fields Codex reads are
// filled; an absent field is omitted rather than sent as null so the object
// keeps the shape a client validates against.
type responseObject struct {
	ID                string                     `json:"id"`
	Object            string                     `json:"object"`
	CreatedAt         int64                      `json:"created_at"`
	Status            string                     `json:"status"`
	Model             string                     `json:"model"`
	Output            []responseOutput           `json:"output"`
	Usage             *responseUsage             `json:"usage,omitempty"`
	IncompleteDetails *responseIncompleteDetails `json:"incomplete_details,omitempty"`
}

type responseIncompleteDetails struct {
	Reason string `json:"reason"`
}

// responseOutput is one output item.  Message content, a function call and a
// reasoning block are all represented by this one struct because they share the
// id/type/status spine and differ only in which extra fields are populated.
type responseOutput struct {
	Type   string `json:"type"`
	ID     string `json:"id,omitempty"`
	Status string `json:"status,omitempty"`

	// message
	Role    string                  `json:"role,omitempty"`
	Content []responseOutputContent `json:"content,omitempty"`

	// function_call
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`

	// reasoning
	Summary []responseOutputSummary `json:"summary,omitempty"`
	// EncryptedContent is emitted as null on reasoning items: this gateway holds
	// no ciphertext of its own to echo, and a stateless client replays what it
	// received.  Sending an empty string would be worse than sending null,
	// because "" is a value a client might try to decrypt.
	EncryptedContent *string `json:"encrypted_content,omitempty"`
}

type responseOutputContent struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	Annotations []any  `json:"annotations,omitempty"`
}

type responseOutputSummary struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responseUsage struct {
	InputTokens         int                        `json:"input_tokens"`
	InputTokensDetails  *responseInputTokenDetail  `json:"input_tokens_details,omitempty"`
	OutputTokens        int                        `json:"output_tokens"`
	OutputTokensDetails *responseOutputTokenDetail `json:"output_tokens_details,omitempty"`
	TotalTokens         int                        `json:"total_tokens"`
}

type responseInputTokenDetail struct {
	CachedTokens int `json:"cached_tokens"`
}

type responseOutputTokenDetail struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

func toResponseUsage(u *core.Usage) *responseUsage {
	if u == nil {
		return nil
	}
	out := &responseUsage{
		InputTokens:  u.PromptTokens,
		OutputTokens: u.CompletionTokens,
		TotalTokens:  u.TotalTokens,
	}
	if out.TotalTokens == 0 {
		out.TotalTokens = out.InputTokens + out.OutputTokens
	}
	if u.CachedTokens > 0 {
		out.InputTokensDetails = &responseInputTokenDetail{CachedTokens: u.CachedTokens}
	}
	if u.ReasoningTokens > 0 {
		out.OutputTokensDetails = &responseOutputTokenDetail{ReasoningTokens: u.ReasoningTokens}
	}
	return out
}

// ---------------------------------------------------------------------------
// request translation
// ---------------------------------------------------------------------------

// reasoningEffortForResponses is the Responses spelling of the thinking level.
// The flat field wins for the same reason as in the Chat endpoint: that is what
// a Codex-shaped caller sends.
func reasoningEffortForResponses(w *responsesRequest) string {
	if w == nil {
		return ""
	}
	if v := normalizeEffort(string(w.ReasoningEffort)); v != "" {
		return v
	}
	if v := normalizeEffort(string(w.Reasoning)); v != "" {
		return v
	}
	if w.Options != nil {
		if s, ok := w.Options["reasoning_effort"].(string); ok {
			if v := normalizeEffort(s); v != "" {
				return v
			}
		}
	}
	return ""
}

// resolveResponsesConversationID mirrors resolveConversationID for the
// Responses body, whose conversation identity arrives in the same two places.
func resolveResponsesConversationID(w *responsesRequest) string {
	if w == nil {
		return ""
	}
	if len(w.Metadata) > 0 {
		var meta struct {
			ConversationID      string `json:"conversation_id"`
			ConversationIDCamel string `json:"conversationId"`
		}
		if err := json.Unmarshal(w.Metadata, &meta); err == nil {
			if v := strings.TrimSpace(meta.ConversationID); v != "" {
				return v
			}
			if v := strings.TrimSpace(meta.ConversationIDCamel); v != "" {
				return v
			}
		}
	}
	if v := strings.TrimSpace(w.ConversationID); v != "" {
		return v
	}
	return strings.TrimSpace(w.ConversationIDCamel)
}

// toCoreRequestFromResponses translates a Responses body into the vendor-neutral
// request every module consumes.
//
// The two shapes differ in more than field names, and the differences are the
// whole point of this function:
//
//   - Responses has no "messages" array. It has an ordered "input" list in which
//     a tool call, its result and a reasoning echo are siblings of the messages
//     rather than nested inside them. Chat models express the same facts as an
//     assistant turn carrying tool_calls followed by tool-role turns, so the
//     flattened list has to be re-paired: a function_call opens an assistant
//     turn and the matching function_call_output closes it with a tool-role
//     message keyed by call_id.
//   - "instructions" is a separate channel. It becomes the leading system
//     message, because a vendor's upstream takes system prompts as messages.
//   - A reasoning item carries provider-encrypted reasoning that no other vendor
//     can decrypt. Its human-readable summary is kept (it is genuinely useful
//     context) and the ciphertext is dropped.
func toCoreRequestFromResponses(w *responsesRequest) (*core.ChatRequest, error) {
	req := &core.ChatRequest{
		Model:          w.Model,
		ToolChoice:     w.ToolChoice,
		Temperature:    w.Temperature,
		TopP:           w.TopP,
		MaxTokens:      w.MaxOutputTokens,
		Stream:         w.Stream,
		User:           w.User,
		ConversationID: resolveResponsesConversationID(w),
		Options:        optionsWithReasoningEffort(w.Options, reasoningEffortForResponses(w)),
	}

	if strings.TrimSpace(w.Instructions) != "" {
		req.Messages = append(req.Messages, core.Message{Role: "system", Content: w.Instructions})
	}

	messages, err := translateResponsesInput(w.Input)
	if err != nil {
		return nil, err
	}
	req.Messages = append(req.Messages, messages...)

	for _, t := range w.Tools {
		name, desc, params := t.resolved()
		if name == "" {
			// A built-in tool (web_search, file_search, computer_use …) has no
			// function name this gateway could forward.  Skipping it keeps a
			// mixed request working instead of failing the whole call.
			continue
		}
		req.Tools = append(req.Tools, core.Tool{
			Type:        "function",
			Name:        name,
			Description: desc,
			Parameters:  params,
		})
	}

	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("input must not be empty")
	}
	return req, nil
}

// resolved reads a tool's name/description/parameters from whichever shape it
// arrived in.
func (t responsesTool) resolved() (name, description string, parameters json.RawMessage) {
	if t.Name != "" {
		return t.Name, t.Description, t.Parameters
	}
	if t.Function != nil {
		return t.Function.Name, t.Function.Description, t.Function.Parameters
	}
	return "", "", nil
}

// translateResponsesInput turns the Input array into ordered Chat messages.
//
// Input is either a string or an array; both are accepted, and so is a missing
// one (a request whose only content is instructions is unusual but legal).  An
// unknown item type is skipped, never fatal.
func translateResponsesInput(raw json.RawMessage) ([]core.Message, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, nil
	}

	// A bare string is one user turn.
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil, fmt.Errorf("input: %w", err)
		}
		return []core.Message{{Role: "user", Content: s}}, nil
	}
	if trimmed[0] != '[' {
		return nil, fmt.Errorf("input must be a string or an array of items")
	}

	var items []responsesInputItem
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, fmt.Errorf("input: %w", err)
	}

	var (
		out []core.Message
		// pending accumulates a function_call and its output into one assistant
		// turn plus one tool turn, which is how Chat expresses a round trip.
		// It is flushed whenever a non-tool item interrupts the sequence.
		pending *core.Message
		// pendingReasoning holds a reasoning item that has not yet been claimed
		// by the turn it explains.  Codex replays reasoning immediately before
		// the function call it produced, so it must attach to that call's
		// assistant turn rather than becoming a turn of its own.
		pendingReasoning string
	)

	// flushPending emits whatever is open, in the order the items arrived: the
	// assistant turn that carries any tool calls, then any reasoning that no
	// turn claimed.  Emitting the reasoning rather than dropping it matters —
	// it is real context the model produced.
	flushPending := func() {
		if pending != nil {
			if pending.Reasoning == "" && pendingReasoning != "" {
				pending.Reasoning = pendingReasoning
				pendingReasoning = ""
			}
			out = append(out, *pending)
			pending = nil
		}
		if pendingReasoning != "" {
			out = append(out, core.Message{Role: "assistant", Reasoning: pendingReasoning})
			pendingReasoning = ""
		}
	}

	for i, it := range items {
		switch it.Type {
		case "function_call", "custom_tool_call", "local_shell_call":
			// A tool invocation.  In Chat it is an assistant turn that carries
			// tool_calls; several consecutive calls belong to one such turn, so
			// an open one is appended to rather than flushed.
			callID := strings.TrimSpace(it.CallID)
			if callID == "" {
				callID = strings.TrimSpace(it.ID)
			}
			if pending == nil {
				pending = &core.Message{Role: "assistant"}
			}
			// The reasoning that preceded this call belongs to this turn.  In
			// Chat it rides on the assistant message's reasoning_content.
			if pending.Reasoning == "" && pendingReasoning != "" {
				pending.Reasoning = pendingReasoning
				pendingReasoning = ""
			}
			pending.ToolCalls = append(pending.ToolCalls, core.ToolCall{
				ID:        callID,
				Type:      "function",
				Name:      strings.TrimSpace(it.Name),
				Arguments: rawToArgumentString(it.Arguments),
			})

		case "function_call_output", "custom_tool_call_output":
			// The result of a prior call.  A tool-role turn keyed by call_id is
			// what Chat expects, so the accumulated assistant turn is emitted
			// first and the result follows it.
			flushPending()
			callID := strings.TrimSpace(it.CallID)
			if callID == "" {
				callID = strings.TrimSpace(it.ID)
			}
			out = append(out, core.Message{
				Role:       "tool",
				ToolCallID: callID,
				Content:    rawToText(it.Output),
			})

		case "reasoning":
			// Replayed reasoning.  Only the human-readable summary survives: the
			// encrypted_content is another vendor's ciphertext and cannot be
			// replayed anywhere but back to its issuer.  The summary is buffered
			// until the turn it explains is seen, so it stays adjacent to the
			// call rather than becoming a separate turn.
			if text := reasoningSummaryText(it); text != "" {
				if pendingReasoning == "" {
					pendingReasoning = text
				} else {
					pendingReasoning += "\n" + text
				}
			}

		case "agent_message":
			// A delegated-agent envelope.  Its payload is encrypted, but the
			// envelope itself is plaintext and is the part the model can act on:
			// who sent it and to whom.  The ciphertext is dropped rather than
			// forwarded, because passing a blob no vendor can read would put an
			// unreadable turn in the history without adding information.
			flushPending()
			out = append(out, core.Message{
				Role:    "user",
				Content: agentMessageEnvelopeText(it),
			})

		case "compaction":
			// A prior turn's context fold.  Its plaintext summary, when the
			// caller kept one, is preserved as an assistant turn; the encrypted
			// form is dropped for the same reason as above.
			flushPending()
			if text := strings.TrimSpace(it.EncryptedContent); text != "" && !looksEncrypted(text) {
				out = append(out, core.Message{Role: "assistant", Content: text})
			}

		case "message", "":
			// A plain message.  An empty type is treated as a message because a
			// role-carrying item with no type is the shape a lenient client
			// sends.  A system/developer role keeps its role so a mid-history
			// instruction is not silently promoted to the user.
			role := responsesRoleToChatRole(it.Role)
			if role == "" {
				continue
			}
			text, parts, err := parseResponsesContent(it.Content)
			if err != nil {
				return nil, fmt.Errorf("input[%d].content: %w", i, err)
			}
			msg := core.Message{Role: role, Content: text, Parts: parts}
			// An assistant turn claims a reasoning summary that preceded it;
			// anything else closes the open items first so the order holds.
			if role == "assistant" && pending == nil && pendingReasoning != "" {
				msg.Reasoning = pendingReasoning
				pendingReasoning = ""
			} else {
				flushPending()
			}
			out = append(out, msg)

		default:
			// An item type this gateway does not model.  Skipping is deliberate:
			// a newer Codex may add one, and a gateway that 400s on it would
			// break every request rather than one field.
			continue
		}
	}
	flushPending()
	return out, nil
}

// Normalize Responses parts before using the shared Chat content decoder.
func parseResponsesContent(raw json.RawMessage) (string, []core.ContentPart, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return parseContent(raw)
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &parts); err != nil {
		return "", nil, err
	}
	for _, part := range parts {
		var kind string
		if err := json.Unmarshal(part["type"], &kind); err != nil && len(part["type"]) > 0 {
			return "", nil, err
		}
		switch kind {
		case "output_text":
			part["type"] = json.RawMessage(`"text"`)
		case "input_image":
			var url string
			if err := json.Unmarshal(part["image_url"], &url); err != nil {
				return "", nil, fmt.Errorf("input_image.image_url: %w", err)
			}
			image := map[string]json.RawMessage{"url": part["image_url"]}
			if detail := part["detail"]; len(detail) > 0 {
				image["detail"] = detail
			}
			encoded, err := json.Marshal(image)
			if err != nil {
				return "", nil, err
			}
			part["type"] = json.RawMessage(`"image_url"`)
			part["image_url"] = encoded
		}
	}
	normalized, err := json.Marshal(parts)
	if err != nil {
		return "", nil, err
	}
	return parseContent(normalized)
}

// responsesRoleToChatRole maps a Responses role onto a Chat role.  "developer"
// becomes "system" because that is the role whose precedence it carries.
func responsesRoleToChatRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "user":
		return "user"
	case "assistant":
		return "assistant"
	case "system", "developer":
		return "system"
	case "tool":
		return "tool"
	default:
		return ""
	}
}

// reasoningSummaryText concatenates a reasoning item's summary parts.
func reasoningSummaryText(it responsesInputItem) string {
	var parts []string
	for _, s := range it.Summary {
		if t := strings.TrimSpace(s.Text); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}

// agentMessageEnvelopeText renders the plaintext part of an agent_message.  The
// envelope's text parts (author, recipient, task name) are the readable half and
// are kept; the encrypted part is replaced by a marker stating that content
// exists but is not readable in this protocol.  The marker is honest about what
// happened -- it does not pretend to be the payload.
//
// Only text parts are extracted.  Rendering the raw content array instead would
// copy the ciphertext into the prompt, which is the one thing that must not
// happen: the model cannot read it, and it would be forwarded to a vendor that
// never issued it.
func agentMessageEnvelopeText(it responsesInputItem) string {
	var sb strings.Builder
	if it.Author != "" || it.Recipient != "" {
		fmt.Fprintf(&sb, "[agent message from %s to %s]", orUnknown(it.Author), orUnknown(it.Recipient))
	}
	if text, _, err := parseContent(it.Content); err == nil {
		if text = strings.TrimSpace(text); text != "" {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(text)
		}
	}
	if hasEncryptedPart(it.Content) || it.EncryptedContent != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString("[encrypted agent payload omitted]")
	}
	if sb.Len() == 0 {
		return "[agent message]"
	}
	return sb.String()
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

// hasEncryptedPart reports whether a content field carries an encrypted part.
// It reads the same shape the reference project does, so an item it would have
// rejected is the item this function recognises.
func hasEncryptedPart(content json.RawMessage) bool {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 {
		return false
	}
	if trimmed[0] == '{' {
		var one struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(trimmed, &one) == nil {
			return one.Type == "encrypted_content"
		}
		return false
	}
	if trimmed[0] != '[' {
		return false
	}
	var parts []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(trimmed, &parts) != nil {
		return false
	}
	for _, p := range parts {
		if p.Type == "encrypted_content" {
			return true
		}
	}
	return false
}

// looksEncrypted is a conservative test for "this is a ciphertext blob, not
// prose".  It exists for the compaction item, whose EncryptedContent may hold
// either a real ciphertext or -- as one relay does it -- a plaintext summary
// smuggled through the same field.  Only the second is usable, and a long
// high-entropy base64 run is how the first is told apart.
func looksEncrypted(s string) bool {
	if len(s) < 256 {
		return false
	}
	// A base64 blob has no spaces; prose does.
	if strings.ContainsAny(s, " \n\t") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '+', r == '/', r == '=', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// rawToText renders a raw JSON value as text.  A string is returned verbatim; an
// object or array is re-marshalled so a structured tool output still reaches the
// model as readable JSON rather than as a Go-printed map.
func rawToText(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) == nil {
			return s
		}
	}
	return string(trimmed)
}

// rawToArgumentString renders tool arguments.  Chat wants a JSON string, so an
// object is marshalled back to a string and a string is passed through.
func rawToArgumentString(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) == nil {
			return s
		}
	}
	return string(trimmed)
}

// ---------------------------------------------------------------------------
// the endpoint
// ---------------------------------------------------------------------------

func (s *server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "use POST")
		return
	}
	s.stats.addRequest()

	rec := UsageRecord{At: time.Now(), StartedAt: time.Now()}
	defer func() {
		rec.At = time.Now()
		s.opts.Usage.Record(rec)
	}()

	stat := newChatStat(rec.StartedAt, "", false)
	defer stat.done()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		stat.status = http.StatusBadRequest
		s.fail(&rec)
		writeError(w, http.StatusBadRequest, "invalid_request_error", "reading body: "+err.Error())
		return
	}

	var wire responsesRequest
	if err := json.Unmarshal(body, &wire); err != nil {
		stat.status = http.StatusBadRequest
		s.fail(&rec)
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}
	rec.Model = wire.Model
	stat.model = wire.Model
	stat.mode = chatMode(wire.Stream)

	req, err := toCoreRequestFromResponses(&wire)
	if err != nil {
		stat.status = http.StatusBadRequest
		s.fail(&rec)
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	req.ConversationRequestID = strings.TrimSpace(r.Header.Get("X-Conversation-Request-ID"))
	rec.SessionID = sessionIDFor(req)
	rec.ReasoningEffort = reasoningEffortForResponses(&wire)

	s.routeAndServe(w, r, routeInput{
		requested:    wire.Model,
		req:          req,
		stream:       wire.Stream,
		modelForWire: wire.Model,
		rec:          &rec,
		stat:         stat,
		emitter:      responsesEmitter{},
	})
}
