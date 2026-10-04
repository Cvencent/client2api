package workbuddy

import (
	"encoding/json"
	"regexp"
	"strings"
)

// DSML tool-call parsing.
//
// WorkBuddy sometimes emits a tool call *inside* the assistant text stream
// rather than as a `delta.tool_calls` fragment: the model writes a DSML/XML
// block that names the tool and its arguments.  This file detects such blocks,
// removes them from the visible text, and turns them into ordinary tool calls
// so the gateway can re-emit them as `core.EventToolCall` fragments.
//
// Provenance: the reference Go implementation has no DSML parser at all (the
// Python monolith that had `parse_dsml_tool_calls` is not on this machine), so
// this parser was written from scratch.  Because the exact upstream spelling is
// not settled by the specification, it is deliberately tolerant: it accepts the
// full-width DSML form (`<｜DSML｜tool_calls>`), a plain XML form
// (`<tool_calls>`), and the `antml:`-prefixed form, with double- or
// single-quoted attributes, and it also accepts a JSON payload inside the
// wrapper.
//
// Recognised shape:
//
//	<｜DSML｜tool_calls>
//	  <｜DSML｜invoke name="read_file">
//	    <｜DSML｜parameter name="path">/tmp/x</｜DSML｜parameter>
//	  </｜DSML｜invoke>
//	</｜DSML｜tool_calls>
//
// Arguments become a JSON object of string values, which is the mapping the
// OpenAI tool-call surface expects.

// dsmlNamePrefix matches the optional markup prefix in front of a tag name.
const dsmlNamePrefix = `(?:[｜|]?\s*(?:DSML|dsml|antml)\s*[｜|]?\s*:?\s*)?`

var (
	reDSMLOpenToolCalls  = regexp.MustCompile(`(?is)<\s*` + dsmlNamePrefix + `tool_calls\s*>`)
	reDSMLCloseToolCalls = regexp.MustCompile(`(?is)</\s*` + dsmlNamePrefix + `tool_calls\s*>`)
	reDSMLInvoke         = regexp.MustCompile(`(?is)<\s*` + dsmlNamePrefix + `invoke([^>]*)>(.*?)</\s*` + dsmlNamePrefix + `invoke\s*>`)
	reDSMLParameter      = regexp.MustCompile(`(?is)<\s*` + dsmlNamePrefix + `parameter([^>]*)>(.*?)</\s*` + dsmlNamePrefix + `parameter\s*>`)
	reDSMLAttrDouble     = regexp.MustCompile(`(?i)\bname\s*=\s*"([^"]*)"`)
	reDSMLAttrSingle     = regexp.MustCompile(`(?i)\bname\s*=\s*'([^']*)'`)
	reDSMLBareOpen       = regexp.MustCompile(`(?is)<\s*` + dsmlNamePrefix + `tool_calls\s*>?`)
)

// dsmlCall is one fully parsed tool call.
type dsmlCall struct {
	Name      string
	Arguments string // JSON object, always an object (possibly "{}")
}

// HasDSMLMarker reports whether s contains something that looks like a DSML
// tool-call marker.  It is cheap and only used for diagnostics.
func HasDSMLMarker(s string) bool {
	return strings.Contains(s, "DSML") || strings.Contains(s, "tool_calls") || strings.Contains(s, "antml")
}

// DSMLParser is a streaming parser.  Feed it text deltas in order; it returns
// the text that should still be shown to the caller plus any tool calls that
// became complete during that delta.  It is not safe for concurrent use.
type DSMLParser struct {
	buf     strings.Builder
	inBlock bool
}

// NewDSMLParser returns an empty parser.
func NewDSMLParser() *DSMLParser { return &DSMLParser{} }

// Feed consumes one text delta.
func (p *DSMLParser) Feed(delta string) (string, []dsmlCall) {
	if p == nil {
		return delta, nil
	}
	if delta != "" {
		p.buf.WriteString(delta)
	}
	var out strings.Builder
	var calls []dsmlCall

loop:
	for {
		s := p.buf.String()
		if s == "" {
			break
		}
		if p.inBlock {
			cl := reDSMLCloseToolCalls.FindStringIndex(s)
			if cl == nil {
				break // wait for the rest of the block
			}
			inner := s[:cl[0]]
			p.replaceBuf(s[cl[1]:])
			p.inBlock = false
			calls = append(calls, parseDSMLBlock(inner)...)
			continue
		}
		openLoc := reDSMLOpenToolCalls.FindStringIndex(s)
		invokeLoc := reDSMLInvoke.FindStringIndex(s)

		switch {
		case openLoc != nil && (invokeLoc == nil || openLoc[0] <= invokeLoc[0]):
			out.WriteString(s[:openLoc[0]])
			p.replaceBuf(s[openLoc[1]:])
			p.inBlock = true
		case invokeLoc != nil:
			// A bare <invoke> without the <tool_calls> wrapper.
			out.WriteString(s[:invokeLoc[0]])
			block := s[invokeLoc[0]:invokeLoc[1]]
			p.replaceBuf(s[invokeLoc[1]:])
			calls = append(calls, parseDSMLBlock(block)...)
		default:
			keep := partialTagTail(s)
			if keep >= len(s) {
				// The whole buffer is a partial tag; wait for more input.
				// (This must leave the loop, not just the switch.)
				break loop
			}
			out.WriteString(s[:len(s)-keep])
			p.replaceBuf(s[len(s)-keep:])
		}
	}
	return out.String(), calls
}

// Finish flushes whatever is left.  An unterminated block is parsed anyway (the
// closing tag may simply have been lost); leftover text is returned so it is
// not silently swallowed.
func (p *DSMLParser) Finish() (string, []dsmlCall) {
	if p == nil {
		return "", nil
	}
	s := p.buf.String()
	p.buf.Reset()
	if s == "" {
		return "", nil
	}
	if p.inBlock {
		p.inBlock = false
		if calls := parseDSMLBlock(s); len(calls) > 0 {
			return "", calls
		}
		return "", nil
	}
	return s, nil
}

func (p *DSMLParser) replaceBuf(s string) {
	p.buf.Reset()
	if s != "" {
		p.buf.WriteString(s)
	}
}

// partialTagTail returns the length of a trailing fragment that could still be
// the beginning of a tag, so it is held back until more input arrives.
func partialTagTail(s string) int {
	i := strings.LastIndexByte(s, '<')
	if i < 0 {
		return 0
	}
	if strings.IndexByte(s[i:], '>') >= 0 {
		return 0
	}
	if len(s)-i > 64 {
		return 0
	}
	return len(s) - i
}

// parseDSMLBlock turns the body of a tool-call block into calls.  It accepts
// both the XML form and a JSON payload.
func parseDSMLBlock(inner string) []dsmlCall {
	var calls []dsmlCall
	for _, m := range reDSMLInvoke.FindAllStringSubmatch(inner, -1) {
		name := dsmlAttrValue(m[1])
		if name == "" {
			continue
		}
		args := map[string]string{}
		for _, pm := range reDSMLParameter.FindAllStringSubmatch(m[2], -1) {
			pname := dsmlAttrValue(pm[1])
			if pname == "" {
				continue
			}
			args[pname] = unescapeXMLText(strings.TrimSpace(pm[2]))
		}
		calls = append(calls, dsmlCall{Name: name, Arguments: marshalDSMLArgs(args)})
	}
	if len(calls) > 0 {
		return calls
	}
	return parseDSMLJSONBlock(inner)
}

// parseDSMLJSONBlock handles a JSON payload wrapped in a tool-call block, e.g.
// `[{"name":"read_file","arguments":{"path":"/tmp/x"}}]`.
func parseDSMLJSONBlock(inner string) []dsmlCall {
	trimmed := strings.TrimSpace(inner)
	if trimmed == "" {
		return nil
	}
	var raw []map[string]any
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		var single map[string]any
		if err := json.Unmarshal([]byte(trimmed), &single); err != nil {
			return nil
		}
		raw = []map[string]any{single}
	}
	var out []dsmlCall
	for _, item := range raw {
		name, _ := item["name"].(string)
		args := item["arguments"]
		if fn, ok := item["function"].(map[string]any); ok && fn != nil {
			if n, _ := fn["name"].(string); n != "" {
				name = n
			}
			if a, ok := fn["arguments"]; ok {
				args = a
			}
		}
		if name == "" {
			continue
		}
		out = append(out, dsmlCall{Name: name, Arguments: normalizeDSMLArguments(args)})
	}
	return out
}

// normalizeDSMLArguments renders an argument value as a JSON object string.
func normalizeDSMLArguments(v any) string {
	switch a := v.(type) {
	case nil:
		return "{}"
	case string:
		if strings.TrimSpace(a) == "" {
			return "{}"
		}
		var probe map[string]any
		if json.Unmarshal([]byte(a), &probe) == nil && probe != nil {
			return a
		}
		b, err := json.Marshal(map[string]string{"value": a})
		if err != nil {
			return "{}"
		}
		return string(b)
	default:
		b, err := json.Marshal(a)
		if err != nil {
			return "{}"
		}
		return string(b)
	}
}

// marshalDSMLArgs renders the name→value parameter map.  encoding/json sorts
// map keys, so the result is deterministic.
func marshalDSMLArgs(args map[string]string) string {
	if len(args) == 0 {
		return "{}"
	}
	b, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// dsmlAttrValue extracts a `name="…"` attribute from an attribute string.
func dsmlAttrValue(attrs string) string {
	if m := reDSMLAttrDouble.FindStringSubmatch(attrs); m != nil {
		return strings.TrimSpace(m[1])
	}
	if m := reDSMLAttrSingle.FindStringSubmatch(attrs); m != nil {
		return strings.TrimSpace(m[1])
	}
	// Bare `name=value` without quotes.
	fields := strings.Fields(attrs)
	for _, f := range fields {
		if v, ok := strings.CutPrefix(f, "name="); ok {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

// unescapeXMLText reverses the five predefined XML entities.  Numeric entities
// are left alone on purpose: tool arguments are code, and a wrong guess is
// worse than an untouched escape.
func unescapeXMLText(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	r := strings.NewReplacer(
		"&lt;", "<",
		"&gt;", ">",
		"&quot;", `"`,
		"&apos;", "'",
		"&amp;", "&",
	)
	return r.Replace(s)
}
