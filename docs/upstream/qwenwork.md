# upstream spec — qwenwork

**Route:** `qwenwork/<model>` · **Licence of the reference: NONE**

🔴 **`nostalgia296/qwenwork2api-makers` ships no LICENSE, no COPYING and no
NOTICE; `package.json` has `private: true` and the repo has a single commit
(`ca6ee5d "更新"`). Default copyright applies — copying, porting or inlining any
of its code is not permitted.**

This module is therefore a **clean-room Go implementation**. Read the reference to
learn the *protocol*, then write Go you own. Do not copy code, comments,
identifiers or file layout. Keep a provenance note in the README.

## Reference implementation (on disk, READ ONLY)

`client2api-lab\_upstream\qwenwork2api-makers\` — JS ESM, **zero runtime
dependencies**, targeting Cloudflare Workers (`wrangler.toml`) and EdgeOne Pages
(`edgeone.json`). Relevant files: `worker.js`,
`edge-functions\_shared\{config,cosy,crypto,upstream,body,accounts,kv,sse,auth,admin,http,stats,desensitize}.js`,
`edge-functions\v1\{chat/completions,models}.js`, `edge-functions\admin\*`,
`edge-functions\cron\keepalive.js`, `public\index.html`.

Note: the reference cannot run locally at all — all state lives in an Edge KV
binding, there is no HTTP server and no CLI, and EdgeOne caps CPU at **200 ms
per slice** with a 1 MB body limit. Its own `scripts/selftest.mjs` *does* pass on
node v22.23.2 (md5 / aes-128-cbc / base64 / RSA PKCS#1 v1.5 / desensitize), and
`scripts/workertest.mjs` / `e2etest.mjs` fake a KV and monkeypatch `fetch`.

## What it proxies

**`gateway.qwenwork.cn` — the QwenWork / QoderWork CN *desktop agent* backend.**
Not the Qwen web chat, and not a local CLI.

* Chat: `POST /algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common` (SSE)
* `GET /algo/api/v2/model/list`
* `GET /api/v1/userinfo`
* `GET /api/v1/adapter/user/account-context?include=user,plan,quota`
* Daily check-in (the "credits growth card" service, same host):
  `GET /sash/api/v1/me/daily-check-in/status`,
  `POST /sash/api/v1/me/daily-check-in/claim`
* `GET /sash/api/v1/me/invitationCode` (declared by the vendor, not used here)
* `POST /api/v1/deviceToken/refresh`, `POST /api/v1/deviceToken/poll`

## Credentials — PKCE (S256) device-authorisation flow

Not a cookie grab, and not a displayed device code.

1. Generate a random 64-char PKCE verifier → SHA-256 → base64url challenge, plus
   a random nonce and a `machine_id`. Build:

```
https://gateway.qwenwork.cn/device/selectAccounts
  ?challenge=<b64url>&challenge_method=S256
  &nonce=<nonce>&machine_id=<machine_id>
  &client_id=e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb
  &redirect_uri=qwenwork-cn://
```

2. The user opens that URL and authorises.
3. Poll `GET /api/v1/deviceToken/poll?nonce=&verifier=&challenge_method=S256`.
   `404` or `202` = still pending. Success yields
   `{token|device_token, refresh_token, expires_in|expires_at, user_id, user_name}`.
4. Refresh: `POST /api/v1/deviceToken/refresh` with `{"refresh_token": ...}`.

User-Agent `qoderwork/1.0.5`.

Since we are a local program and not an Edge worker, implement this as a
**CLI helper** (`client2api.exe -login qwenwork`, or a small `cmd/` subcommand —
your call, but it must not touch another module) that prints the URL, polls, and
writes the credential into `Deps.DataDir` with `core.WriteJSONAtomic`.

## Authentication is a COSY signature, not a bearer cookie

For each account:

1. Build a **compactly-sorted** JSON object
   `{uid, aid, nickname, email, accessToken}`.
2. AES-128-CBC encrypt it with a **random 16-character `tempKey` used as both the
   key and the IV** → base64 → this is `info`.
3. RSA-encrypt `tempKey` with **PKCS#1 v1.5** under a **hardcoded 1024-bit public
   key** (`RSA_N = 0xc0f22307…ebcecf`, `e = 65537`) → base64 → this is `cosyKey`.
4. `payload = sortedCompact{cosyVersion:"1.1.18", ideVersion:"1.0.5", info,
   requestId, version:"v1"}` → base64 → `payloadB64`.
5. `sig = md5( payloadB64 + "\n" + cosyKey + "\n" + unixDate + "\n" + body + "\n" +
   pathnameWithoutTheLeading"/algo" )`
6. Send `Authorization: Bearer COSY.<payloadB64>.<sig>`

Accompanying headers: `x-qwenwork-version`, `-release-version`
(`1.0.5-26090901`), `-build` (`26090901`), `-platform`, `-arch`, `-channel`;
`cosy-version`, `cosy-clienttype` (`6`), `cosy-business-product` (`qoder_work`),
`cosy-business-type` (`agent`), `cosy-scene` (`qwork`), `cosy-machineos`
(`x86_64_win32`); `login-version: v2`; `cosy-key`, `cosy-user`, `cosy-date`,
`cosy-machineid`; `x-model-key`; `x-model-source: system`.

The reference reimplements md5, AES-128-CBC, base64/base64url, uuid and RSA
PKCS#1 v1.5 in pure JS. **In Go, use the platform `crypto/*` packages** — do not
transliterate the pure-JS implementations.

⚠️ Reusing the first-party `client_id`, `redirect_uri`, User-Agent and the whole
COSY header set carries a real account-ban risk. Note it in the README.

## Request body (the QwenWork agent envelope)

```json
{
  "request_id": "...", "request_set_id": "...", "chat_record_id": "...",
  "session_id": "...", "stream": true, "chat_task": "FREE_INPUT",
  "chat_context": { "text": "...", "extra": { "modelConfig": {"key": "...", "is_reasoning": false}, "originalContent": "..." } },
  "is_reply": false, "source": 1, "version": "3",
  "agent_id": "agent_common", "task_id": "common", "session_type": "qoder_work",
  "model_config": { "key": "...", "format": "openai", "is_vl": true, "source": "system", "max_input_tokens": 180000 },
  "system": "...", "messages": [...], "tools": [...],
  "parameters": { "...": "...", "max_tokens": 32000 },
  "business": { "product": "qoder_work", "type": "agent", "version": "1" }
}
```

`CONTEXT_LENGTH = 180000`, `MAX_COMPLETION_TOKENS = 32768`.

## Models

`pro` (default), `flash`, `qwen3.8-max-preview`. Live list from
`/algo/api/v2/model/list` (`qwork[]` where `enable !== false`), cached 5 minutes.
Reference aliases: `auto|advanced|pro → pro`, `lite|flash → flash`,
`max|qwen3.8-max* → qwen3.8-max-preview`.

## Account pool (reference policy)

LRU selection, at most 3 attempts, one refresh-and-retry on 401/403, 15 s
cooldown on 5xx/429, **24 h cooldown on quota exhaustion** (`14018`, "积分不足"),
`markDead` when refresh fails. TTLs: login 10 min, session 12 h, cooldown 60 s
(short 15 s), refresh margin 30 min, model cache 5 min.

Be aware that multi-account LRU rotation plus a 24 h quota cooldown is
essentially free-tier farming. Implement it (it is the reference's behaviour) but
say so in the README.

## Our OpenAI surface

Core provides `POST /v1/chat/completions` and `GET /v1/models`. The reference
also has CORS preflight, `allowAnonymous`, an `sk-qwenwork-<hex>` key and a
buffered (non-stream) mode — ours come from the core, so do not reimplement them.
No `/v1/embeddings`, no `/v1/responses`.

## Live evidence on this machine

* **QwenWork (千问办公) v1.2.1 is running**: 9 `QwenWorkCN.exe` processes from
  `D:\qwenwork\QwenWorkCN\1.2.1-26092107\QwenWorkCN.exe` plus `qwenworkhelper.exe`.
  PID 24944 owns `127.0.0.1:54365`.
* **`C:\Users\vencent\.qwenworkcn\mcp-adaptor.config`** — verified live:

```json
{"url":"http://127.0.0.1:54365",
 "token":"bc00303bcb5d6d4e6478bdd1bb0e7c9ac4be663f020b49027cf977247bc41123",
 "headers":{"x-api-key":"<same string>"},
 "payguardChannelProofUrl":"http://127.0.0.1:54365/get_alipay_agent_pay_token"}
```

  A `POST http://127.0.0.1:54365/mcp` with that `x-api-key` returns a valid MCP
  `initialize` and a `tools/list` of ~30 tools. **Read this file at runtime; do
  not hardcode the token or the port.**

* That local MCP server is JSON-RPC 2.0, **POST-only** (`GET` → 405
  `Allow: POST`), authenticated by `x-api-key`. `initialize` reports
  `serverInfo {name:"qw-builtin", version:"1.0.0"}`, protocolVersion
  `2025-06-18`. Tools include `qwenwork_task_{get_detail,cancel,send_message,submit_response}`,
  `qwenwork_cron_manage_job`, `qwenwork_channel_{list_conversations,delegate_to_im}`,
  `qwenwork_file_present_files`, `qwenwork_{image,video,music}_generate`,
  `qwenwork_media_task`, `qwenwork_pages_*`, `qwenwork_mcp_tool_{list,get,call}`,
  `qwenwork_skill_manage`, `skill_suggest`, `qoder_show_widget`,
  `qoder_load_widget_guide`, `memory`/`memory_search`/`memory_get`, `qw_query`,
  `qw_action`.

  🔴 **There is no chat/completions tool and no raw LLM tool.** The local MCP is a
  task/agent orchestrator, **not a model gateway** — it cannot replace the
  `gateway.qwenwork.cn` path. Do not try to route `Chat` through it.

* `%APPDATA%\QwenWorkCN\auth.dat` (371 B) and `auth-v2.dat` (1581 B) are `v10`
  Chromium OSCrypt blobs (only the headers were inspected).
  `%APPDATA%\QoderWork CN\auth.dat` (382 B) is an older sibling blob.
  `~\.qwenworkcn\.status.json`: `logged_in: true`, `user_type: personal`, plan
  个人免费版, `version 1.2.1`, `login_method: browser`, username `34292c1`.
* `~\.qwenworkcn\bin\dws.cmd --help` **fails**:
  `qwork-cli-shim: dws requires QwenWork session (no QODERWORK_SOURCE_CHAT_ID)`,
  exit 1. `dws` is a session-scoped tool shim injected into an agent turn, not a
  standalone CLI. Do not build on it.
* `relay-port.json` = `{"port":16799,"appName":"千问办公","pid":24944}`.
* Family: **QwenWorkCN is the product itself** (its `versions.json` key is
  `qoderwork`, and the worker's `BUSINESS_PRODUCT` is `qoder_work`) — i.e.
  QwenWork is a renamed QoderWork CN desktop agent. QoderWork CN is an older
  same-origin build; QoderWork is a stale international Electron app (no auth
  file, mtime 2026-06-06); QoderCN is a separate Qoder IDE (VS Code fork, no
  credential files). Only QwenWorkCN has a recent `auth-v2.dat`.
* **The daily check-in is not enabled for this account.** `GET
  https://gateway.qwenwork.cn/sash/api/v1/me/daily-check-in/status` with the real
  bearer credential answers **`404` with an empty body**, and the desktop log
  repeats `Dispatch null on initial sync for opt-in registration with no remote
  value {"namespace":"operations","key":"announcement:credits-growth-card"}` —
  the server has never pushed the `credits-growth-card` operation, so the vendor's
  own UI does not render the button either. `daily.gateway.qwenwork.cn` is a
  **different backend** (401 `INVALID_TOKEN` for the same credential), so the
  `QODER_ENV=test` host is not a way in. Consequence: the check-in success path
  cannot be exercised live here; only the `404` branch has been observed.
* The base host was pinned live, not guessed: the real token gets `200` from
  `https://gateway.qwenwork.cn/api/v1/userinfo` and the returned `id` matches the
  `uid` in `client2api`'s `data/qwenwork/accounts.json`; the desktop log carries
  30762 occurrences of `Resolved default OpenAPI domain {"domain":"gateway.qwenwork.cn"}`.
  The vendor derives that host in `out/main/chunks/constants-*.js`:
  `SASH_API_BASE_URL` override → else `QODER_ENV === "test" ? daily.… : https://<resolveOpenApiDomain()>`
  (`resolveOpenApiDomain()` reads `~/.qodercn/.env`, which does not exist here).

## Deliverables

* `clients/qwenwork/{qwenwork.go,config.go,cosy.go,device.go,upstream.go,sse.go,pool.go}`
* `clients/qwenwork/qwenwork_test.go` — offline tests: COSY signing (fixed key,
  fixed IV, fixed clock → deterministic expected signature), PKCE derivation,
  the agent-envelope body builder, SSE decoding, error/cooldown classification.
  Use `crypto/*` and table-driven fixtures; **no network**.
* `clients/qwenwork/README.md` — including an explicit provenance statement and
  the ban-risk note.

## Definition of done

* `go vet ./... && go build ./...` clean; `go test ./clients/qwenwork/...` green
  offline.
* `GET /v1/models` lists `pro`, `flash`, `qwen3.8-max-preview` from the fallback
  catalog.
* Either a live streaming chat succeeds against `gateway.qwenwork.cn`, or
  `Status()` says exactly which of {no credential, device flow not completed,
  upstream unreachable} is blocking.
