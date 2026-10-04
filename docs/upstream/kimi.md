# upstream spec — kimi

**Route:** `kimi/<model>` · **Licence of the reference:** MIT (port freely)

## Reference implementation (on disk)

`client2api-lab\_upstream\kimi2api\` — TypeScript/Node, MIT, ~600 lines in 7 files:
`src/{kimi,openai,anthropic,responses,server,cli,types}.ts`. The **only runtime
dependency is `express ^4.19.2`**. `src/cli.ts` is 8 lines (just
`KIMI2API_HOST` default `127.0.0.1`, `KIMI2API_PORT` default `3000`).

## How the reference works — and why we are NOT copying that shape

The reference does **not** speak HTTP to an upstream. It spawns a local process:

```
kimi -p <flattened prompt> --output-format stream-json --skills-dir empty-skills [-m <model>]
```

stdio = ignore/pipe/pipe, NDJSON parsed line by line, emitting text when
`role === 'assistant' && typeof content === 'string'`. **One fresh stateless
process per request.** Images are written to
`%TEMP%\kimi2api-media-<uuid>.<ext>` and referenced in the prompt as `@/path`.
Tool calls are prompt-injected (the model is told to emit
`{"tool_calls":[...]}`), not native. The CLI itself talks to
`https://api.kimi.com/coding/v1`, but kimi2api never touches that.

Models: `kimi` (default), `kimi-k2`; any other id is passed straight to `-m`.

## 🚫 The blocker: the `kimi` CLI is NOT installed

* `where.exe kimi` → empty. No `~/.kimi`, no `~/.kimi-code`. The npm global root
  contains only corepack and npm.
* Therefore **the reference cannot run on this machine at all**, and neither can
  a naive port.
* `~/.kimi-work` holds only `kimi-tools\kimi-slides.exe` + a bash wrapper.
* `~/.kimi-webbridge` holds a Go daemon `kimi-webbridge.exe` (10.4 MB,
  `v3.2.6|10401664|1788791858000`) listening on **127.0.0.1:10086**, telemetry to
  `gator.volces.com`. `daemon.pid` = 51076 is dead; no `kimi*` process is running.
  `identity.json` is just `{"device_id":"daej3fat6i0tjs6qoth0"}` — no key.
* kimi-desktop's model cache lists `k3-agent` (K3), `k3-agent-ultra`,
  `k2d6-agent`.

## Authentication paths that exist (for the README / an optional helper)

1. **Kimi Code CLI = RFC 8628 device-code OAuth.** client_id
   `17e5f671-d194-4dfb-9706-5516cb48c098`, host `auth.kimi.com`, endpoints
   `/api/oauth/device_authorization` and `/api/oauth/token`. Tokens live in the
   OS keyring (service `kimi-code`, key `oauth/kimi-code`) or
   `~/.kimi/credentials/kimi-code.json`; refreshed on every prompt (forced when
   expired, background when < 5 min remain).
2. **kimi-desktop**: QR/SMS login, then silent refresh roughly every 15 minutes
   via `POST auth.kimi.com/api/account.gateway.v1.AuthService/RefreshToken`
   (Connect/protobuf).

Install path if you need the CLI:
`irm https://code.kimi.com/kimi-code/install.ps1 | iex` (Windows also needs Git
for Windows / `KIMI_SHELL_PATH`), then `kimi login`.

## Live credentials on this machine — and why they are NOT enough

kimi-desktop has an active session. JWT sub `cnu1na9kqq4j9lk2fgng`,
`abstract_user_id` `cnu1na9kqq4j9lk2fgmg`, ssid `1730129924823121270`,
device_id `7682351192822790922`, region `cn`, membership tier 10.
**access_token lives 900 seconds** (iat 1788936854 → exp 1788937754);
**refresh_token lives 90 days** (exp 1796712854). Last successful refresh
2026-09-09 14:54:19.

Where they are stored:

* kimi-desktop keeps **plaintext JWTs** in its Electron localStorage LevelDB
  (origin `https://www.kimi.com`, keys `access_token` / `refresh_token`).
* `%APPDATA%\kimi-desktop\bridge-store\token-store.json` is
  `{"encryption":"safeStorage.v1","data":"<1896 b64 → 1422 bytes, magic 'v10' = Chromium OSCrypt / Windows DPAPI>"}`.
* `Network\Cookies` holds only UI preferences.

👉 **These desktop tokens are not usable by the reference's design** (it needs
the CLI's own keyring credential). Do not pretend otherwise. Either install the
CLI, or make the module speak `api.kimi.com/coding/v1` directly with the
desktop token — the latter is a **new** protocol nobody has reverse-engineered
here, so treat it as a research task with a clear TODO, not a deliverable.

## What to build

Port the reference's **shape** (it is MIT and small) into Go:

* `clients/kimi/kimi.go` — the `core.Client`
* `clients/kimi/cli.go` — locate and run the `kimi` binary, NDJSON stream parse,
  cancellation, per-request timeout, concurrency cap
* `clients/kimi/prompt.go` — flatten `core.ChatRequest` into the CLI prompt,
  including the tool-call injection block and the image `@/path` handling
* `clients/kimi/openai.go` — reuse the core's types; do not re-invent them

**Improvements we require over the reference** (all of these are real defects in
it):

1. **A concurrency cap and a request timeout.** The reference spawns unbounded
   OS processes with no backpressure and no timeout.
2. **Do not pass `--skills-dir empty-skills` by default.** It replaces the user's
   skill directory and silently changes model behaviour. Make it opt-in config.
3. **Do not run the CLI in its `auto` permission mode by default.** The wrapped
   agent can otherwise execute tools, write files and run shell commands on the
   host with no sandbox. Make the permission policy explicit config, defaulting
   to the most restrictive mode the CLI offers, and **document this loudly** —
   it is the single biggest risk in this module.
4. **Propagate cancellation** — kill the child process when `ctx` is done.
5. **Real usage accounting if the CLI reports it**; otherwise emit no
   `EventUsage` rather than a fake `{0,0,0}` (the reference's behaviour).
6. **Parse tool calls structurally**, not by regexing the whole output for a
   top-level `{"tool_calls":[...]}`. If the CLI cannot stream partial tool
   arguments, emit the whole call in one fragment and say so in the README.
7. **Do not flatten SSE into one block** if the CLI's NDJSON gives you
   incremental text — emit `EventDelta` per fragment.

## Degraded mode (important)

Because the CLI may be absent, the module **must degrade honestly**:

* `New` succeeds even with no CLI and no credentials.
* `Chat` returns `core.ErrNotConfigured` with a message that names the missing
  prerequisite (`kimi CLI not found on PATH` vs `not logged in`).
* `Status()` reports `Ready:false`, the discovered binary path (or "not found"),
  the credential file it looked for, and what to run to fix it.
* `Models()` returns the built-in fallback catalog offline.

Do **not** auto-install the CLI, do not download anything, do not prompt.

## Deliverables

* `clients/kimi/{kimi.go,cli.go,prompt.go,catalog.go}`
* `clients/kimi/kimi_test.go` — offline tests: prompt flattening (text, images,
  tools, multi-turn, system), NDJSON decoding (including malformed lines and
  partial reads), child-process lifecycle (use a stub script, not the real CLI),
  cancellation, and the degraded-mode status.
* `clients/kimi/README.md` — prerequisites, the permission-mode warning, config
  schema, known gaps.

## Definition of done

* `go vet ./... && go build ./...` clean; `go test ./clients/kimi/...` green
  **offline** (the real CLI is not present, so tests must not require it).
* `GET /v1/models` lists `kimi/<id>` entries from the fallback catalog.
* `GET /v1/status` explains, in one line, that the CLI is missing.
* If — and only if — the CLI happens to be installed, a live round trip works.
  Otherwise the honest degraded report *is* the acceptance criterion.
