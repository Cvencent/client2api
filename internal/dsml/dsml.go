// Package dsml recognizes tool calls that a model writes as DSML/XML text
// instead of returning them through the provider's structured tool_calls field.
package dsml

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Call is one fully parsed tool call. Arguments is always a JSON object string.
type Call struct {
	Name      string
	Arguments string
}

const namePrefix = `(?:[｜|]*\s*(?:DSML|dsml|antml)\s*[｜|]*\s*:?\s*)?`

var (
	reOpenCalls  = regexp.MustCompile(`(?is)<\s*` + namePrefix + `(?:tool_calls|calls)\s*>`)
	reCloseCalls = regexp.MustCompile(`(?is)</\s*` + namePrefix + `(?:tool_calls|calls)\s*>`)
	reInvoke     = regexp.MustCompile(`(?is)<\s*` + namePrefix + `invoke([^>]*)>(.*?)</\s*` + namePrefix + `invoke\s*>`)
	reParameter  = regexp.MustCompile(`(?is)<\s*` + namePrefix + `parameter([^>]*)>(.*?)</\s*` + namePrefix + `parameter\s*>`)
	reAttrDouble = regexp.MustCompile(`(?i)\bname\s*=\s*"([^"]*)"`)
	reAttrSingle = regexp.MustCompile(`(?i)\bname\s*=\s*'([^']*)'`)
)

// HasMarker reports whether s contains a likely DSML/XML tool-call marker.
func HasMarker(s string) bool {
	return strings.Contains(s, "DSML") || strings.Contains(s, "tool_calls") || strings.Contains(s, "antml")
}

// Parser is a streaming parser. Feed it text deltas in order; it returns the
// visible text plus any tool calls that became complete. It is not safe for
// concurrent use.
type Parser struct {
	buf     strings.Builder
	inBlock bool
}

// NewParser returns an empty parser.
func NewParser() *Parser { return &Parser{} }

// Feed consumes one text delta.
func (p *Parser) Feed(delta string) (string, []Call) {
	if p == nil {
		return delta, nil
	}
	if delta != "" {
		p.buf.WriteString(delta)
	}
	var out strings.Builder
	var calls []Call

loop:
	for {
		s := p.buf.String()
		if s == "" {
			break
		}
		if p.inBlock {
			closeLoc := reCloseCalls.FindStringIndex(s)
			if closeLoc == nil {
				break
			}
			inner := s[:closeLoc[0]]
			p.replaceBuf(s[closeLoc[1]:])
			p.inBlock = false
			calls = append(calls, parseBlock(inner)...)
			continue
		}
		openLoc := reOpenCalls.FindStringIndex(s)
		invokeLoc := reInvoke.FindStringIndex(s)

		switch {
		case openLoc != nil && (invokeLoc == nil || openLoc[0] <= invokeLoc[0]):
			out.WriteString(s[:openLoc[0]])
			p.replaceBuf(s[openLoc[1]:])
			p.inBlock = true
		case invokeLoc != nil:
			out.WriteString(s[:invokeLoc[0]])
			block := s[invokeLoc[0]:invokeLoc[1]]
			p.replaceBuf(s[invokeLoc[1]:])
			calls = append(calls, parseBlock(block)...)
		default:
			keep := partialTagTail(s)
			if keep >= len(s) {
				break loop
			}
			out.WriteString(s[:len(s)-keep])
			p.replaceBuf(s[len(s)-keep:])
		}
	}
	return out.String(), calls
}

// Finish flushes whatever is left. An unterminated block is parsed when its
// complete invoke elements are present; leftover visible text is returned.
func (p *Parser) Finish() (string, []Call) {
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
		if calls := parseBlock(s); len(calls) > 0 {
			return "", calls
		}
		return "", nil
	}
	return s, nil
}

func (p *Parser) replaceBuf(s string) {
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

// parseBlock turns the body of a tool-call block into calls. It accepts the
// XML form and a JSON payload.
func parseBlock(inner string) []Call {
	var calls []Call
	for _, m := range reInvoke.FindAllStringSubmatch(inner, -1) {
		name := attrValue(m[1])
		if name == "" {
			continue
		}
		args := map[string]string{}
		argumentsJSON := ""
		for _, pm := range reParameter.FindAllStringSubmatch(m[2], -1) {
			pname := attrValue(pm[1])
			if pname == "" {
				continue
			}
			value := unescapeXMLText(strings.TrimSpace(pm[2]))
			if pname == "arguments" {
				argumentsJSON = normalizeArguments(value)
				continue
			}
			args[pname] = value
		}
		callArgs := marshalArgs(args)
		if argumentsJSON != "" {
			callArgs = argumentsJSON
		}
		calls = append(calls, Call{Name: name, Arguments: callArgs})
	}
	if len(calls) > 0 {
		return calls
	}
	return parseJSONBlock(inner)
}

// parseJSONBlock handles a JSON payload wrapped in a tool-call block, e.g.
// [{"name":"read_file","arguments":{"path":"/tmp/x"}}].
func parseJSONBlock(inner string) []Call {
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
	var out []Call
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
		out = append(out, Call{Name: name, Arguments: normalizeArguments(args)})
	}
	return out
}

// normalizeArguments renders an argument value as a JSON object string.
func normalizeArguments(v any) string {
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

// marshalArgs renders the name to value parameter map. encoding/json sorts map
// keys, so the result is deterministic.
func marshalArgs(args map[string]string) string {
	if len(args) == 0 {
		return "{}"
	}
	b, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func attrValue(attrs string) string {
	if m := reAttrDouble.FindStringSubmatch(attrs); m != nil {
		return strings.TrimSpace(m[1])
	}
	if m := reAttrSingle.FindStringSubmatch(attrs); m != nil {
		return strings.TrimSpace(m[1])
	}
	fields := strings.Fields(attrs)
	for _, f := range fields {
		if v, ok := strings.CutPrefix(f, "name="); ok {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

// unescapeXMLText reverses the five predefined XML entities. Numeric entities
// are intentionally left alone because tool arguments are code.
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
