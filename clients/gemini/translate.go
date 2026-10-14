package gemini

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// envelope construction
//
// The envelope is the whole reason this module keeps TWO serialisation
// concerns apart: the structure (maps, so Go sorts the keys) and the parts
// (typed structs, for readability where order is not observable).  Anything
// that reaches the wire is a map.
// ---------------------------------------------------------------------------

// geminiPart is one element of a content turn.  Field names match the upstream
// verbatim.
type geminiPart struct {
	Text             string              `json:"text,omitempty"`
	Thought          bool                `json:"thought,omitempty"`
	ThoughtSignature string              `json:"thoughtSignature,omitempty"`
	FunctionCall     *geminiFunctionCall `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResp `json:"functionResponse,omitempty"`
	InlineData       *geminiInlineData   `json:"inlineData,omitempty"`
}

type geminiFunctionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

type geminiFunctionResp struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type geminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

// buildEnvelope assembles the request body.  It returns the envelope as a
// nested map so encoding/json sorts every layer alphabetically, which is what
// the vendor's fingerprint expects to see byte-for-byte.
func buildEnvelope(project, model string, req *core.ChatRequest, sig *sigStore, lane string, generation int) (map[string]any, error) {
	contents, system, err := translateContents(req, sig)
	if err != nil {
		return nil, err
	}
	if len(contents) == 0 {
		return nil, errEmptyContents
	}

	tier := effortToTier(requestEffort(req))
	spec := map[string]any{
		"model":     upstreamModelName(tier),
		"project":   project,
		"request":   buildRequestMap(contents, system, req, project, lane, tier, generation),
		"requestId": newRequestID(),
		"userAgent": upstreamUserAgent,
	}
	// `model` already carries the tier; the caller's own model string is only
	// used to pick the tier, never forwarded.
	_ = model
	return spec, nil
}

// buildRequestMap is the inner `request` object.
func buildRequestMap(contents []geminiContent, system string, req *core.ChatRequest, project, lane, tier string, generation int) map[string]any {
	request := map[string]any{
		"contents":         contents,
		"generationConfig": buildGenerationConfig(req, tier),
		"sessionId":        deriveSessionID(project, firstUserText(contents), lane, generation),
	}
	if system != "" {
		request["systemInstruction"] = map[string]any{
			"role":  "system",
			"parts": []map[string]any{{"text": system}},
		}
	}
	if len(req.Tools) > 0 {
		decls := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			decl := map[string]any{"name": t.Name}
			if t.Description != "" {
				decl["description"] = t.Description
			}
			decl["parameters"] = sanitizeSchema(rawToMap(t.Parameters))
			decls = append(decls, decl)
		}
		request["tools"] = []map[string]any{{"functionDeclarations": decls}}
		if tc := buildToolConfig(req.ToolChoice); tc != nil {
			request["toolConfig"] = tc
		}
	}
	return request
}

// buildGenerationConfig builds the `generationConfig` object.
func buildGenerationConfig(req *core.ChatRequest, tier string) map[string]any {
	maxTokens := maxOutputTokens
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		maxTokens = *req.MaxTokens
	}
	thinking := map[string]any{"includeThoughts": true}
	if b := thinkingBudget(tier); b >= 0 {
		thinking["thinkingBudget"] = b
	}
	config := map[string]any{
		"maxOutputTokens": maxTokens,
		"thinkingConfig":  thinking,
	}
	if req.Temperature != nil {
		config["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		config["topP"] = *req.TopP
	}
	return config
}

// buildToolConfig maps the OpenAI-shaped tool_choice onto the upstream
// `toolConfig`.  A nil return means "leave the choice to the model".
func buildToolConfig(raw json.RawMessage) map[string]any {
	mode := "AUTO"
	var name string
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 {
		switch trimmed[0] {
		case '"':
			var s string
			if json.Unmarshal(trimmed, &s) == nil {
				switch s {
				case "none":
					mode = "NONE"
				case "any", "required":
					mode = "ANY"
				}
			}
		case '{':
			var obj struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
				Name string `json:"name"`
			}
			if json.Unmarshal(trimmed, &obj) == nil {
				switch obj.Type {
				case "none":
					mode = "NONE"
				case "any", "tool", "required":
					mode = "ANY"
				}
				name = firstNonEmpty(obj.Function.Name, obj.Name)
			}
		}
	}
	if mode == "AUTO" {
		// The upstream accepts an explicit AUTO config; sending it keeps the
		// generated bytes closer to the reference's always-present toolConfig.
		return map[string]any{"functionCallingConfig": map[string]any{"mode": "AUTO"}}
	}
	cfg := map[string]any{"mode": mode}
	if mode == "ANY" && name != "" {
		cfg["allowedFunctionNames"] = []string{name}
	}
	return map[string]any{"functionCallingConfig": cfg}
}

// translateContents turns the core messages into upstream `contents`, plus the
// flattened system prompt.
//
// Roles are only user / model.  A tool result is a functionResponse part keyed
// by the tool NAME (the upstream pairs on name, not id), so the id→name map is
// built from every assistant tool call first.
func translateContents(req *core.ChatRequest, sig *sigStore) ([]geminiContent, string, error) {
	if req == nil {
		return nil, "", nil
	}
	names := map[string]string{}
	for _, m := range req.Messages {
		for _, tc := range m.ToolCalls {
			if tc.ID != "" && tc.Name != "" {
				names[tc.ID] = tc.Name
			}
		}
	}

	var (
		contents []geminiContent
		system   strings.Builder
	)
	for _, m := range req.Messages {
		switch strings.ToLower(strings.TrimSpace(m.Role)) {
		case "system", "developer":
			if m.Content != "" {
				if system.Len() > 0 {
					system.WriteString("\n")
				}
				system.WriteString(m.Content)
			}
			continue
		}

		role := "user"
		if strings.EqualFold(m.Role, "assistant") {
			role = "model"
		}

		parts, err := translateMessageParts(m, role, names, sig)
		if err != nil {
			return nil, "", err
		}
		if len(parts) == 0 {
			continue
		}
		contents = append(contents, geminiContent{Role: role, Parts: parts})
	}
	return contents, system.String(), nil
}

// translateMessageParts builds the parts for one message.
func translateMessageParts(m core.Message, role string, names map[string]string, sig *sigStore) ([]geminiPart, error) {
	var parts []geminiPart

	// A tool result is its own turn whose part is a functionResponse.
	if strings.EqualFold(m.Role, "tool") {
		name := firstNonEmpty(names[m.ToolCallID], m.Name)
		if name == "" {
			return nil, nil // unpaired result: drop it rather than 400
		}
		response := map[string]any{"content": m.Content}
		parts = append(parts, geminiPart{FunctionResponse: &geminiFunctionResp{Name: name, Response: response}})
		return parts, nil
	}

	if m.Content != "" {
		parts = append(parts, geminiPart{Text: m.Content})
	}
	for _, p := range m.Parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				parts = append(parts, geminiPart{Text: p.Text})
			}
		case "image_url":
			inline, err := imageFromURL(p.ImageURL)
			if err != nil {
				return nil, err
			}
			parts = append(parts, geminiPart{InlineData: inline})
		}
	}
	for _, tc := range m.ToolCalls {
		args := parseArgsObject(tc.Arguments)
		call := &geminiFunctionCall{Name: tc.Name, Args: args}
		part := geminiPart{FunctionCall: call}
		if sig != nil {
			if s := sig.Get(tc.Name, canonicalArgs(args)); s != "" {
				part.ThoughtSignature = s
			}
		}
		parts = append(parts, part)
	}
	return parts, nil
}

// imageFromURL decodes a data: URL into inline bytes.  A remote http(s) image
// cannot be inlined without a fetch, and silently dropping it would let the
// caller believe the model saw it, so it is refused.
func imageFromURL(raw string) (*geminiInlineData, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if strings.HasPrefix(raw, "data:") {
		rest := raw[len("data:"):]
		comma := strings.IndexByte(rest, ',')
		if comma < 0 {
			return nil, errBadImage
		}
		meta := rest[:comma]
		data := rest[comma+1:]
		if !strings.Contains(meta, "base64") {
			return nil, errBadImage
		}
		mime := strings.TrimSpace(strings.SplitN(meta, ";", 2)[0])
		if mime == "" {
			mime = "image/png"
		}
		if cleaned := normalizeBase64(data); cleaned != "" {
			data = cleaned
		}
		return &geminiInlineData{MimeType: mime, Data: data}, nil
	}
	return nil, errRemoteImage
}

// canonicalArgs is the compact JSON with keys sorted, used as the signature
// cache key.  Both the storing and the looking-up side must agree on it or the
// lookup never hits.
func canonicalArgs(args map[string]any) string {
	if args == nil {
		return "{}"
	}
	return string(mustSortedJSON(args))
}

// parseArgsObject parses a tool call's argument string into a map, falling back
// to an empty object rather than inventing arguments.
func parseArgsObject(raw string) map[string]any {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil || out == nil {
		return map[string]any{}
	}
	return out
}

// firstUserText is the session-key input: only the FIRST user turn, and only
// its text parts.
func firstUserText(contents []geminiContent) string {
	for _, c := range contents {
		if c.Role != "" && c.Role != "user" {
			continue
		}
		for _, p := range c.Parts {
			if p.Text != "" {
				return p.Text
			}
		}
	}
	return ""
}

// deriveSessionID is a deterministic function of (project, first user text,
// lane).  It is deliberately FNV-1a (the reference's own hash could not be
// recovered), so the value differs from the reference's, but the DEPENDENCIES
// match: same input, same output; a different conversation, a different id.
func deriveSessionID(project, firstUser, lane string, generation int) string {
	const (
		offset64 = 0xcbf29ce484222325
		prime64  = 0x100000001b3
	)
	h := uint64(offset64)
	base := project + "\x00" + lane + "\x00" + firstUser
	if generation > 0 {
		base += "\x00" + strconv.Itoa(generation)
	}
	for i := 0; i < len(base); i++ {
		h ^= uint64(base[i])
		h *= prime64
	}
	return strconv.FormatInt(int64(h), 10)
}

// newRequestID mints the per-request id the reference sends.
func newRequestID(now ...time.Time) string {
	t := time.Now()
	if len(now) > 0 {
		t = now[0]
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "agent/" + strconv.FormatInt(t.UnixMilli(), 10) + "/" + hex.EncodeToString(b[:])
}

// requestEffort reads the caller's reasoning effort out of the request options.
func requestEffort(req *core.ChatRequest) string {
	if req == nil || req.Options == nil {
		return ""
	}
	if s, ok := req.Options["reasoning_effort"].(string); ok {
		return s
	}
	return ""
}

// mustSortedJSON marshals a value; a marshal error cannot happen for the plain
// maps this module builds, so it falls back to "{}".
func mustSortedJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// rawToMap decodes a JSON object, returning nil for anything that is not one.
func rawToMap(raw json.RawMessage) map[string]any {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	var out map[string]any
	if json.Unmarshal(trimmed, &out) != nil {
		return nil
	}
	return out
}

// --- schema sanitisation ----------------------------------------------------

// schemaKeys is the whitelist Gemini's FunctionDeclaration.parameters accepts.
// Anything outside it is a hard 400 upstream, and harness tool schemas
// routinely carry $schema / additionalProperties / default.
var schemaKeys = map[string]bool{
	"type": true, "format": true, "description": true, "nullable": true,
	"enum": true, "items": true, "minItems": true, "maxItems": true,
	"properties": true, "required": true, "minProperties": true,
	"maxProperties": true, "minLength": true, "maxLength": true,
	"pattern": true, "anyOf": true, "propertyOrdering": true,
	"minimum": true, "maximum": true,
}

// sanitizeSchema strips everything outside the whitelist, recurses into
// properties/items/anyOf, collapses a union type into a single type plus
// nullable, and drops any enum that is not all strings.
func sanitizeSchema(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{"type": "object"}
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		if !schemaKeys[key] {
			continue
		}
		switch key {
		case "properties":
			src, ok := value.(map[string]any)
			if !ok {
				continue
			}
			props := make(map[string]any, len(src))
			for name, child := range src {
				cm, ok := child.(map[string]any)
				if !ok {
					continue
				}
				props[name] = sanitizeSchema(cm)
			}
			out["properties"] = props
		case "items":
			switch v := value.(type) {
			case map[string]any:
				out["items"] = sanitizeSchema(v)
			case []any:
				items := make([]any, 0, len(v))
				for _, item := range v {
					if im, ok := item.(map[string]any); ok {
						items = append(items, sanitizeSchema(im))
					}
				}
				out["items"] = items
			}
		case "anyOf":
			list, ok := value.([]any)
			if !ok {
				continue
			}
			outList := make([]any, 0, len(list))
			for _, item := range list {
				if im, ok := item.(map[string]any); ok {
					outList = append(outList, sanitizeSchema(im))
				}
			}
			out["anyOf"] = outList
		case "type":
			switch v := value.(type) {
			case string:
				out["type"] = v
			case []any:
				var nonNull []string
				sawNull := false
				for _, item := range v {
					s, ok := item.(string)
					if !ok {
						continue
					}
					if s == "null" {
						sawNull = true
						continue
					}
					nonNull = append(nonNull, s)
				}
				if len(nonNull) > 0 {
					out["type"] = nonNull[0]
				}
				if sawNull {
					out["nullable"] = true
				}
			}
		case "enum":
			list, ok := value.([]any)
			if !ok {
				continue
			}
			allStrings := true
			for _, item := range list {
				if _, ok := item.(string); !ok {
					allStrings = false
					break
				}
			}
			if allStrings {
				out["enum"] = list
			}
		default:
			out[key] = value
		}
	}
	// A schema with no type at all is still valid to the vendor only as an
	// object; the reference relies on object-shaped parameters.
	if _, ok := out["type"]; !ok {
		if _, hasProps := out["properties"]; hasProps {
			out["type"] = "object"
		}
	}
	return out
}

// normalizeBase64 loosely cleans and validates a base64 blob.
func normalizeBase64(data string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', ' ', '\t':
			return -1
		}
		return r
	}, data)
	if cleaned == "" || len(cleaned)%4 != 0 {
		return ""
	}
	for _, r := range cleaned {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '+' || r == '/' || r == '=') {
			return ""
		}
	}
	return cleaned
}

// sortKeys is used by tests to prove the serialisation is alphabetical.
func sortKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
