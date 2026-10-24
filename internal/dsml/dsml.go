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

// namePrefix matches the optional marker that an upstream leaves in front of an
// element name.  Two spellings occur in real upstream text:
//
//	<DSML｜｜: calls>  labelled, with bars around the label
//	<｜｜ calls>       bars only -- the vendor already stripped the label
//
// The bars-only spelling is restricted to the fullwidth bar: a bare ascii pipe
// run before an element name is far more likely to be prose than an envelope.
// The run is matched as one-or-more rather than as an exact count because an
// upstream may open with two bars and close with three.
//
// The label used to be mandatory, so a markerless envelope fell through to the
// caller as visible text and the turn ended with no tool call at all.
const namePrefix = `(?:[｜]+\s*|[｜|]*\s*(?:DSML|dsml|antml)\s*[｜|]*\s*:?\s*)?`

var (
	reOpenCalls  = regexp.MustCompile(`(?is)<\s*` + namePrefix + `(?:tool_calls|calls)\s*>`)
	reCloseCalls = regexp.MustCompile(`(?is)</\s*` + namePrefix + `(?:tool_calls|calls)\s*>`)
	reInvoke     = regexp.MustCompile(`(?is)<\s*` + namePrefix + `invoke([^>]*)>(.*?)</\s*` + namePrefix + `invoke\s*>`)
	// A leaked envelope may close a parameter with the bare bar tag
	// (`</｜｜｜>`) instead of naming it, so accept either close.
	// Otherwise the value runs on into the next parameter.
	reParameter  = regexp.MustCompile(`(?is)<\s*` + namePrefix + `parameter([^>]*)>(.*?)(?:</\s*` + namePrefix + `parameter\s*>|</\s*[｜]+\s*>)`)
	reAttrDouble = regexp.MustCompile(`(?i)\bname\s*=\s*"([^"]*)"`)
	reAttrSingle = regexp.MustCompile(`(?i)\bname\s*=\s*'([^']*)'`)
	// The vendor sometimes writes the element's own name as a bare attribute
	// with no key at all -- `<｜｜ parameter="cmd" string="true">` --
	// which leaves `="cmd"` at the head of the attribute string.
	reBareAttrDouble = regexp.MustCompile(`(?:^|\s)="([^"]*)"`)
	reBareAttrSingle = regexp.MustCompile(`(?:^|\s)='([^']*)'`)
	// reBareInvokeOpen recognises the start of a fullwidth-bar invoke element.
	// It holds a truncated envelope back instead of leaking it as prose, so
	// unlike reInvoke it insists on the bar marker: with no close tag to confirm
	// the shape, accepting a markerless `<invoke ...>` would let ordinary prose
	// be swallowed as a call.
	reBareInvokeOpen = regexp.MustCompile(`(?is)<\s*[｜]+\s*invoke([^>]*)>`)
	// reMarkerless finds the bars-only envelope, which is distinguishable only
	// by a fullwidth bar run glued to an element or attribute name.
	reMarkerless = regexp.MustCompile(`[｜]{1,}\s*(?:tool_calls|calls|invoke|parameter)\b`)
)

// HasMarker reports whether s contains a likely DSML/XML tool-call marker.
// The labelled spellings are substring tests; the bars-only envelope needs the
// pattern, because a vendor that stripped the label leaves nothing else behind.
func HasMarker(s string) bool {
	return strings.Contains(s, "DSML") || strings.Contains(s, "tool_calls") || strings.Contains(s, "antml") || reMarkerless.MatchString(s)
}

// Parser is a streaming parser. Feed it text deltas in order; it returns the
// visible text plus any tool calls that became complete. It is not safe for
// concurrent use.
type Parser struct {
	buf       strings.Builder
	inBlock   bool
	malformed bool

	// Some upstreams write a call as the tool's own element name --
	// `<exec_command>` with `parameter` children -- instead of
	// `<invoke name="exec_command">`.  That spelling can only be told
	// apart from ordinary markup by checking the name against the tools the
	// caller actually offered, so these stay nil unless a tool list is given.
	named     *regexp.Regexp
	namedOpen *regexp.Regexp
}

// NewParser returns an empty parser. It recognises the labelled and
// fullwidth-bar DSML envelopes but not the named-element spelling, which
// needs the caller's tool list.
func NewParser() *Parser { return &Parser{} }

// NewParserWithTools returns a parser that also recognises a call written as
// the tool's own element name (`<exec_command>` with `parameter` children).
// The names come from the request's tool list; without them any element name
// would look like a call.
func NewParserWithTools(names []string) *Parser {
	p := &Parser{}
	p.named, p.namedOpen = namedToolRegexps(names)
	return p
}

// namedToolRegexps builds the two patterns used for the named-element
// spelling. RE2 has no backreferences, so the close tag is matched with the
// same alternation and the two captured names are compared at match time.
func namedToolRegexps(names []string) (*regexp.Regexp, *regexp.Regexp) {
	seen := make(map[string]bool, len(names))
	alts := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		alts = append(alts, regexp.QuoteMeta(name))
	}
	if len(alts) == 0 {
		return nil, nil
	}
	alt := strings.Join(alts, "|")
	call := regexp.MustCompile(`(?is)<\s*(` + alt + `)\s*>(.*?)</\s*(` + alt + `)\s*>`)
	open := regexp.MustCompile(`(?is)<\s*(` + alt + `)\s*>`)
	return call, open
}

// Malformed reports whether a recognized tool-call construct closed or ended
// without yielding a usable call. Callers can fail the turn instead of showing
// protocol markup as ordinary assistant text.
func (p *Parser) Malformed() bool {
	return p != nil && p.malformed
}

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
			parsed := parseBlock(inner)
			if len(parsed) == 0 {
				p.malformed = true
				// A block that closed but yielded no call would otherwise vanish.
				// That turns "the model tried to call a tool and the gateway could
				// not read it" into a clean-looking stop, so the text is handed back
				// the same way Finish already hands back an unterminated block.
				out.WriteString(inner)
			}
			calls = append(calls, parsed...)
			continue
		}
		openLoc := reOpenCalls.FindStringIndex(s)
		invokeLoc := reInvoke.FindStringIndex(s)
		bareLoc := reBareInvokeOpen.FindStringIndex(s)
		var namedLoc, namedOpenLoc []int
		if p.named != nil {
			namedLoc = p.named.FindStringIndex(s)
		}
		if p.namedOpen != nil {
			namedOpenLoc = p.namedOpen.FindStringIndex(s)
		}

		switch {
		case bareLoc != nil && (openLoc == nil || bareLoc[0] < openLoc[0]) && (invokeLoc == nil || bareLoc[0] < invokeLoc[0]):
			// A bars invoke with no matching close is a truncated envelope.  Hold it
			// back so Finish can still parse the calls it does contain; leaking it
			// as prose is what ended the turn with no tool call.
			out.WriteString(s[:bareLoc[0]])
			p.replaceBuf(s[bareLoc[0]:])
			p.inBlock = true
		case openLoc != nil && (invokeLoc == nil || openLoc[0] <= invokeLoc[0]):
			out.WriteString(s[:openLoc[0]])
			p.replaceBuf(s[openLoc[1]:])
			p.inBlock = true
		case invokeLoc != nil:
			out.WriteString(s[:invokeLoc[0]])
			block := s[invokeLoc[0]:invokeLoc[1]]
			p.replaceBuf(s[invokeLoc[1]:])
			parsed := parseBlock(block)
			if len(parsed) == 0 {
				p.malformed = true
				// A nameless invoke is the other silent-drop path.
				out.WriteString(block)
			}
			calls = append(calls, parsed...)
		case namedLoc != nil:
			out.WriteString(s[:namedLoc[0]])
			block := s[namedLoc[0]:namedLoc[1]]
			p.replaceBuf(s[namedLoc[1]:])
			parsed := parseNamedCalls(p.named, block)
			if len(parsed) == 0 {
				p.malformed = true
				out.WriteString(block)
			}
			calls = append(calls, parsed...)
		case namedOpenLoc != nil:
			// An opening tool element with no close yet: hold it back so a
			// truncated call is recovered at Finish instead of leaking as prose.
			out.WriteString(s[:namedOpenLoc[0]])
			p.replaceBuf(s[namedOpenLoc[0]:])
			p.inBlock = true
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

// Finish flushes whatever is left. A block that never closed still carries
// complete `parameter` elements, so it is parsed with the same rules as a
// closed one; only when that yields nothing is the text handed back so the
// caller can surface the truncation instead of a clean-looking stop.
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
		// A complete `invoke` inside a wrapper that never closed is still a
		// usable call; that is the ordinary truncated-wrapper case.
		if calls := parseBlock(s); len(calls) > 0 {
			return "", calls
		}
		// Otherwise the invoke itself was cut off, so there is no `</invoke>`
		// to match and only the parameters it did send can be recovered.
		if calls := parseTruncatedBlock(s); len(calls) > 0 {
			return "", calls
		}
		if calls := parseNamedCalls(p.named, s); len(calls) > 0 {
			return "", calls
		}
		if calls := parseTruncatedNamedCalls(p.namedOpen, s); len(calls) > 0 {
			return "", calls
		}
		// Nothing usable: fail the turn rather than hand back a silent stop.
		p.malformed = true
		return s, nil
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
		calls = append(calls, Call{Name: name, Arguments: parseParameters(m[2])})
	}
	if len(calls) > 0 {
		return calls
	}
	return parseJSONBlock(inner)
}

// parseTruncatedBlock reads an envelope the upstream cut off mid-flight: the
// `invoke` element opened but never closed, so there is no `</invoke>` for
// parseBlock to match. The parameters before the cut are complete, and they
// are exactly what the call needs, so the element is parsed as if it had
// closed. Only a bars-marked invoke is accepted here: at end of stream the
// closing tag can no longer disambiguate, and a markerless `<invoke ...>` in
// ordinary prose must not become a call.
func parseTruncatedBlock(inner string) []Call {
	var calls []Call
	for _, m := range reBareInvokeOpen.FindAllStringSubmatchIndex(inner, -1) {
		attrs := inner[m[2]:m[3]]
		name := attrValue(attrs)
		if name == "" {
			continue
		}
		// Everything up to the next opening invoke (or end of stream) is this
		// call's body.
		body := inner[m[1]:]
		if next := reBareInvokeOpen.FindStringIndex(body); next != nil {
			body = body[:next[0]]
		}
		// A cut-off call with no parameter at all is not something to run:
		// there is no evidence of what it was meant to do.
		if len(reParameter.FindAllStringSubmatch(body, -1)) == 0 {
			continue
		}
		calls = append(calls, Call{Name: name, Arguments: parseParameters(body)})
	}
	return calls
}

// parseNamedCalls turns `<tool>...</tool>` blocks into calls. The closing
// name must equal the opening one: RE2 has no backreferences, so the match
// is checked here rather than in the pattern.
func parseNamedCalls(re *regexp.Regexp, text string) []Call {
	if re == nil {
		return nil
	}
	var calls []Call
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		if m[1] != m[3] {
			continue
		}
		calls = append(calls, Call{Name: m[1], Arguments: parseParameters(m[2])})
	}
	return calls
}

// parseTruncatedNamedCalls reads a `<tool>` element the upstream cut off
// before its close tag. As with the fullwidth-bar envelope, only the
// parameters that actually completed are used.
func parseTruncatedNamedCalls(re *regexp.Regexp, text string) []Call {
	if re == nil {
		return nil
	}
	var calls []Call
	for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
		name := text[m[2]:m[3]]
		body := text[m[1]:]
		if next := re.FindStringIndex(body); next != nil {
			body = body[:next[0]]
		}
		if len(reParameter.FindAllStringSubmatch(body, -1)) == 0 {
			continue
		}
		calls = append(calls, Call{Name: name, Arguments: parseParameters(body)})
	}
	return calls
}

// parseParameters marshals the `parameter` elements of one invoke body into
// the JSON object the call carries. A parameter named `arguments` is the
// whole-object form and wins over the named ones.
func parseParameters(body string) string {
	args := map[string]string{}
	argumentsJSON := ""
	for _, pm := range reParameter.FindAllStringSubmatch(body, -1) {
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
	if argumentsJSON != "" {
		return argumentsJSON
	}
	return marshalArgs(args)
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
	// The bare-attribute spelling leaves the name at the head of the
	// attribute string with no key, so it is read before the generic scan.
	if m := reBareAttrDouble.FindStringSubmatch(attrs); m != nil {
		return strings.TrimSpace(m[1])
	}
	if m := reBareAttrSingle.FindStringSubmatch(attrs); m != nil {
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
