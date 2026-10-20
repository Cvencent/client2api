# Responses Compaction + OpenCode Context-Refusal Classification - Design

**Date:** 2026-10-20
**Status:** Approved for implementation

## Goal

Two related defects, one delivery:

1. **OpenCode (and every module wording it the same way) does not signal
   "compact" when it refuses an oversized prompt.** The vendor says
   `... more tokens than this model's maximum context length allows ... run
   compaction`, the module classifies it as `FailureOther`, and the gateway
   answers `error.code = invalid_request`. Codex therefore never triggers
   compaction and re-sends the same oversized body forever.
2. **The gateway has no Responses compaction protocol at all.** A client that
   does call `POST /v1/responses/compact` (or sends
   `context_management: [{type:"compaction",compact_threshold:N}]`) gets a 404
   or has the field silently dropped.

The fix makes the gateway (a) classify context-size refusals as
`context_window_exceeded` so the caller compacts, and (b) implement the
Responses compaction surface with a gateway-owned, self-verifying compaction
blob that this gateway can replay.

## Background and verified facts

- Current behaviour, read from `internal/gateway/context_guard.go`: the proactive
  guard only trims when it knows the window (`promptBudget > 0`). The OpenCode
  free model advertises 200k in `clients/opencode/models.go`, so the guard
  *would* trim — but only if the refusal is recognised as a size refusal first.
- `clients/opencode/error.go` has marker lists for model / free-tier / rate /
  quota / auth. There is **no** context-size marker, so a 400 containing
  "context length" falls through `kindClient` -> `coreKindFor` -> `FailureOther`.
- Response shape for the refusal reaching the client:
  `upstreamErrorShape` maps `FailureOther` to `typ = invalid_request_error`
  and `errorCodeFor` to `invalid_request`. Codex only compacts on
  `context_window_exceeded`, which is what `FailureContextWindow` already maps
  to (`internal/gateway/server.go`).
- `internal/gateway/responses.go` already decodes and replays a `compaction`
  input item: it keeps the plaintext summary when it holds one and drops real
  ciphertext (`translateResponsesInput`, `case "compaction"`).
- Official protocol (verified against the OpenAI docs):
  - `POST /responses` accepts `context_management: [{type, compact_threshold}]`.
  - When the rendered token count crosses the threshold the server compacts and
    the response stream carries a `compaction` output item; the item's
    `encrypted_content` is opaque state the client must append to the next
    `input`.
  - `POST /responses/compact` is the explicit form and returns an object with
    `object: "response.compaction"` whose `output` is the retained user
    messages followed by one `compaction` item.
- **Constraint that shapes the whole design:** OpenAI's `compaction` blob is
  OpenAI-server state. A third-party gateway cannot mint a blob OpenAI will
  accept, and OpenAI will never mint one this gateway's upstreams understand.
  The only honest implementation for this project is a **gateway-owned** blob
  that this gateway can verify and replay, and that is never sent to an
  upstream as ciphertext.

## Scope

In scope:

- Classify context-size refusals in the OpenCode module.
- A shared, conservative context-refusal/limit recogniser used by modules.
- `POST /v1/responses/compact` (buffered and streamed).
- `context_management` on `POST /v1/responses`, with automatic compaction when
  the estimated rendered tokens cross `compact_threshold`.
- A gateway-owned compaction blob (mint + verify + replay) carried in the
  standard `type: "compaction"` item shape.
- Streaming: `response.output_item.added` / `response.output_item.done` /
  `response.completed` for the compaction item.
- Usage accounting for compaction turns.
- Docs (`README.md`, `docs/REFERENCE.md`, `docs/REFERENCE.zh-CN.md`,
  `CHANGELOG.md`).

Out of scope (explicit):

- Emitting or accepting genuine OpenAI ciphertext.
- Stateful `previous_response_id` / `store` (the gateway stays stateless).
- Compaction on the Anthropic `/v1/messages` surface.
- Changing when Codex chooses to compact for non-OpenAI models (client-side).

## Component design

### 1. Context-refusal recognition (shared)

New helper in `internal/gateway/context_guard.go`:

```go
// looksLikeContextRefusal reports whether an upstream message is a prompt/context
// size refusal, and the limit when the vendor named one.
func looksLikeContextRefusal(msg string) (limit int64, ok bool)
```

It reuses the existing `mentionsSize`, `tooLongPatterns`, `parseLimitFromMessage`
and `unescapeWireEscapes` machinery, and adds the wording variants seen in the
wild that carry **no** number, e.g.:

- `more tokens than this model's maximum context length allows`
- `reduce the length of the messages`
- `run compaction`
- `maximum context length`
- `context length` / `too long` / `too large` (already partly covered)

`ok == true` means "this is a size refusal"; `limit > 0` only when a number was
also parsed. The recogniser must stay conservative: an unrelated 400 that merely
mentions "context" must not become a compaction trigger. Tests pin both the
positive wordings and a negative set.

### 2. OpenCode classification

In `clients/opencode/error.go`:

- Add `contextMarkers` for the no-number wordings.
  - Extend the classifier with a new `kindContext` verdict, placed **after**
    the model/rate/quota/free-tier/auth markers so existing verdicts never
    change; it is the last text-marker arm before the status fallback.
- Map it to `core.FailureContextWindow` in `coreKindFor`.
- `noteFailure` must treat it like `kindModel`/`kindClient`: **no account
  penalty**. An oversized body is the caller's request shape, not the
  credential's health.
- `classifyHTTP` must preserve the vendor's original 400 status.

Regression tests mirror `clients/loomy/context_window_test.go`:
classification, status preservation, exactly one upstream call, no account
cooldown, and the exact production message
(`Your request resulted in more tokens than this model's maximum context length
allows. Please reduce the length of the messages or run compaction.`).

### 3. `/v1/responses/compact`

Route: `mux.HandleFunc("/v1/responses/compact", s.auth(s.handleResponsesCompact))`.
Order before `/v1/responses` is irrelevant (Go's mux prefers the longer
pattern).

Request type `responsesCompactRequest`:

```go
type responsesCompactRequest struct {
    Model              string          `json:"model"`
    Input              json.RawMessage `json:"input,omitempty"`
    Instructions       string          `json:"instructions,omitempty"`
    PreviousResponseID string          `json:"previous_response_id,omitempty"`
    PromptCacheKey     string          `json:"prompt_cache_key,omitempty"`
    ServiceTier        string          `json:"service_tier,omitempty"`
    Metadata           json.RawMessage `json:"metadata,omitempty"`
    ConversationID     string          `json:"conversation_id,omitempty"`
    ConversationIDCamel string         `json:"conversationId,omitempty"`
}
```

Validation: `input` must not be empty and must contain at least one user
message; otherwise 400 `invalid_request_error`.

Response type: `responseObject` with `Object = "response.compaction"` and
`Output` = the retained user messages (as `message` items with `input_text`
parts) followed by exactly one `compaction` item:

```json
{ "type": "compaction", "id": "cmp_...", "encrypted_content": "<gateway blob>" }
```

`responseOutput` gains an `encrypted_content` field for the compaction item and
must omit the fields that do not apply to it.

### 4. Gateway-owned compaction blob

New file `internal/gateway/compaction.go`:

- Format: base64url of `v1.<json payload>.<hmac>` or an equivalent versioned
  envelope. The payload carries the summary text, the source message count, the
  model/platform the summary was produced for, and a created timestamp.
- The key: HMAC-SHA256 over the payload, using a **persisted random key**
  (32 bytes, generated on first use) stored at `data/compaction.key`, 0600.
  `data/` survives the installer (see project conventions), so blobs replayed
  after an upgrade still verify. If the file cannot be read or written the
  gateway falls back to an in-process key and logs one warning: compaction
  must never fail a request because of key storage.
  A blob whose signature does not verify (foreign ciphertext, tampering, a
  different deployment) is dropped, never turned into a hard failure.

The blob is **never** forwarded to an upstream as ciphertext. On replay the
gateway verifies it and materialises the summary as an assistant message.

### 5. Summarisation

`handleResponsesCompact` and automatic compaction both need a summary. Two
viable routes; the spec selects the second:

- (a) Ask the same upstream model to summarise via a normal `routeAndServe`
  call with a built-in compaction prompt. Costs an extra upstream round trip and
  can itself hit the window.
- (b) **Split-and-summarise within budget**: reuse `applyContextBudget` to keep
  the newest turns that fit, ask the upstream to summarise the *older* prefix in
  one (or more) bounded calls, and concatenate. This is the only approach that
  works when the whole conversation is over the window.

Selected: (b), with a bounded number of summarisation calls and a hard cap on
the produced summary length. If every summarisation attempt fails, fall back to
the deterministic retention list (latest user turns) so the endpoint still
returns a usable, honest `response.compaction` object rather than a 500.

The summarisation prompt and framing must be a gateway constant, not
vendor-specific wording, and must not leak tool schemas or system prompts that
the caller did not ask to preserve.

### 6. `context_management` on `/v1/responses`

- Decode `context_management` as `[]responsesContextManagement`
  (`type`, `compact_threshold`).
- Accept only `type == "compaction"`; unknown entries are ignored (consistent
  with the file's existing "unknown fields never 400" rule).
- `compact_threshold` is a positive int; `0`/absent means "no automatic
  compaction".
- In `handleResponses`, after translation and before routing, compute
  `estimateRequestTokens(req)`. If a threshold is set and the estimate is
  >= threshold, perform compaction first, replace the conversation with the
  compacted history, and remember that a `compaction` item must be appended to
  the response output.
- The threshold is judged against the **rendered/estimated** count, matching the
  docs' wording, and never against the upstream's reported usage (which the
  gateway does not have before the call).

### 7. Emitting the compaction item

- Buffered `bufferResponse`: when the request carried a compaction item (either
  freshly produced by automatic compaction or replayed and verified), append it
  after the assistant message/function calls, with status `completed`.
- Streaming `streamResponsesSSE`: for a freshly produced compaction item emit
  `response.output_item.added` then `response.output_item.done` carrying the
  full item (per the docs, `.done` is where the encrypted content is
  authoritative), and include it in the final `response.completed` output.
  A short compaction may finish without deltas — the implementation must not
  assume any delta events.
- `responseOutput.MarshalJSON` must keep `arguments` present only for
  `function_call`, and include `encrypted_content` only for `compaction`.

### 8. Usage accounting

- One usage row per compaction request, same as chat: a successful
  `/responses/compact` records model, platform, account where known, and the
  summarisation call's token usage when the module reported it.
- The automatic-compaction path records the trim/compaction on the existing
  usage row (`Trimmed*` fields stay meaningful) and must not double-count the
  summarisation call as a second caller-visible request.
- `errEmptyCompletion` logic must not fire for a legitimate
  `response.compaction` object whose only content is the compaction item.

## Error handling

- Unknown/failed model resolution on `/responses/compact`: same error envelope
  and status as `/v1/responses` for the same failure.
- Foreign/tampered/expired blob on replay: drop it (existing behaviour) and
  continue; never 400 the whole conversation because of it.
- Summarisation failure: degrade to the deterministic retention list, never
  fabricate a summary, never return a 500 unless the request itself is invalid.
- Oversized request with no way to compact: keep the existing
  `context_window_exceeded` 400.

## Testing

Layer 1 — context-refusal recognition (unit):
- Positive: production OpenCode wording, `maximum context length`, `prompt is
  too long`, `too many tokens`, Chinese Loomy wording, escaped `\uXXXX`.
- Negative: `no such field`, `invalid api key`, `rate limit`, a generic 400.
- Limit parsing: two-number form yields the compare limit; no-number form yields
  `ok=true, limit=0`.

Layer 2 — OpenCode module (unit, mirrors `clients/loomy/context_window_test.go`):
- `classify` returns the context verdict for the exact 400 body; `classifyHTTP`
  produces `core.FailureContextWindow` with the original status; one call only;
  no account penalty.

Layer 3 — gateway endpoint (uses the real `httptest` server + a stub client):
- `POST /v1/responses/compact` returns `object: "response.compaction"`, retains
  user messages, and returns one `type: "compaction"` item with a non-empty
  `encrypted_content`.
- The returned blob, replayed in a subsequent `/v1/responses` `input`, is
  verified and materialised; a foreign blob is dropped without error.
- `context_management.compact_threshold` triggers compaction only when the
  estimate crosses it; the stream carries the compaction item's
  `.added`/`.done`; the buffered response includes it in `output`.
- Usage: exactly one row for a compaction request; a compaction-triggered chat
  turn still records exactly one row.
- Error: `/responses/compact` with empty `input` is 400; a foreign blob is
  accepted silently.

Layer 4 — regression:
- `go build ./...`, `go test ./...`, plus `go test ./installer/...` if the
  installer asserts version strings affected by this change (no version bump
  is in scope for this spec).

## Documentation

- `README.md`: new endpoint, `context_management`, gateway-owned blob caveat,
  and the OpenCode classification fix.
- `docs/REFERENCE.md` + `docs/REFERENCE.zh-CN.md`: the compaction row already
  exists; extend it with `response.compaction`, the blob's
  gateway-owned nature, and replay semantics.
- `CHANGELOG.md`: one entry under a new version heading (version bump decided
  in the packaging step, not here).

## Risks and mitigations

| Risk | Mitigation |
|---|---|
| Over-broad context recognition turns unrelated 400s into compaction triggers | Conservative marker set; the recogniser returns `ok` only for explicit size wording; negative tests. |
| Blob replay fails after a key loss or a move to another deployment | Key persists in `data/`; an unverifiable blob is dropped (today's behaviour for foreign ciphertext), never a request failure. |
| Summarisation itself exceeds the window | Split-and-summarise using `applyContextBudget`; bounded number of calls; deterministic retention fallback. |
| Codex path expectations differ from docs (e.g. event names) | Follow the docs exactly (`response.output_item.done` carries the item) and pin with tests; verify against the live client in the packaging step if the user asks. |
| Double usage rows | Explicit tests for one-row-per-request on both paths. |

## Open questions resolved in this spec

1. Order of work: classification fix first (stops the current bleeding), then
   the compaction protocol. Both land before the install package is built.
2. Blob ownership: gateway-owned, HMAC-signed with a persisted key, replayable
   only by this deployment. Accepted.
3. Streaming: implement both buffered and streamed, following the docs' event
   sequence.

