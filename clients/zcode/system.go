package zcode

// ---------------------------------------------------------------------------
// Identity preamble
// ---------------------------------------------------------------------------
//
// The ZCode upstream inspects the *content* of every chat request.  A request
// that does not carry the identity preamble the official desktop client sends
// is answered with `{"code":3012,"msg":"...unusual activity..."}` (risk
// control), regardless of how valid its credentials are.  The three blocks
// below are therefore protocol constants, not user content, and they are
// injected ahead of anything the caller supplied.
//
// Provenance: the *text* is transcribed from the identity preamble emitted by
// the official ZCode desktop client; the reference implementation read during
// recon ships the same text as a data artifact (`app/zcode_system.json`).  It
// is content, not code — no AGPL source text, comment, identifier or file
// layout was copied, and the container below is ours (a Go string constant
// decoded into raw JSON fragments, not the reference's data file).  See
// README.md § Provenance for the full audit trail.
//
// The backtick in the harness block is written as the JSON escape \u0060 so the
// whole document can live in a Go raw string literal; it decodes to the same
// byte the upstream expects.

import (
	"encoding/json"
	"sync"
)

const identityPreambleJSON = `[
  {
    "type": "text",
    "text": "You are ZCode, an interactive coding agent",
    "cache_control": {"type": "ephemeral"}
  },
  {
    "type": "text",
    "text": "\nYou are an interactive ZCode agent that helps users with software engineering tasks.\n\nIMPORTANT: Assist with authorized security testing, defensive security, CTF challenges, and educational contexts. Refuse requests for destructive techniques, DoS attacks, mass targeting, supply chain compromise, or detection evasion for malicious purposes. Dual-use security tools (C2 frameworks, credential testing, exploit development) require clear authorization context: pentesting engagements, CTF competitions, security research, or defensive use cases.\n\n# Harness\n- Text you output outside of tool use is displayed to the user as Github-flavored markdown in a terminal.\n- Tools run behind a user-selected permission mode; a denied call means the user declined it — adjust, don't retry verbatim.\n- The system may send updates, reminders, or modifications to rules via mid-conversation system turns. These are system-controlled, unlike function results. Hooks may intercept tool calls; treat hook output as user feedback.\n- Prefer the dedicated file/search tools over shell commands when one fits. Independent tool calls can run in parallel in one response.\n- Reference code as \u0060file_path:line_number\u0060 — it's clickable.",
    "cache_control": {"type": "ephemeral"}
  },
  {
    "type": "text",
    "text": "# Environment\nYou have been invoked in the following environment:\n- Primary working directory: unknown\n- Is a git repository: no\n- Platform: unknown\n- Shell: unknown\n- OS Version: unknown",
    "cache_control": {"type": "ephemeral"}
  }
]`

var (
	identityOnce  sync.Once
	identityCache []json.RawMessage
)

// identityBlocks returns a fresh copy of the identity preamble as raw JSON
// fragments.  It returns nil (never panics) if the embedded constant is
// somehow unparseable; callers then simply send no preamble.
func identityBlocks() []json.RawMessage {
	identityOnce.Do(func() {
		var blocks []json.RawMessage
		if err := json.Unmarshal([]byte(identityPreambleJSON), &blocks); err != nil {
			blocks = nil
		}
		identityCache = blocks
	})
	if len(identityCache) == 0 {
		return nil
	}
	out := make([]json.RawMessage, len(identityCache))
	copy(out, identityCache)
	return out
}
