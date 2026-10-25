package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// ContextManagement is the Responses compaction control.  Only the
	// "compaction" type is meaningful here; an entry with any other type is
	// ignored rather than rejected, matching the file's rule that a newer
	// option must never turn a working request into a 400.
	ContextManagement []responsesContextManagement `json:"context_management,omitempty"`
}

// responsesContextManagement is one entry of the request's context_management
// array.  CompactThreshold is the rendered-token count at which the server is
// asked to fold the conversation; a non-positive value means "no automatic
// compaction".
type responsesContextManagement struct {
	Type             string `json:"type,omitempty"`
	CompactThreshold int    `json:"compact_threshold,omitempty"`
}

// compactionThreshold returns the requested automatic-compaction threshold, or
// 0 when the request did not ask for one.
func (w *responsesRequest) compactionThreshold() int {
	if w == nil {
		return 0
	}
	for _, cm := range w.ContextManagement {
		if !strings.EqualFold(strings.TrimSpace(cm.Type), "compaction") {
			continue
		}
		if cm.CompactThreshold > 0 {
			return cm.CompactThreshold
		}
	}
	return 0
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
	// Tools carries a namespace declaration's children.  Codex groups every MCP
	// server's tools -- and its own namespaced built-ins -- under
	// {"type":"namespace"}, with ordinary function declarations inside.  They are
	// flattened on the way to a vendor, because a namespace is a container and
	// Chat neither has nor can validate a tool that carries no schema.
	Tools []responsesTool `json:"tools,omitempty"`
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
	// Input is the payload of a custom tool call.  A "custom_tool_call"
	// (Codex's freeform tools such as exec and apply_patch) carries its body
	// here as freeform text, not as a JSON "arguments" object: the field is
	// absent from the item entirely.  It has to be read separately, because
	// treating the missing arguments as "" produced a history no strict
	// vendor would accept.
	Input json.RawMessage `json:"input,omitempty"`
	// Namespace names the tool namespace a call belongs to.  Codex addresses a
	// namespaced tool as the pair (namespace, name); a vendor only ever sees the
	// flattened name, so the pair is the half this gateway has to restore.
	Namespace string          `json:"namespace,omitempty"`
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
	CallID string `json:"call_id,omitempty"`
	Name   string `json:"name,omitempty"`
	// Namespace accompanies Name when the call addresses a tool inside a
	// namespace; Codex dispatches on the pair, so a call the vendor made under
	// the flattened name is split back before it is sent.  A plain function call
	// leaves this empty.
	Namespace string `json:"namespace,omitempty"`
	Arguments string `json:"arguments,omitempty"`

	// reasoning
	Summary []responseOutputSummary `json:"summary,omitempty"`
	// EncryptedContent is emitted as null on reasoning items: this gateway holds
	// no ciphertext of its own to echo, and a stateless client replays what it
	// received.  Sending an empty string would be worse than sending null,
	// because "" is a value a client might try to decrypt.
	EncryptedContent *string `json:"encrypted_content,omitempty"`

	// compaction
	// CompactionContent is the gateway-owned blob on a compaction item.  It uses
	// the same wire field as OpenAI's opaque state so a client that understands
	// the Responses compaction shape replays it unchanged; the difference is that
	// this gateway can verify and read back what it signed.  See compaction.go.
	CompactionContent string `json:"-"`
}

func (o responseOutput) MarshalJSON() ([]byte, error) {
	type output responseOutput
	switch o.Type {
	case "function_call":
		// Arguments is required on a function call even when no delta arrived.
		return json.Marshal(struct {
			output
			Arguments string `json:"arguments"`
		}{output: output(o), Arguments: o.Arguments})
	case "compaction":
		// A compaction item carries the opaque state under encrypted_content,
		// which is the field a Responses client replays.  It carries no role,
		// content, summary or arguments; emitting them as nulls would make the
		// item's shape disagree with the protocol.
		return json.Marshal(struct {
			Type             string `json:"type"`
			ID               string `json:"id,omitempty"`
			Status           string `json:"status,omitempty"`
			EncryptedContent string `json:"encrypted_content"`
		}{Type: o.Type, ID: o.ID, Status: o.Status, EncryptedContent: o.CompactionContent})
	}
	return json.Marshal(output(o))
}

type responseOutputContent struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
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
func toCoreRequestFromResponses(w *responsesRequest, verify *compactionSigner) (*core.ChatRequest, error) {
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

	messages, err := translateResponsesInput(w.Input, verify)
	if err != nil {
		return nil, err
	}
	req.Messages = append(req.Messages, messages...)

	for _, t := range w.Tools {
		// A namespace is a container, not a tool: it has a name but no schema of
		// its own.  Forwarding it unchanged would offer the vendor a function
		// called `mcp__codegraph` with nothing behind it, and every call to that
		// phantom would be refused as an unsupported call.  Its children are the
		// tools, so the children are what gets declared.
		if children := t.namespaceChildren(); len(children) > 0 {
			namespace := strings.TrimSpace(t.Name)
			for _, child := range children {
				childName, childDesc, childParams := child.resolved()
				if childName == "" {
					continue
				}
				req.Tools = append(req.Tools, core.Tool{
					Type:        "function",
					Name:        chatToolName(namespace, childName),
					Description: childDesc,
					Parameters:  childParams,
				})
			}
			continue
		}
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

// namespacedTool records where a flattened Chat-visible tool name came from.
// The Responses protocol addresses a namespaced tool as the pair (namespace,
// name) while Chat has one flat namespace, so the pair has to be remembered
// rather than re-derived: a namespace of its own can contain the separator.
type namespacedTool struct {
	Namespace string
	Name      string
}

// chatToolName flattens a (namespace, name) pair into the single name a Chat
// vendor sees.  The spelling is the one Codex itself used for MCP tools before
// it grew namespaces (`mcp__server__tool`), so a model that has met those names
// reads this one the same way.
func chatToolName(namespace, name string) string {
	return strings.TrimSpace(namespace) + "__" + strings.TrimSpace(name)
}

// namespaceChildren returns the tools a namespace declaration carries, or nil
// when the declaration is an ordinary function.  A child with no name is not
// addressable in either protocol, so it is dropped rather than declared.
func (t responsesTool) namespaceChildren() []responsesTool {
	if !strings.EqualFold(strings.TrimSpace(t.Type), "namespace") {
		return nil
	}
	var children []responsesTool
	for _, child := range t.Tools {
		if strings.TrimSpace(child.Name) == "" {
			continue
		}
		children = append(children, child)
	}
	return children
}

// namespacedTools indexes every namespace child by its flattened name.  The
// first declaration of a name wins, which keeps the table — and the tool list
// built from it — stable for a caller that declared the same name twice.
func namespacedTools(tools []responsesTool) map[string]namespacedTool {
	var out map[string]namespacedTool
	for _, t := range tools {
		children := t.namespaceChildren()
		if len(children) == 0 {
			continue
		}
		namespace := strings.TrimSpace(t.Name)
		for _, child := range children {
			flat := chatToolName(namespace, child.Name)
			if out == nil {
				out = make(map[string]namespacedTool, len(children))
			}
			if _, ok := out[flat]; ok {
				continue
			}
			out[flat] = namespacedTool{Namespace: namespace, Name: strings.TrimSpace(child.Name)}
		}
	}
	return out
}

// namespacedCall splits a vendor-side tool name back into the pair Codex
// dispatches on.  A name that was not declared as a namespace child is passed
// through untouched, which is every ordinary function call.
func namespacedCall(names map[string]namespacedTool, name string) (string, string) {
	if ref, ok := names[strings.TrimSpace(name)]; ok {
		return ref.Name, ref.Namespace
	}
	return name, ""
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
func translateResponsesInput(raw json.RawMessage, verify *compactionSigner) ([]core.Message, error) {
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
		// callNames maps a function-call id back to the function name.  Chat's
		// tool result carries both fields; Responses carries the name only on
		// the call, so the name has to be recovered when the output arrives.
		callNames = map[string]string{}
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
			custom := !strings.EqualFold(it.Type, "function_call")
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
			callName := strings.TrimSpace(it.Name)
			if namespace := strings.TrimSpace(it.Namespace); namespace != "" && callName != "" {
				// Codex names the pair, the vendor names the flattening of it: the
				// history has to use the vendor's spelling or the replayed call no
				// longer matches the tool list the vendor answered.
				callName = chatToolName(namespace, callName)
			}
			if callID != "" && callName != "" {
				callNames[callID] = callName
			}
			// The reasoning that preceded this call belongs to this turn.  In
			// Chat it rides on the assistant message's reasoning_content.
			if pending.Reasoning == "" && pendingReasoning != "" {
				pending.Reasoning = pendingReasoning
				pendingReasoning = ""
			}
			// A custom tool call has no "arguments" object: its body is
			// freeform text in "input".  Wrap it so the replayed call still
			// carries well formed JSON, which every Chat vendor parses.
			arguments := rawToArgumentString(it.Arguments)
			if custom {
				arguments = customToolArguments(it.Input)
			}
			pending.ToolCalls = append(pending.ToolCalls, core.ToolCall{
				ID:        callID,
				Type:      "function",
				Name:      callName,
				Arguments: arguments,
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
			text, parts := parseResponsesToolOutput(it.Output)
			out = append(out, core.Message{
				Role:       "tool",
				ToolCallID: callID,
				Name:       callNames[callID],
				Content:    text,
				Parts:      parts,
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
			// A prior turn's context fold.  Three shapes reach here:
			//
			//   - a blob this gateway minted and signed: it verifies and is
			//     replayed as the assistant turn it stands for;
			//   - a plaintext summary smuggled through encrypted_content by some
			//     relay: preserved verbatim, exactly as before;
			//   - genuine ciphertext (OpenAI's own, another deployment's, a
			//     tampered blob): dropped, because this gateway holds no key for
			//     it and inventing text would be a fabrication.
			flushPending()
			text := strings.TrimSpace(it.EncryptedContent)
			if text == "" {
				continue
			}
			if summary, ok := verify.parse(text); ok {
				if summary != compactionNoSummaryMarker {
					out = append(out, core.Message{Role: "assistant", Content: summary})
				}
				continue
			}
			if looksLikeCompactionBlob(text) {
				// Our own wire shape, but the signature did not verify: a blob
				// from another deployment, a tampered one, or one whose key is
				// gone.  Dropping it is the only honest answer -- replaying it as
				// assistant prose would put a base64 string in the model's
				// context, and inventing a summary would be worse.
				continue
			}
			if !looksEncrypted(text) {
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
			if role == "assistant" {
				// Codex replays the assistant's visible text as its own item,
				// immediately before the function_call it introduced.  Chat
				// expresses that whole turn as ONE assistant message carrying
				// content, reasoning and tool_calls, so the text is held open
				// rather than emitted: a following call attaches to it, and a
				// following non-tool item flushes it unchanged.  Emitting the
				// text as its own turn instead splices two assistant turns
				// together and hands the model a history where a turn ends right
				// after a bare preamble -- the shape it then imitates by
				// stopping mid-task.
				if pending != nil {
					flushPending()
				}
				if pendingReasoning != "" {
					msg.Reasoning = pendingReasoning
					pendingReasoning = ""
				}
				pending = &msg
				continue
			}
			// Any other role closes the open items first so the order holds.
			flushPending()
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
//
// A missing or null field becomes "{}" rather than "": an empty string is not
// valid JSON and strict vendors reject the whole request over it, while an
// empty object is what a call with no arguments actually means.
func rawToArgumentString(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "{}"
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) == nil {
			return ensureJSONObject(s)
		}
	}
	return ensureJSONObject(string(trimmed))
}

// customToolArguments renders a custom tool call's freeform input as the JSON
// object Chat requires.  The text is preserved verbatim under "input" so the
// tool result that follows in the history still refers to something the model
// can read.
func customToolArguments(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "{}"
	}
	var s string
	if json.Unmarshal(trimmed, &s) == nil {
		if strings.TrimSpace(s) == "" {
			return "{}"
		}
		obj, err := json.Marshal(map[string]string{"input": s})
		if err == nil {
			return string(obj)
		}
	}
	// An object-shaped input is already the JSON this call wants.
	return ensureJSONObject(string(trimmed))
}

// ensureJSONObject guarantees the string put on the wire parses.  A value that
// is already valid is returned byte for byte; anything else is preserved under
// "_raw" instead of being dropped, so the replayed turn stays coherent.
func ensureJSONObject(s string) string {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return "{}"
	}
	if json.Valid([]byte(trimmed)) {
		if trimmed[0] == '{' || trimmed[0] == '[' {
			return trimmed
		}
		// A bare JSON scalar is legal but not the object shape a tool call
		// expects, so it is boxed the same way as a damaged value.
	}
	obj, err := json.Marshal(map[string]string{"_raw": s})
	if err != nil {
		return "{}"
	}
	return string(obj)
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
		s.recordUsage(rec)
	}()

	stat := newChatStat(rec.StartedAt, "", false)
	defer stat.done()

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var wire responsesRequest
	if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
		stat.status = http.StatusBadRequest
		s.fail(&rec, err)
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}
	rec.Model = wire.Model
	stat.model = wire.Model
	stat.mode = chatMode(wire.Stream)

	req, err := toCoreRequestFromResponses(&wire, s.compaction)
	if err != nil {
		stat.status = http.StatusBadRequest
		s.fail(&rec, err)
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	req.ConversationRequestID = strings.TrimSpace(r.Header.Get("X-Conversation-Request-ID"))
	rec.SessionID = sessionIDFor(req)
	rec.ReasoningEffort = reasoningEffortForResponses(&wire)

	// Automatic compaction.  When the caller set compact_threshold and the
	// assembled request is already at or over it, the history is folded before it
	// is routed, and the freshly minted compaction item is appended to whatever
	// this turn returns so the caller can carry it forward.
	//
	// The estimate is the same heuristic the context guard uses, so "threshold"
	// means the same thing on both paths.  A failure to summarise is not fatal: the
	// turn proceeds with the caller's own history, exactly as it would have
	// without context_management.
	var pendingCompaction *responseOutput
	if threshold := wire.compactionThreshold(); threshold > 0 {
		if estimateRequestTokens(req) >= threshold {
			if item := s.autoCompact(ctxForRequest(r), req, &rec); item != nil {
				pendingCompaction = item
			}
		}
	}
	// The context guard would otherwise trim this request, and a trimmed turn
	// looks like an ordinary success to the caller: it re-sends the same
	// oversized history next turn and gets trimmed again, forever.  Folding the
	// history instead turns that silent, repeating loss into a compaction item
	// the caller carries forward.  It costs one summarisation call, so it only
	// runs when the guard would really have cut something -- and if it cannot
	// summarise, the guard's own trim still applies and the turn still succeeds.
	if pendingCompaction == nil && s.responsesWouldTrim(ctxForRequest(r), req) {
		if item := s.autoCompact(ctxForRequest(r), req, &rec); item != nil {
			pendingCompaction = item
		}
	}

	s.routeAndServe(w, r, routeInput{
		requested:          wire.Model,
		req:                req,
		toolNames:          namespacedTools(wire.Tools),
		stream:             wire.Stream,
		modelForWire:       wire.Model,
		rec:                &rec,
		stat:               stat,
		emitter:            responsesEmitter{},
		trailingCompaction: pendingCompaction,
	})
}

// ctxForRequest derives a routing context from an inbound request, so a helper
// that runs before routeAndServe can still resolve a catalogue and honour the
// caller's cancellation.
func ctxForRequest(r *http.Request) context.Context {
	if r == nil {
		return context.Background()
	}
	return r.Context()
}

// responsesWouldTrim reports whether the context guard would shorten this
// request for the candidate that would serve it.
//
// It answers the same question prepareCandidateAttempt later asks, using the
// same promptBudget, so "the turn was trimmed" and "we folded instead" are
// decided from one number.  It is deliberately conservative: when the model
// cannot be resolved or its window is unknown, it says no and the guard's own
// behaviour is unchanged.
func (s *server) responsesWouldTrim(ctx context.Context, req *core.ChatRequest) bool {
	if req == nil || len(req.Messages) == 0 {
		return false
	}
	candidates, err := s.opts.Registry.ResolveCandidates(ctx, req.Model)
	if err != nil || len(candidates) == 0 {
		return false
	}
	client, model := candidates[0].Client, candidates[0].Model
	maxOutput := 0
	if req.MaxTokens != nil {
		maxOutput = *req.MaxTokens
	} else if n, ok := s.resolveMaxOutputTokens(ctx, client, model); ok {
		maxOutput = n
	}
	// A model this gateway has never tested gets probed first.  That costs one
	// round trip and teaches the real window: if the vendor serves it, the size
	// is proven servable; if it refuses, the refusal names the limit and the
	// retry path learns it.  Folding first would spend a summarisation call to
	// answer a question the vendor was about to answer for free -- and would
	// throw away the history the probe exists to test.
	//
	// Once the model is settled, compaction outranks trimming: folding keeps
	// the old turns in the summary the caller carries forward, while a trim
	// drops them for good.
	if s.probeEnabled() {
		switch s.probeDecision(ctx, req, client, model, maxOutput, req.ContextWindowLimit) {
		case probeForwardWhole:
			return false
		case probeProceed, probeTrimExceeded:
			// probeProceed only means "the normal budget logic owns this", and
			// that logic may still trim.  Fall through so the trim report below
			// decides whether folding can help.
		}
	}
	budget := s.probeAdjustedBudget(ctx, client, model, maxOutput, req.ContextWindowLimit)
	if budget <= 0 {
		return false
	}
	report, trimmed := applyContextBudget(cloneChatRequest(req), budget)
	if report.Impossible {
		// The newest turn alone is over the window.  Folding older history
		// cannot help, and the guard will refuse it with the code the caller
		// acts on; there is nothing for a compaction item to fix.
		return false
	}
	return trimmed
}

// Compaction must survive a transient in-flight refusal.  A platform configured
// with max_in_flight_per_account=1 reports ErrBusy whenever another request --
// including the user's own concurrent turn -- already holds the only slot.
// That is backpressure, not a verdict, so giving up on the first refusal would
// turn a momentary collision back into the silent trim this whole path exists
// to avoid.
const (
	compactionBusyAttempts = 3
	compactionBusyDelay    = 200 * time.Millisecond
)

func (s *server) compactConversationWaitingOutBackpressure(ctx context.Context, client core.Client, model string, req *core.ChatRequest, keepTurns int) (string, int, *core.Usage, error) {
	var lastErr error
	for attempt := 0; attempt < compactionBusyAttempts; attempt++ {
		summary, folded, used, err := compactConversation(ctx, s, client, model, req, keepTurns)
		if err == nil || !errors.Is(err, core.ErrBusy) {
			return summary, folded, used, err
		}
		lastErr = err
		if attempt+1 == compactionBusyAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return "", 0, nil, lastErr
		case <-time.After(compactionBusyDelay):
		}
	}
	return "", 0, nil, lastErr
}

// autoCompact folds req in place for the context_management path and returns the
// compaction item to append to this turn's output, or nil when nothing was
// folded.
func (s *server) autoCompact(ctx context.Context, req *core.ChatRequest, rec *UsageRecord) *responseOutput {
	if req == nil || len(req.Messages) == 0 {
		return nil
	}
	candidates, err := s.opts.Registry.ResolveCandidates(ctx, req.Model)
	if err != nil || len(candidates) == 0 {
		return nil
	}
	beforeTokens := int64(estimateRequestTokens(req))
	result := s.compactWithFailover(ctx, req, candidates)
	if !result.attempted {
		// Every candidate's own health surface already says nothing can serve.
		// The caller-visible turn is about to learn the same thing; spending a
		// summarisation round trip first would only add latency and noise.
		return nil
	}
	if result.err != nil {
		s.opts.Logger.Printf("compaction: auto-compaction for %s/%s failed (%v); forwarding the full history",
			result.client.Name(), result.model, result.err)
		return nil
	}
	summary, folded, usage := result.summary, result.folded, result.usage
	client, model := result.client, result.model
	if folded == 0 {
		return nil
	}
	if summary == "" {
		return nil
	}
	start, ok := compactionSplit(req.Messages, 1)
	if !ok {
		return nil
	}
	kept := make([]core.Message, 0, len(req.Messages)-start+1)
	for _, m := range req.Messages {
		if strings.EqualFold(strings.TrimSpace(m.Role), "system") {
			kept = append(kept, m)
		}
	}
	kept = append(kept, core.Message{Role: "assistant", Content: summary})
	kept = append(kept, req.Messages[start:]...)
	req.Messages = kept

	if rec != nil {
		// folded turns were replaced by the single summary message.
		rec.Trimmed = true
		rec.TrimDropped = folded - 1
		rec.TrimmedFromTokens = beforeTokens
		rec.TrimmedToTokens = int64(estimateRequestTokens(req))
	}
	if usage != nil && rec != nil && rec.PromptTokens == 0 && rec.CompletionTokens == 0 {
		// The summarisation call's tokens are real spend and belong on the row.
		// The caller-visible turn reports its own usage later and overwrites
		// this, so the two are never added together.
		rec.setUsage(usage)
	}
	blob, berr := s.mintCompactionBlob(summary, folded, req.Model)
	if berr != nil {
		s.opts.Logger.Printf("compaction: cannot mint a blob for %s/%s: %v", client.Name(), model, berr)
		return nil
	}
	return &responseOutput{
		Type:              "compaction",
		ID:                newID("cmp_"),
		Status:            "completed",
		CompactionContent: blob,
	}
}

// compactionResult is what one auto-compaction attempt produced.  attempted
// separates "no candidate was worth trying" from "a candidate was tried and
// failed": the caller stays silent for the first and logs a real reason for the
// second.
type compactionResult struct {
	summary   string
	folded    int
	usage     *core.Usage
	client    core.Client
	model     string
	err       error
	attempted bool
}

// compactWithFailover folds the older history using the first candidate that can
// actually serve it.
//
// Summarising through candidates[0] alone was a real production failure.  The
// caller-visible turn may fail over to a sibling -- workbuddy parks accounts per
// model, which its own Health() cannot express -- while the fold stayed pinned to
// the exhausted platform, failed, and degraded straight back into the silent trim
// the compaction path exists to prevent.  Walking the same candidate chain the
// request itself uses keeps the fold on a platform that can answer.
func (s *server) compactWithFailover(ctx context.Context, req *core.ChatRequest, candidates []core.Candidate) compactionResult {
	var (
		attempted bool
		last      compactionResult
	)
	for _, candidate := range candidates {
		c, m := candidate.Client, candidate.Model
		last.client, last.model = c, m
		if _, unavailable := moduleUnavailable(c); unavailable != nil {
			// The module itself says nothing can serve right now.  Entering
			// Chat would only repeat the empty rotation the turn is about to
			// hit, so this candidate is skipped without paying a round trip.
			continue
		}
		attempted = true
		summary, folded, usage, err := s.compactConversationWaitingOutBackpressure(ctx, c, m, req, 1)
		if err == nil {
			return compactionResult{
				summary:   summary,
				folded:    folded,
				usage:     usage,
				client:    c,
				model:     m,
				attempted: true,
			}
		}
		last.err = err
		if !compactionMayFailOver(err) {
			// A content-policy refusal or a malformed-request verdict would be
			// repeated verbatim by every sibling, so keep the real reason.
			return compactionResult{client: c, model: m, err: err, attempted: true}
		}
	}
	last.attempted = attempted
	return last
}

// compactionMayFailOver reports whether another candidate is worth trying for a
// failed summarisation.  Only verdicts about the platform or the account are:
// the folded request is unchanged, so a sibling may well serve it.
func compactionMayFailOver(err error) bool {
	if err == nil {
		return false
	}
	if candidateUnavailable(err) {
		return true
	}
	if f, ok := core.AsFailure(err); ok && core.Retryable(f.Kind) {
		return true
	}
	return false
}

// responsesCompactRequest is the inbound body of POST /v1/responses/compact.
//
// Only the fields the endpoint acts on are declared; the rest of the published
// parameter set (service_tier, prompt_cache_key, ...) is accepted by omission,
// which keeps a client that sends the full set working.
type responsesCompactRequest struct {
	Model               string          `json:"model"`
	Input               json.RawMessage `json:"input,omitempty"`
	Instructions        string          `json:"instructions,omitempty"`
	PreviousResponseID  string          `json:"previous_response_id,omitempty"`
	ConversationID      string          `json:"conversation_id,omitempty"`
	ConversationIDCamel string          `json:"conversationId,omitempty"`
	Metadata            json.RawMessage `json:"metadata,omitempty"`
}

// handleResponsesCompact answers POST /v1/responses/compact: it folds the
// supplied conversation into a handover summary and returns a
// response.compaction object whose output is the retained user turns followed by
// one compaction item.
//
// The summary is produced by the routed model through the normal chat pipeline,
// so account rotation and accounting behave exactly as they do for a
// user-visible request.  When summarisation cannot run at all (no candidate, an
// upstream refusal), the endpoint still answers with the deterministic retention
// list rather than a 500: a shorter honest history is a usable answer, and a
// fabricated summary is not.
func (s *server) handleResponsesCompact(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "use POST")
		return
	}
	s.stats.addRequest()

	rec := UsageRecord{At: time.Now(), StartedAt: time.Now()}
	defer func() {
		rec.At = time.Now()
		s.recordUsage(rec)
	}()

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var wire responsesCompactRequest
	if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
		s.fail(&rec, err)
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}
	rec.Model = wire.Model

	if strings.TrimSpace(wire.Model) == "" {
		err := fmt.Errorf("model is required")
		s.fail(&rec, err)
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	req, err := toCoreRequestFromResponses(&responsesRequest{
		Model:          wire.Model,
		Input:          wire.Input,
		Instructions:   wire.Instructions,
		ConversationID: firstNonEmptyValue(wire.ConversationID, wire.ConversationIDCamel),
		Metadata:       wire.Metadata,
	}, s.compaction)
	if err != nil {
		s.fail(&rec, err)
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	req.Model = wire.Model
	if !hasUserTurn(req.Messages) {
		err := fmt.Errorf("input must contain at least one user message")
		s.fail(&rec, err)
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	req.ConversationRequestID = strings.TrimSpace(r.Header.Get("X-Conversation-Request-ID"))
	rec.SessionID = sessionIDFor(req)

	if !s.exposedModelAllowed(r, wire.Model) {
		err := fmt.Errorf("model %q is not exposed by this gateway", wire.Model)
		rec.Failed = true
		s.fail(&rec, err)
		writeError(w, http.StatusNotFound, "invalid_request_error", err.Error())
		return
	}
	candidates, err := s.opts.Registry.ResolveCandidates(r.Context(), wire.Model)
	if err != nil {
		s.fail(&rec, err)
		writeError(w, http.StatusNotFound, "invalid_request_error", err.Error())
		return
	}
	if len(candidates) == 0 {
		err := fmt.Errorf("no candidate can serve %q", wire.Model)
		s.fail(&rec, err)
		writeError(w, http.StatusNotFound, "invalid_request_error", err.Error())
		return
	}

	client := candidates[0].Client
	req.Model = candidates[0].Model
	rec.Client = client.Name()
	rec.Model = client.Name() + "/" + candidates[0].Model

	out, usage, cerr := s.compactHistory(r.Context(), client, req, &rec)
	if cerr != nil {
		rec.Failed = true
		s.fail(&rec, cerr)
		s.writeUpstreamError(w, client, s.hintContextFunc(r.Context(), client, req.Model, req.Messages), cerr)
		return
	}
	rec.setUsage(usage)

	envelope := responseObject{
		ID:        newID("resp_"),
		Object:    "response.compaction",
		CreatedAt: time.Now().Unix(),
		Status:    "completed",
		Model:     wire.Model,
		Output:    out,
		Usage:     toResponseUsage(usage),
	}
	writeJSON(w, http.StatusOK, envelope)
}

// compactHistory folds req's older turns into one summary and renders the
// response.compaction output list.  It returns the rendered items and any usage
// the summariser reported.
func (s *server) compactHistory(ctx context.Context, client core.Client, req *core.ChatRequest, rec *UsageRecord) ([]responseOutput, *core.Usage, error) {
	summary, folded, usage, err := compactConversation(ctx, s, client, req.Model, req, 1)
	if err != nil {
		// The caller still needs a usable history.  Fall back to the
		// deterministic retention list -- the newest user turn(s) plus whatever
		// system prompt was supplied -- and report the refusal on the usage row.
		if rec != nil {
			rec.FailureReason = core.Redact(err.Error())
		}
		s.opts.Logger.Printf("compaction: %s/%s could not summarise (%v); returning the retained turns only", client.Name(), req.Model, err)
		summary, folded = "", 0
		usage = nil
	}

	var out []responseOutput
	// Instructions / system turns are preserved verbatim: they are standing
	// direction, not history to fold.
	start := 0
	for start < len(req.Messages) && strings.EqualFold(strings.TrimSpace(req.Messages[start].Role), "system") {
		out = append(out, messageItemFromCore("system-"+newID("msg_"), req.Messages[start]))
		start++
	}
	if summary != "" {
		out = append(out, responseOutput{
			Type:   "message",
			ID:     "msg_" + newID("sum_"),
			Status: "completed",
			Role:   "user",
			Content: []responseOutputContent{{
				Type:        "input_text",
				Text:        summary,
				Annotations: []any{},
			}},
		})
	}
	// The retained tail: the newest user turn and anything after it.
	if idx, ok := compactionSplit(req.Messages, 1); ok {
		for _, m := range req.Messages[idx:] {
			out = append(out, messageItemFromCore(newID("msg_"), m))
		}
	} else {
		for _, m := range req.Messages[start:] {
			out = append(out, messageItemFromCore(newID("msg_"), m))
		}
	}

	blob, berr := s.mintCompactionBlob(summary, folded, req.Model)
	if berr != nil {
		return nil, nil, berr
	}
	out = append(out, responseOutput{
		Type:              "compaction",
		ID:                newID("cmp_"),
		Status:            "completed",
		CompactionContent: blob,
	})
	return out, usage, nil
}

// mintCompactionBlob signs (or, when there is no summary, still mints a
// placeholder for) the compaction state.
func (s *server) mintCompactionBlob(summary string, folded int, model string) (string, error) {
	if s == nil || s.compaction == nil {
		return "", fmt.Errorf("compaction: no signer is configured")
	}
	// A deterministic retention list produces no summary text.  The blob then
	// carries that fact explicitly rather than an empty string, so a reader can
	// tell "this run kept the turns instead of summarising" apart from a
	// truncated blob.
	text := strings.TrimSpace(summary)
	if text == "" {
		text = compactionNoSummaryMarker
	}
	return s.compaction.mint(text, compactionPayload{SourceMessages: folded, Model: model})
}

// compactionNoSummaryMarker is what a blob carries when summarisation was
// unavailable and the endpoint returned the retained turns instead.  It is
// plain text on purpose: it is produced by this gateway, never by a vendor.
const compactionNoSummaryMarker = "(compaction produced no summary; the retained turns carry the state)"

// messageItemFromCore renders one core.Message as a Responses input item.
func messageItemFromCore(id string, m core.Message) responseOutput {
	text := m.Content
	if text == "" {
		var b strings.Builder
		for _, p := range m.Parts {
			if p.Type == "" || p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		text = b.String()
	}
	role := strings.ToLower(strings.TrimSpace(m.Role))
	if role == "" {
		role = "user"
	}
	kind := "input_text"
	if role == "assistant" {
		kind = "output_text"
	}
	return responseOutput{
		Type:   "message",
		ID:     id,
		Status: "completed",
		Role:   role,
		Content: []responseOutputContent{{
			Type:        kind,
			Text:        text,
			Annotations: []any{},
		}},
	}
}

// firstNonEmptyValue returns the first argument that is non-empty after
// trimming, or "".
func firstNonEmptyValue(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// hasUserTurn reports whether the conversation carries a user message with
// something in it.  An empty string input decodes to a single blank user turn,
// and "compact nothing" is a request error rather than a compaction.
func hasUserTurn(msgs []core.Message) bool {
	for _, m := range msgs {
		if !strings.EqualFold(strings.TrimSpace(m.Role), "user") {
			continue
		}
		if strings.TrimSpace(m.Content) != "" {
			return true
		}
		for _, p := range m.Parts {
			if strings.TrimSpace(p.Text) != "" || (p.Type != "" && p.Type != "text") {
				return true
			}
		}
	}
	return false
}

// parseResponsesToolOutput decodes the union shape of a Responses
// function_call_output.  A string is plain text; an array carries typed content
// parts.  input_text contributes text and input_image becomes an image_url part
// so a view_image screenshot reaches the vendor as an image instead of being
// flattened into JSON prose.  Objects and other scalar values remain readable
// JSON text, preserving the behaviour for ordinary structured tool output.
func parseResponsesToolOutput(raw json.RawMessage) (string, []core.ContentPart) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "", nil
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) == nil {
			return s, nil
		}
		return rawToText(raw), nil
	}
	if trimmed[0] != '[' {
		return rawToText(raw), nil
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &parts); err != nil {
		return rawToText(raw), nil
	}
	var (
		texts []string
		out   []core.ContentPart
	)
	for _, part := range parts {
		var kind string
		if err := json.Unmarshal(part["type"], &kind); err != nil && len(part["type"]) > 0 {
			continue
		}
		switch kind {
		case "input_text", "text", "output_text":
			var text string
			if err := json.Unmarshal(part["text"], &text); err != nil {
				continue
			}
			texts = append(texts, text)
		case "input_image", "image_url":
			var imageURL string
			if err := json.Unmarshal(part["image_url"], &imageURL); err != nil {
				// Responses normally carries image_url as a string, but some
				// relays wrap it as {"url": "..."}. Accept both shapes.
				var wrapped struct {
					URL string `json:"url"`
				}
				if err := json.Unmarshal(part["image_url"], &wrapped); err != nil {
					continue
				}
				imageURL = wrapped.URL
			}
			if strings.TrimSpace(imageURL) == "" {
				continue
			}
			var detail string
			if len(part["detail"]) > 0 {
				_ = json.Unmarshal(part["detail"], &detail)
			}
			out = append(out, core.ContentPart{Type: "image_url", ImageURL: imageURL, Detail: detail})
		}
	}
	if len(out) == 0 {
		return rawToText(raw), nil
	}
	// Content is the flattened text; Parts carries only non-text media.  This
	// matches the gateway's existing message contract, where a module that sees
	// both fields must not send the text twice.
	return strings.Join(texts, ""), out
}
