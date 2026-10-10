# `opencode` client

OpenCode Zen — the paid model gateway run by the [opencode](https://opencode.ai)
project — exposed to client2api as an OpenAI-compatible chat endpoint under the
route prefix `opencode/<model>`.

```go
import _ "client2api/clients/opencode" // registers "opencode"
```

The module is registered by `init()` in `opencode.go`:

```go
func init() { core.Register(clientName, New) }
```

## What Zen is, in one paragraph

Zen is a single OpenAI-chat-completions endpoint in front of a mixed fleet of
models. Its own server rewrites an OpenAI chat request into the target model's
native protocol — Anthropic Messages for Claude, Google `generateContent` for
Gemini, the Responses API for GPT and Grok, `/systemone` for Jev — and converts
the response back. Everything it accepts and emits is built on a neutral
OpenAI-shaped intermediate (`CommonRequest` / `CommonChunk` / `CommonResponse`
in the vendor's `provider.ts`), which is why this module is a plain OpenAI
client and nothing more: **one inference endpoint, `POST /chat/completions`.**

## Routing

The gateway splits `<client>/<model>` on the **first** slash
(`internal/core/registry.go:169`), so:

| request | module | `req.Model` |
| --- | --- | --- |
| `opencode/gpt-5.1` | `opencode` | `gpt-5.1` |
| `opencode/claude-opus-4-5` | `opencode` | `claude-opus-4-5` |
| `opencode/mimo-v2.6-flash-free` | `opencode` | `mimo-v2.6-flash-free` |

Model ids contain no slash, so this module never has to re-add or re-strip the
prefix. A model id that *did* contain a slash would be truncated by the gateway
before this module ever saw it.

---

## Verified versus assumed

This section is the honest ledger the module contract asks for. **There is no
`OPENCODE_API_KEY` on the machine this module was built on, so no authenticated
request was ever made.** Everything below the line marked *assumed* is inferred
from the vendor's public documentation, from the vendor's own server source, or
from the OpenAI chat-completions specification — not from an observed success.

### Verified live (unauthenticated probes, 2026-10-01)

| Claim | How it was checked |
| --- | --- |
| `GET https://opencode.ai/zen/v1/models` is public, needs no credential | 200, no `Authorization` header sent; captured in `run/zen-models.json` |
| The list carries **84 ids** and no display names, no limits | the same response; `data[]` has `id`/`object`/`created`/`owned_by` only |
| `POST /chat/completions` is reachable and validates the model | `claude-opus-4-5`, `gemini-3-flash` and `gpt-5.1` all reached the auth check; only a bogus id was rejected |
| The error envelope is `{"type":"error","error":{"type":…,"message":…}}` | observed on the live error responses |
| A missing key answers `AuthError` `"Missing API key."` | observed live |
| An unknown model answers `ModelError` `"Model <id> is not supported"` | observed live |
| The **model check runs before the auth check** | a bogus id with no key returned `ModelError`, not `AuthError` |
| There is **no** balance, credit or quota endpoint | `POST /zen/v1/key`, `/zen/v1/credits`, `/zen/v1/me` all 404 with an HTML body |
| Zen speaks OpenAI chat completions and translates server-side | the vendor's `provider.ts` builds converters over `anthropic \| openai \| oa-compat` on an OpenAI-shaped intermediate |

### Assumed, or not verifiable from this machine

| Claim | Status |
| --- | --- |
| A valid key actually serves a completion | **Never observed.** No key was available. The whole authenticated path is covered only by a fake `http.RoundTripper`. |
| The exact SSE frame shapes Zen emits | Inferred from the OpenAI chat-completions spec and from the vendor's converters. No real stream was read. |
| `stream_options: {"include_usage": true}` is honoured | Best-effort. Zen's neutral request shape does not list `stream_options`, so it is off by default (`include_usage`). |
| Per-model context and output limits | Taken from the models.dev registry entry embedded in the opencode binary (`run/zen-extract.txt`). That registry is **stale** relative to the live 84 ids: only **11 of 84** ids carry a published output limit, and 9 carry a deprecation marker. Where no number is published this module returns *unknown*, never a guess. |
| `reasoning_content` on the way out | Sent, but Zen's `CommonMessage` has no such field and its converters drop unknown keys — it may never reach the model. |
| `image_url` parts | Sent as `{"url": …}` only. `CommonContentPart.image_url` carries no `detail`, so a caller's `ContentPart.Detail` is dropped rather than sent where it would be ignored or rejected. |

### What could not be fetched

Nothing failed permanently, but two things are worth recording:

- `opencode auth list` reports **0 credentials**, and
  `C:\Users\vencent\.local\share\opencode\auth.json` does not exist on this
  machine. The credential-import path is therefore exercised only against files
  the tests write themselves.
- The vendor's `provider.ts` was read from a mirror of
  `github.com/anomalyco/opencode`; the file itself is not vendored here.

---

## Endpoints used

| Method | Path | Auth | Purpose |
| --- | --- | --- | --- |
| `POST` | `/chat/completions` | `Authorization: Bearer <key>`, or `x-api-key: public` for the anonymous free credential | the only inference call |
| `GET` | `/models` | **none** | the live id list |

Inference is the whole Zen surface: there is no check-in, no task/claim and no
balance endpoint. The `oauth` realm additionally drives the **OpenCode Console**
origin (`auth_base_url`, default `https://opencode.ai/console`):

| Method | Path | Auth | Purpose |
| --- | --- | --- | --- |
| `POST` | `/auth/device/code` | none | start a device authorisation |
| `POST` | `/auth/device/token` | none | poll for the token, and refresh it |
| `GET` | `/api/user` | Bearer | the signed-in user |
| `GET` | `/api/orgs` | Bearer | the orgs the user can bill |
| `GET` | `/api/config` | Bearer + `x-opencode-org-id` | the workspace's allowed model ids |

`GET /models` is deliberately called **without** a credential: it is public, and
sending a key to an endpoint that does not need one is a leak waiting to happen.
`TestModelsFetchesAndCachesWithACredential` pins that the request carries no
`Authorization` header.

---

## Model ids

The catalogue is a merge of two sources, and the merge order is the whole point:

1. **The live `GET /models` list wins for *which ids exist*.** It is the
   authority. An id the live list does not serve is not offered, even if the
   static table knows it.
2. **The static table fills in display names, context lengths, output limits and
   the `deprecated` marker** for ids it knows.
3. **A live id the static table does not know is still served**, with a derived
   display name (`"Muse Spark 9.9 Turbo (derived)"`) and **no** limits. An
   unknown limit is reported as unknown — inventing a number would silently
   truncate a caller's answer.

`extra_models` in the config appends ids; it can never remove one.

The public list is fetched even when no credential is configured. If the live
fetch fails, the module keeps the last known-good list and only then falls back
to the built-in catalogue.

### `ModelLimitsProvider`

```go
func (c *Client) ModelMaxOutputTokens(ctx context.Context, model string) (int, bool)
```

Answers from the static table, else from the cached live catalogue, else
**declines** (`0, false`). It **never** performs a fetch: the contract notes this
interface is consulted on every request that omitted `max_tokens`, so a network
call here would put a round trip in front of every chat. A cold cache declines.
`TestModelMaxOutputTokensNeverFetches` pins this with a transport that records
whether it was reached — it is never reached.

For **73 of the 84 live ids** the vendor publishes no output limit, so this
method declines. That is the correct answer, not a gap.

### Deprecation

Nine ids are marked `deprecated: true` in the static table — the ids the vendor's
own docs list under "Deprecated models" that the live list still serves
(`claude-sonnet-4`, `minimax-m2.5`, the five `*-codex*` ids, `glm-5`,
`kimi-k2.5`). The marker lands in `core.Model.Extra["deprecated"]` so a picker
can grey them out. Nothing is removed automatically.

---

## Configuration

Everything under `clients.opencode` is optional. A deployment that only exports
`OPENCODE_API_KEY`, or that imports the credential from opencode's own
`auth.json`, needs no config block at all.

See `config.example.json` for the complete shape.

### Top-level keys

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `base_url` | string | `https://opencode.ai/zen/v1` | Zen origin. Exists so a mirror or a local recorder can be pointed at without a rebuild. A value that is not an absolute `http(s)` URL is discarded. |
| `api_key` | string | — | single-credential shorthand; becomes one pool account |
| `accounts` | array | — | multi-credential form; see below |
| `extra_models` | string[] | `[]` | appends ids to the catalogue; never removes one |
| `extra_headers` | object | `{}` | added to every request; reserved names dropped |
| `chat_timeout` | duration | `10m` | whole-call deadline for a chat |
| `models_timeout` | duration | `20s` | deadline for `GET /models` and for the account probe |
| `models_ttl` | duration | `1h` | how long a fetched catalogue is reused |
| `idle_timeout` | duration | `2m` | SSE idle guard: no bytes for this long ends the stream |
| `cooldown` | duration | `60s` | how long a throttled account parks |
| `quota_cooldown` | duration | `12h` | how long an out-of-credit account parks |
| `max_in_flight` | int | `4` | per-account concurrent requests before `core.ErrBusy` |
| `include_usage` | bool | `false` | asks for a usage frame via `stream_options` (best-effort) |
| `disable_auth_json_discovery` | bool | `false` | turns off the on-machine `auth.json` scan |
| `test_model` | string | `gpt-5-nano` | what the panel's "test" button asks |
| `auth_base_url` | string | `https://opencode.ai/console` | OpenCode Console origin the `oauth` device-code login drives; a non-absolute `http(s)` value is discarded |
| `default_realm` | string | `free` | the realm `StartLogin` targets when the panel sends none: `free` or `oauth`; anything else folds to `free` |
| `free_models` | string[] | `["space-bunny-free"]` | extra zero-cost ids for an anonymous credential; vendor ids ending in `*-free` are recognized automatically |

**Durations** accept a Go duration string (`"90s"`) or a bare JSON number read
as seconds (`90`). A value that cannot be parsed becomes zero, and zero means
"use the documented default". This is deliberate: a mistyped duration must not
discard the rest of the block, and a config decode failure must never take the
module down. `New()` turns even a hard JSON syntax error into a logged warning
plus defaults — `clients.opencode` can never be the reason the process fails to
start.

**`extra_headers`** may not override `authorization`, `content-type`,
`content-length`, `accept`, `accept-encoding`, `user-agent`, `host` or
`transfer-encoding`: this module owns authentication, content negotiation and
framing.

### `accounts[]` entries

| Key | Meaning |
| --- | --- |
| `label` | shown in the account table; falls back to `config account N` |
| `api_key` (or `key`) | the credential |
| `id` | ignored — the id is always the key's fingerprint |
| `disabled` | accepted by the schema; the panel is the place to disable an account |

An entry with no key is dropped at normalize time: keeping it would put a
permanently-dead row in the panel.

### Environment

| Variable | Meaning |
| --- | --- |
| `OPENCODE_API_KEY` | the Zen key |
| `XDG_DATA_HOME` | relocates where opencode's own `auth.json` is looked for |

---

## Credential sources and precedence

The key can arrive from four places. Order only decides **which label wins when
the same key appears twice** — the account id is a fingerprint of the key, so two
sources holding the same key are the same account, not two.

1. **The module config** — `clients.opencode.api_key`, or an `accounts[]` entry.
2. **`OPENCODE_API_KEY`** in the environment.
3. **This module's own store** — `Deps.DataDir/accounts.json`.
4. **opencode's own `auth.json`** — `$XDG_DATA_HOME/opencode/auth.json`, else
   `~/.local/share/opencode/auth.json`.

That last path is **not** `%APPDATA%` on Windows: opencode is a Node program
that follows XDG, so the module looks in `~/.local/share/opencode` on every
platform.

The vendor file's shape is a provider **map**:

```json
{
  "opencode": { "type": "api", "key": "sk-…", "metadata": { "…": "…" } },
  "anthropic": { "type": "oauth", "access": "…", "refresh": "…", "expires": 1790000000 }
}
```

Only `type == "api"` carries a usable key. An `oauth` entry is **reported and not
imported**: Zen has no refresh flow, so importing one would create an account
that fails forever. The lookup is case-insensitive, and a miss names the
providers that *were* found so the operator can see why nothing happened.

### Persisted state

Imported and panel-added credentials are written to
`Deps.DataDir/accounts.json` at mode **0600** through an atomic rename
(`core.WriteJSONAtomic`), in a versioned envelope:

```json
{ "version": 1, "accounts": [ { "id": "opencode:…", "label": "…", "api_key": "…", "enabled": true, "source": "import" } ] }
```

An OAuth record carries `auth_mode`, `access_token`, `refresh_token`,
`expires_at`, `org_id`/`org_name`, `email` and `allowed_models` instead of
`api_key`; the anonymous free record is the fixed `opencode:anonymous` row with
`auth_mode: "anonymous"` and `api_key: "public"`.

Credentials that came from the **config or the environment are not copied onto
disk**: the operator deliberately keeps those elsewhere, and duplicating them
would make this module a second place the secret lives.

An unwritable data directory is recorded and never returned — a chat that
already succeeded must not turn into an error because a file could not be
written.

### Discovery and import

`Discover` is **read-only** and never returns an error. It reports:

- the vendor `auth.json`, kind `opencode-auth`, with a note that says what was
  found (`provider "opencode", type api, key sk-…`) and what was not;
- every `*.json` in `Deps.DataDir/import/`, kind `opencode-json`.

`Import(paths, all)` imports the named files, or every importable one when `all`
is set. A path the operator named **explicitly** gets its real error; a sweep
skips a broken file so one bad leftover cannot hide the good ones. The module's
own `accounts.json` is deliberately **not** a discovery source — it is the store,
and offering to import it would be a no-op dressed up as an action.

---

## Login

The panel's Add Account flow drives `core.RealmLoginProvider`. `LoginRealms`
publishes two realms, and `StartLogin` defaults to `default_realm` (`free`).

**`free` — anonymous.** No account is needed: the realm upserts one account with
the fixed id `opencode:anonymous`, `AuthMode: "anonymous"` and the literal
`APIKey: "public"`, sent upstream as `x-api-key: public` with no
`Authorization`. Adding it twice updates the same row rather than creating a
duplicate. Anonymous accounts are narrowed to the vendor ids ending in `*-free`
plus the configured `free_models` additions: `Models()` serves **only** those
ids, each tagged `Extra["free"] = true`, and a chat request for any other model
is refused locally without an HTTP call. The configured list remains available
for zero-cost ids that do not carry the suffix.

Because a paid id answers `Missing API key.`, the panel's Test button probes
the first configured free id for an anonymous account (a configured
`test_model` is kept only when it is itself free), and re-adding the free
account clears any failure state a previous refusal left behind.

**`oauth` — OpenCode Console.** The module runs the RFC 8628 device-code flow
against `auth_base_url`: `StartLogin` returns the `verification_uri_complete`
as `URL` plus the `user_code` as `Code`, and `PollLogin` polls
`/auth/device/token` on the server's interval, honouring
`authorization_pending`, `slow_down` (backing off up to 60s), `expired_token`
and `access_denied`. On success it reads `/api/user` and `/api/orgs`, picks
the first org, and fetches `/api/config` with `x-opencode-org-id` for the
workspace's allowed model ids. A config-fetch failure is **not** fatal: the
account is still stored and the catalogue falls back to the public list. The
record carries `AuthMode: "oauth"`, the access/refresh tokens, `ExpiresAt`,
`OrgID`/`OrgName`, `Email` and `AllowedModels`. Before each chat and account
test, `ensureFreshOAuth` refreshes the token five minutes ahead of expiry and
rewrites the same account id.

The vendor sends a relative `/console/device?…` path as the verification URL;
the module resolves it against `auth_base_url` so the panel always hands the
browser an absolute address.

When the pool is not anonymous-only, every OAuth account's `AllowedModels` is
unioned into the served catalogue so the picker offers what the signed-in
workspace can actually use; if no OAuth account has a workspace list, the
public catalogue is served unchanged.

---

## Account pool, cooldowns and errors

- One `Chat` call may try up to **3** accounts (`maxRotate`) before giving up.
  The last real verdict is what the caller sees: "every account is cooling down"
  is a worse answer than the 429 that caused it.
- `core.NoteServedBy` is called immediately after the pick, so the panel always
  shows the account that actually served the request.
- Failure verdicts are classified into auth / quota / rate / model / client /
  transport. Only the **capacity** verdicts park the account, each for its own
  duration: a rejected key is **disabled**, out-of-credit parks for
  `quota_cooldown`, throttling parks for `cooldown`. A bad request from the
  caller and an unknown model are recorded against the module, not the account —
  they say nothing about the account's health.
- Re-enabling an account clears its failure state, so it does not come back
  already parked.
- `AccountRecord.Identity` is **always empty**. Zen publishes no account
  identifier for a key — the key is not an account id — and guessing one would
  merge unrelated logins, which is worse than showing them apart.
- `AccountRecord.Fields["key"]` is `core.MaskSecret(key)`. The raw key never
  reaches the panel, and `core.Redact` plus a literal-key scrub runs before
  anything credential-shaped is logged.

---

## Streaming

The SSE reader in `stream.go` is hand-written (this repo has no shared helper).
It:

- uses `core.GoSafe` rather than a bare `go func()`;
- enforces an **idle timeout** (`idle_timeout`, default `2m`): no bytes for that
  long ends the stream instead of hanging a caller forever;
- ends cleanly on `data: [DONE]` and on `io.EOF` from the transport;
- reassembles fragmented `tool_calls` argument deltas across frames, merging by
  index so an argument slice is never appended twice;
- closes the response body exactly once, and cancels the request context when the
  caller stops reading.

Because Zen's server is itself a converter, a stream may carry frames this
module does not model; unknown frames are skipped rather than treated as errors.

---

## Files in `Deps.DataDir`

| File | Mode | Purpose |
| --- | --- | --- |
| `accounts.json` | 0600 | the credentials this module owns (imported + panel-added) |
| `import/*.json` | — | a drop-box the operator can put credential files in |

With no `DataDir` the module does not persist at all: writing a credential file
into the process working directory would be worse than losing the accounts.

---

## How `Status` is rendered

`Status` is cheap and makes **no** network call — the panel polls it every 10s.

- `Ready` is true when at least one account exists.
- With no account, the detail names all three ways to get one: set
  `OPENCODE_API_KEY`, add one in the panel, or import opencode's `auth.json`.
- A rejected config block is surfaced first, redacted and truncated.
- The pool summary and the last recorded error are appended.
- `Detail` never contains a raw credential.

---

## Interfaces implemented

| Interface | Implemented | Why |
| --- | --- | --- |
| `core.Client` | **yes** | `Name`, `Models`, `Chat`, `Status` |
| `core.AccountManager` | **yes** | the panel's account table is the editing surface |
| `core.CredentialImporter` | **yes** | `Discover` / `Import` from `auth.json` and the import dir |
| `core.ModelRefresher` | **yes** | `RefreshModels` bypasses the TTL |
| `core.ModelLimitsProvider` | **yes** | from the static table and the cache only |
| `core.PoolStatsReporter` | **yes** | in-flight accounting |
| `core.HealthProvider` | **yes** | pool census over the same account states Status renders |
| `core.LoginProvider` / `core.RealmLoginProvider` | **yes** | two realms: `free` (anonymous `x-api-key: public`) and `oauth` (Console device-code sign-in) |

### Deliberately **not** implemented

| Interface | Why not |
| --- | --- |
| `core.BalanceProvider` | **Zen exposes no balance, credit or quota endpoint.** `/zen/v1/key`, `/zen/v1/credits` and `/zen/v1/me` all answer 404 with an HTML body, and the vendor's binary contains no such path. Billing is console-only (USD credits, auto-reload $20 below $5). A fabricated `0` balance would be worse than saying so. |
| `Reviver`, `CaptchaProvider` | OAuth tokens are refreshed lazily before a call (see "Login"); there is no separate health record to revive, and no endpoint used carries a captcha. |
| `CheckinProvider`, `TaskProvider`, `BatchPlanner` | Zen has no check-in and no task/claim API. |
| `PackageProvider`, `VoucherProvider`, `BundleImporter` | No such concept in Zen. |
| `ConversationBinder`, `HintProvider`, `Degrader` | Not needed by this module's shape. |

The compile-time assertions in `health.go` prove the health capability;
`opencode.go` and `accounts.go` carry the rest of the implemented set.


---

## Known gaps

- **No authenticated request has ever been made from this machine.** The SSE
  parser, the error classifier and the account probe are exercised against fake
  transports only.
- **The two login realms are hermetic-tested, not live-verified.** The device
  flow, the token refresh and the workspace-config fetch are pinned against
  `httptest` servers; no real device authorisation has been completed from this
  machine.
- **Output limits are known for 11 of 84 ids.** The rest decline.
- **The static table will drift.** It was captured on 2026-10-01. New live ids
  are served with a derived name and no limits; retired ids drop out on their
  own because the live list wins.
- `include_usage` may be ignored by the vendor.
- A model id containing a slash cannot be addressed through the gateway, because
  routing splits on the first slash.

---

## Testing

```powershell
gofmt -l clients/opencode
go vet ./clients/opencode/
go test -count=1 ./clients/opencode/
```

The suite is hermetic: `newTestClient` blanks `OPENCODE_API_KEY` and arms the
`ensureOnce` seam so a test never reads the machine's real credentials, and every
network path goes through an injected `http.RoundTripper`. Tests that touch the
vendor path point `XDG_DATA_HOME` at a temp directory.

215 top-level tests, covering the wire body, the SSE reader, the error
classifier, the model catalogue and its merge order, the credential sources and
their precedence, discovery/import, persistence and the account pool, plus the
two login realms (anonymous free + device-code OAuth), the token refresh, the
free-allowlist narrowing and the anonymous probe-model choice.

---

## Provenance

| Fact | Source |
| --- | --- |
| the 84 live model ids | `run/zen-models.json` — a live unauthenticated `GET /models` |
| display names, the deprecated list, pricing | `run/zen-docs.txt` — a cleaned `https://opencode.ai/docs/zen` |
| per-model context/output limits | `run/zen-extract.txt` — the models.dev registry entry embedded in the opencode binary |
| the OpenAI-shaped intermediate and its converters | `run/zen-provider.ts` — the vendor's own server source |
| the `auth.json` shape and location | opencode's own credential store |
