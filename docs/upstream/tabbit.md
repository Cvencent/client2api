# upstream spec — tabbit

**Route:** `tabbit/<model>` · **Licence of the reference: GPL-3.0-only**

🔴 **`hwttop5/tabbit2api` is GPL-3.0-only.** We may **not** copy, port or link its
code into our binary — that would make the whole of client2api GPL. The only
permitted reuse is **process-level**: run it as a separate, unmodified sidecar
executable and talk to it over HTTP.

## The decision, restated

* **tabbit is a sidecar, not a native module.**
* `clients/tabbit` contains **only our own code**: process supervision, health
  probing, request forwarding, and honest status reporting.
* We never read, modify or vendor anything inside the upstream checkout.
* If the sidecar is not installed or not running, the module degrades honestly
  (see below) and **must not** try to auto-install, auto-download or auto-login.

## Reference (do not copy; read for behaviour only)

`client2api-lab\_upstream\tabbit2api\` — Node.js ESM, `playwright ^1.60.0`,
`ws ^8.21.3`, `@anthropic-ai/tokenizer`. 22 ESM modules;
`src/tabbit-web-bridge.js` 48757 B, `src/session-core.js` 39638 B,
`src/gateway-app.js` 17806 B; bin → `src/cli.js`;
`test/gateway-contract.test.js` 82370 B. v0.1.9.

**Not installed on this machine:** `node_modules` is absent and
`%LOCALAPPDATA%\tabbit2api` (its `LAB_ROOT`) does not exist → it has never run
here.

## Why it must stay a sidecar (beyond the licence)

The reference drives a **real browser**:

* It copies `%LOCALAPPDATA%\Tabbit\User Data`'s `Local State`,
  `Default/Preferences`, `Default/Cookies`, `Default/Network/*` and
  `Default/Local Storage/*` into
  `%LOCALAPPDATA%\tabbit2api\tabbit-user-data` and launches a **second** Tabbit
  Browser on that copy. `tabbit2api login --refresh` re-copies and opens the real
  client for interactive login. The README requires **all Tabbit windows be
  closed first**, otherwise the Cookies are locked and the copy fails.
* It then injects itself into the page's webpack runtime:
  `self.webpackChunk_N_E.push([[Symbol("tabbit-gateway-bridge")],{},(require)=>{runtime=require}])`
  and calls the site's own `sendMessage({message, selectedModels, mod, useDirectApi:false, references, ...})`
  with stubbed callbacks. Module ids: `sendMessage` = 51523 or 187 (export `._z`),
  chat-mode constants = 32386/86220 (`R7`) or 81487 (`R`), COS presign = 93703,
  file upload = 68886, attachment refs = 53045/45677. When the ids drift it
  falls back to scanning `Function.prototype.toString()` for
  `selectedModels+setMessages+startGenerating+stopGenerating+onChatFinish+onFailed+useDirectApi`.
* Streaming is **snapshot diffing** (`setMessages` → extract assistant text →
  diff against what was already sent → push only the new suffix) through
  `page.exposeFunction('tabbit-stream-<ts>-<seq>')`.
* There is a DOM fallback path (`sendUsingPageUi()`): `[data-chip-editor="true"]`,
  `#ChatSendButton` (wait for `data-send-blocked !== "true"`),
  `[data-message-type="assistant"]` → `.markdown-renderer` innerText,
  `[data-message-action-bar="true"]` (pointer-events-none = still generating),
  polled every 100 ms. **That path cannot attach files.**
* Above the browser it wraps every request in a "protocol translation assistant"
  JSON envelope: the model is told to emit exactly one JSON object
  `{stop_reason, content:[{type:"text"},{type:"tool_use"},{type:"server_tool_use"}]}`,
  parsed by `parseStructuredEnvelope` (strips up to 2 levels of nested single
  text blocks, removes markdown fences, slices on braces) with one repair
  round-trip on failure. Prompts are compacted to **≤19000 target / 20500 hard
  limit** characters. `runGatewaySession` iterates up to 8 rounds for tool calls.

Reproducing that is not our job. That is exactly why it is a sidecar.

## Sidecar interface (what our module talks to)

The reference's own HTTP surface (default `127.0.0.1:50124`, bearer
`sk-tabbit-local` — a static local string from `TABBIT_API_KEY` /
`DEFAULT_API_KEY`):

```
GET  /health
GET  /v1/models
GET  /v1/models/{id}
POST /v1/chat/completions      (SSE)
POST /v1/responses             (SSE)
POST /v1/messages              (+ /v1/messages/count_tokens)
     /v1/assistants*, /v1/threads*, WS /v1/realtime
```

We only need `/health`, `/v1/models` and `/v1/chat/completions`.

Models exposed by the reference: `tabbit/priority` (virtual alias, the only one
on the Anthropic face), `tabbit/Claude-Opus-4.7`, `tabbit/GPT-5.5`,
`tabbit/Claude-Sonnet-4.6`, `tabbit/GPT-5.4`, `tabbit/DeepSeek-V4-Pro`,
`tabbit/GLM-5.1`, `tabbit/Gemini-3.1-Pro`, `tabbit/Default`. Each carries
metadata (`tabbit_display_name`, `tabbit_selected_model`, `supports_images`,
`supports_tools`, `support_thinking`, `model_access_type`, `priority_group`,
`priority_rank`, `owned_by: tabbit`).

## Our module's responsibilities

`clients/tabbit/`:

1. **Locate** the sidecar. Config first (`base_url`, `api_key`, `command`,
   `args`, `workdir`), then a small discovery list of likely install paths and
   `PATH`. If nothing is found → `Ready:false`, `ErrNotConfigured` on `Chat`.
2. **Supervise** it *only if the user opted in* (`"manage": true`). Otherwise
   assume it is already running and just talk to it. Never start a browser
   unasked.
3. **Proxy** `GET /health`, `GET /v1/models` and
   `POST /v1/chat/completions`. Translate the sidecar's OpenAI-shaped SSE into
   `core.Event`s. The sidecar speaks OpenAI already, so this is mostly a
   re-framing job — but verify the frame shape rather than assuming.
4. **Report honestly** in `Status()`: base URL, whether `/health` answered,
   sidecar version if reported, model count, and the last upstream error.
5. **Never** touch the sidecar's checkout, its profile copy, its cookies, or its
   `node_modules`.

### Mapping to our model namespace

The sidecar's ids are already prefixed `tabbit/...`. Our core strips
`<client>/`, so a request for `tabbit/priority` arrives with
`req.Model == "priority"`. Re-add the `tabbit/` prefix when forwarding, and
strip it again from `Models()` results — otherwise `GET /v1/models` would list
`tabbit/tabbit/priority`.

### Offline behaviour

`Models()` must return a built-in fallback catalog (the list above) when the
sidecar is unreachable, so the panel and `GET /v1/models` stay useful.

## Known upstream hazards (document in the README, do not try to fix in-process)

* **Host drift:** the reference hardcodes `web.tabbit.ai`, but this machine's
  live session is on **`web.tabbit.com`** (64 hits vs 4). A sidecar pinned to the
  wrong host will silently fail. Surface the configured host in `Status()`.
* **webpack module ids and DOM attributes change with every build** → the bridge
  breaks silently.
* **Profile copying is inherently racy** (needs all windows closed; tolerates
  EBUSY/EPERM; cannot read locked Cookies).
* **Prompt truncation:** > 19000 chars is silently dropped, > 20500 hard-fails.
* **Snapshot-diff streaming:** an edit or retraction re-sends the whole text,
  producing duplicates or out-of-order output.
* `[492]`, welcome banners and free-tier prompts surface as `model_unavailable`.

## A better transport exists — and is off-limits

`%LOCALAPPDATA%\Tabbit\LocalAgent\bin\tabbit-cli.exe` (2670440 B) is a thin
client that reaches a Runtime Service **inside the already-running Tabbit
Browser** over a Windows named pipe (`launcher-target.json` →
`D:\tabbit\...\TabbitDance\tabbit-playwright-cli.exe`; `instances\E8BFCAF84FC45E19.json`;
`skills\tabbit\SKILL.md` 30408 B). It drives the user's **real logged-in profile
and real tab groups** — no profile copy, no second browser. Its dispatch op is
`"cli"` → `runCliCommand(argv, stdin, controller, workingDirectory)`, and its
transport descriptor is
`{kind:"win-named-pipe", address:"\\\\.\\pipe\\..."}` with a token and an
`instanceId` matching `/^[0-9A-F]{16}$/` from the browser's `endpoint.json`.

🚫 **Handling `endpoint.json` or that token is forbidden by the `tabbit` skill's
own rules.** Do not read, copy, log or use them. Note the option in the README as
future work and move on.

## Live evidence on this machine

* Tabbit Browser is installed and running (19 processes), real exe at
  `D:\tabbit\Tabbit Browser\Application\Tabbit Browser.exe`.
* `%LOCALAPPDATA%\Tabbit Browser\User Data\Default\Preferences` (16795 B,
  2026/9/28 20:20:39) contains `account_info`, `account_id`,
  `accounts_metadata_dict`.
* `Default\Network\Cookies` (32768 B) is **locked by the running browser** and
  cannot be copied offline.
* `Default\Local Storage\leveldb\000003.log` (32301 B) has
  `chat_user_base_info` under the `https://web.tabbit.com` origin. History shows
  a completed auth flow:
  `https://web.tabbit.com/login?callback=close&launch_source=first_run_login_tab`
  → `/chat/new` → `/session/<uuid>` → `/member/usage`.
* **Auth is cookie-based; no plaintext JWT was found in the leveldb.**

## Deliverables

* `clients/tabbit/{tabbit.go,sidecar.go,proxy.go,openai.go}`
* `clients/tabbit/tabbit_test.go` — offline tests: sidecar discovery precedence,
  model prefix re-add/strip round-trip, SSE re-framing (with a recorded fixture
  served by `httptest.Server`), degraded status when `/health` fails, and
  `Chat` returning `ErrNotConfigured` when no sidecar is configured.
* `clients/tabbit/README.md` — the GPL boundary, the config schema, how to
  install and run the sidecar, the hazards above, and the forbidden
  `tabbit-cli` note.

## Definition of done

* `go vet ./... && go build ./...` clean; `go test ./clients/tabbit/...` green
  offline **without Node or Playwright present**.
* `GET /v1/models` lists `tabbit/<id>` from the fallback catalog.
* `GET /v1/status` reports the sidecar as not configured/not reachable, in one
  clear line.
* The module contains **zero** GPL-derived code — state this in the README.
