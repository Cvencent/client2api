# upstream spec — workbuddy

**Route:** `workbuddy/<model>` · **Licence of the reference:** MIT (port freely)

## Reference implementation (on disk, read it)

| Path | What |
|---|---|
| `client2api-lab\_upstream\workbuddy2api-panel\` | **Go 1.22.5, MIT — this is our skeleton donor and the primary reference** |
| `…\internal\upstream\client.go` (89700 B) | upstream transport, request building, SSE cleaning |
| `…\internal\server\handler.go` (57998 B) | OpenAI surface, error policy |
| `…\internal\auth\auth.go` | credential model, `Parse`, `SaveAtomic`, `LoadDir` |
| `…\internal\pool\{pool.go,entry.go}` | account pool + selection |
| `…\internal\panel\login.go` | OAuth state flow |
| `…\cmd\server\{main.go,config.go,wiring.go}` | wiring + config (env prefix `WB2A_*`) |
| `client2api-lab\workbuddy2api-lab\hub\wb_proxy.py` (6066 lines) | older Python monolith; useful for the *body* details: `build_upstream_body`@2509, `inject_prompt_cache_key`@2618, `clean_chunk`@1867, `parse_dsml_tool_calls`@2332, `backfill_reasoning_content`@2221, `record_usage`@382 |
| `client2api-lab\workbuddy2api-lab\tom\` | Tom6814/WorkBuddy2API — `server.py`, `codebuddy_direct_api.py`, `workbuddy_checkin.py`, `token-acquisition/` |

## Product

WorkBuddy is Tencent's coding agent. Two **realms**, independently routed:

* `intl` — `www.workbuddy.ai`
* `cn` — `copilot.tencent.com` (a.k.a. `codebuddy.cn`)

An account belongs to exactly one realm; the realm is part of the credential and
must be sent to the right host. Do **not** hardcode one realm.

## Credentials

Reference layout — one JSON file per account, mode 0600, atomic tmp+rename:

```json
{
  "auth":    { "accessToken": "...", "refreshToken": "...", "expiresAt": 1790000000,
               "domain": "copilot.tencent.com", "realm": "cn" },
  "account": { "uid": "...", "enterpriseId": "...", "nickname": "..." }
}
```

* Directory: `auth_dir`, default `./auths`, glob `workbuddy*.json`. In our
  layout this becomes `Deps.DataDir` (= `data/workbuddy/`), glob `*.json`.
* Refresh: `POST /plugin/auth/token/refresh` on the realm host.
* Acquisition (for the README / an optional helper command, not the request path):
  * `GET /v2/plugin/auth/state?platform=CLI`
  * `GET /v2/plugin/auth/token?state=<state>`
  * `GET /v2/plugin/login/account?state=<state>`

**On this machine:** the WorkBuddy desktop app is installed at
`%LOCALAPPDATA%\WorkBuddy`, `%APPDATA%\WorkBuddy`, `~\.workbuddy`
(`app/`, `binaries/`, `connectors/`, `blobs/`, `cache/`), plus `~\.codebuddy`
and `~\CodeBuddy`. Look there for live tokens before inventing a login flow —
auto-discovery beats configuration. Whatever you find, copy it into
`Deps.DataDir` under our own schema; never mutate the vendor's files.

## OpenAI surface we must expose (core already does the HTTP)

Our gateway exposes `POST /v1/chat/completions` and `GET /v1/models`. The
reference also has `GET /status` and `GET /healthz` — ours are `/v1/status`
and `/healthz`; do not reimplement them.

## Must-carry request details (from the Python reference)

* `inject_prompt_cache_key` — inject the prompt-cache key so repeated prefixes
  hit the upstream cache.
* `clean_chunk` — upstream SSE frames need normalising before they are
  re-emitted.
* `parse_dsml_tool_calls` — WorkBuddy emits tool calls as **DSML** inside the
  text stream; they must be parsed out and re-emitted as proper
  `core.EventToolCall` fragments.
* `backfill_reasoning_content` — the upstream reports reasoning separately and
  it must be backfilled into the delta stream as `Event.Reasoning`.
* `record_usage` — token accounting; surface it as one `core.EventUsage`.

## Account pool

The reference pools accounts and rotates on failure. Selection policy lives in
**your** module (`internal/pool` in the reference is 2 files, ~180 lines).

* Rotation triggers: 401/403 → refresh then retry once; 429/5xx → short cooldown;
  quota/credit exhaustion → long cooldown (reference semantics: credit/expiry
  aware, because WorkBuddy grants are per-account).
* Expose per-account health via `Status().Accounts`.

## Reuse vs rewrite

The reference is MIT Go and already compiles into roughly the same shape as our
`core.Client`. **Port the upstream layer almost verbatim** — the request body
builder, the SSE cleaner, the DSML parser, the auth struct — but:

* rewrite the wiring to `core.Deps` / `core.Client` / `core.Stream`;
* **do not** copy `handler.go`, `panel/`, or `scheduler/`. Those couple
  directly to concrete `*auth.Auth` / `*upstream.Client` / `*pool.Pool` types
  and are exactly what our core replaces. (See the note in the recon: the main
  refactor risk is that coupling.)
* keep the scheduler/signin behaviour out of the request path; if you want it,
  it is optional and must never block `Chat`.

## Models

Discover from the upstream model list; do not hardcode a stale catalog. The
catalogue is **pure-dynamic**, exactly as the reference does it: cache a good
fetch for 10 minutes, and when a fetch fails answer an *empty* list — a
hardcoded id the backend no longer serves only makes the caller pick a model
that answers `11102`. A failure arms a 5-minute negative cache so clients
polling `/v1/models` through an outage cannot amplify it, and that failure is
deliberately not reported to the account pool (a models-endpoint hiccup says
nothing about chat health). The only non-discovered ids ever served are the ones
the operator writes in the `models` config key.

## Deliverables

* `clients/workbuddy/{workbuddy.go,auth.go,pool.go,upstream.go,body.go,sse.go,dsml.go}`
  (split as you see fit, but keep it readable and each file single-purpose)
* `clients/workbuddy/workbuddy_test.go` — offline unit tests for the body
  builder, the DSML parser, the SSE cleaner, and credential load/save.
* `clients/workbuddy/README.md` — config schema, credential discovery, status
  fields, known gaps.

## Definition of done

* `go vet ./... && go build ./...` clean.
* `go test ./clients/workbuddy/...` passes with no network.
* `GET /v1/models` lists at least one `workbuddy/<id>`.
* A live non-streaming and a live streaming `POST /v1/chat/completions` succeed
  using a credential found on this machine, or `Status()` honestly reports why
  not (`Ready:false` + a specific `Detail`).
