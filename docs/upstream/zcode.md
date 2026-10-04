# upstream spec — zcode

**Route:** `zcode/<model>` · **Licence of the reference: AGPL-3.0**

⚠️ **This module must be a clean-room Go implementation.** AGPL-3.0 is viral: any
line copied into our binary makes the whole project a derivative work. Read the
Python/JS references to learn the *protocol*, then write Go that you own. Do not
copy code, comments, identifiers or file structure. Record what you observed in
the README so the provenance is auditable.

## Reference implementations (on disk, READ ONLY)

| Path | What |
|---|---|
| `client2api-lab\_upstream\zcode2api\` (liu5269, ★255, AGPL-3.0) | `app/{settings,agent,oauth,store,captcha,quota,identity,fingerprint,body_transform,openai_compat}.py`, `app/routes/{gateway,admin_api,pages}.py`, `captcha_node\solver.js`, `docs\ARCHITECTURE.md` |
| `client2api-lab\zcode2api-lab\SQMY-dor\` | same shape + `data\accounts.db` + `probe_upstream.py`; **its jsdom captcha solver is the only one that works on this machine** |
| `client2api-lab\zcode2api-lab\dengyie\` | FastAPI rewrite; has `POST /v1/chat/completions` (OpenAI↔Anthropic, incl. SSE) |
| `client2api-lab\zcode2api-lab\ZCode反代API-可行性报告.md`, `ZCode反代API-部署成功报告.md` | field reports (but see the region warning below) |

## Upstream

* **Primary (JWT + captcha):**
  `POST https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages`
* Fallbacks: `https://api.z.ai/api/anthropic/v1/messages`,
  `https://open.bigmodel.cn/api/anthropic/v1/messages`
* **Wire protocol is Anthropic Messages (`/v1/messages`)**, not OpenAI. We
  expose OpenAI on our side, so you translate OpenAI → Anthropic → OpenAI.
  The `dengyie` tree has a worked two-way translator including SSE — read it for
  the *mapping rules* only.
* SSE frames are standard Anthropic: `message_start`, `ping`,
  `content_block_start`, `content_block_delta`, `message_delta`, `message_stop`.
  Usage appears as `cache_read_input_tokens`. Thinking deltas carry a
  `signature`.
* There is a **second, desktop schema family** `openai:chat` on
  `open.bigmodel.cn/api/coding/paas/v4` and `api.z.ai/api/coding/paas/v4`.

## Credentials — two channels

### (a) JWT channel (captcha required)

`%USERPROFILE%\.zcode\v2\credentials.json` (observed live: mtime 2026-09-28
17:35:25) holds:

* `zcodejwttoken` — a **204-character JWT**. Header + payload
  `{user_id, token_version, sub, iat}`, **no `exp` claim** — the server decides
  lifetime. iat observed `1790588088`.
* a 335-char `oauth:bigmodel:access_token`
* an `enc:v1:`-prefixed coding-plan api-key

**Every request on this channel needs a fresh Aliyun traceless-verification
param.** The captcha solver is Node + jsdom. On this machine only the SQMY-dor
solver works (`dengyie`'s happy-dom solver is broken: exit 2 / pe-stall).

The reference shells out to Node. Decide deliberately: either (1) shell out to a
Node solver like the reference, documenting the Node dependency and vendoring
the solver **as our own clean-room JS** (the AGPL solver may not be copied), or
(2) implement the signature/param derivation in Go. Prefer (2) if the algorithm
is tractable; otherwise (1) but keep it strictly optional — the module must
still work on the API-key channel with Node absent.

Captcha success rate is only ~2/3–4/6 and it is brittle: when Aliyun changes the
fingerprint logic it breaks. **It is a fallback, not the default.**

### (b) API-key channel (no captcha) — prefer this

`open.bigmodel.cn/api/anthropic` or `api.z.ai/api/anthropic`, authenticated with a
32-hex key. `app/oauth.py` L18-42 shows how to obtain one:

* `POST /api/v1/oauth/cli/init` → `authorize_url`
* `GET /oauth/cli/poll/{flow_id}`
* L44-100 exchanges an access token at `api.z.ai`:
  `api/auth/z/login` → `getCustomerInfo` → `org/project api_keys` →
  `copy/{key}`.

Note this yields a **Z.AI API key, not a JWT**.

🔴 **Security: during recon a live 32-hex bigmodel API key was printed in clear
text into this session's logs. That key must be rotated.** Do not reuse it, do
not hardcode it, do not commit it.

## Mandatory request injection

The request body **must** carry the official CLI-Prefix / Agent-Identity /
Environment system blocks (they live in the reference's `app/zcode_system.json`).
Without them the upstream answers **3012**. Transcribe the *content* of those
blocks into our own config/asset file; do not copy the file itself.

## Headers

`X-ZCode-App-Version`, `X-ZCode-Agent`, `X-Title`, `X-Platform`,
`X-Release-Channel`, `X-Client-Language`, `X-Client-Timezone`, `X-Os-Category`,
`X-Os-Version`, `X-Device-Mid`, `X-Aliyun-Captcha-Verify-Param`,
`X-Aliyun-Captcha-Verify-Region`.

**On the start-plan / JWT channel, send only** `x-request-id`,
`x-zcode-session-type`, `x-zcode-trace-id`. Sending `x-query-id` or
`x-session-id` as well triggers **3012 risk control**.

## Error envelope

```json
{"code": 3006, "message": "..."}
```

| code | meaning | HTTP |
|---|---|---|
| 3006 | model not allowed | 200 |
| 3007 | captcha verify failed | 400 |
| 3009 | concurrency exceeded | 200 |
| 3012 | risk control / unusual activity | 405 |

Map 3006 → `core.ErrUnsupported` (the model genuinely is not allowed);
3007 → captcha retry; 3009 → short cooldown + retry; 3012 → back off hard and
mark the account cooling (risk score accumulates).

## Models

**Working:** `GLM-5.3`, `GLM-5.3-Flash`.
**Rejected with 3006:** `GLM-5.2`, `GLM-5-Turbo`, `glm-4.7`.

🔴 **Do not hardcode the region.** The two Chinese field reports both say `cn`,
but a live `GET client/configs` returned `region: "sgp", enabled: true`. The
region is dynamic — **read it from the endpoint at runtime**, never from a
constant or a config default.

## Concurrency

3009 means the concurrency slots are full. Observed: when the desktop app holds
GLM-5.3, only GLM-5.3-Flash stays usable. Keep the slot limit configurable and
back off on 3009 rather than failing the caller.

## Storage (reference schema, for orientation only)

SQLite `data/accounts.db` (WAL):

```sql
accounts(id TEXT PK, provider, name, mode, status, enabled, created_at REAL, data JSON)
meta(key, value)   -- admin_key (default 'zcode'), gateway_key, quota_refresh
```

`mode` ∈ `jwt|api_key`, `status` ∈ `active|exhausted|cooling|invalid`.
The reference loads account state into memory **only at process start**, so a DB
edit needs a restart. We can do better: JSON files via
`core.WriteJSONAtomic`, loaded lazily with a mutex — no restart needed.

Panel login default password in the reference is `zcode`. **We do not ship a
per-module panel**; the core panel covers everyone. Do not invent credentials
for our panel.

## Live evidence on this machine

* 12 ZCode Electron processes running.
* **`SQMY-dor` gateway is running**: PID 38236, `python main.py serve --port 3000`,
  started 19:28:45. `GET 127.0.0.1:3000/v1/models` → 200, returns GLM-5.3 and
  GLM-5.3-Flash. Its logs show successful `model.request.completed` against
  `zcode.z.ai/api/v1/zcode-plan/anthropic` with no 3012.
* ZCode CLI logs likewise show successful plan-channel requests.

👉 **You may point at `127.0.0.1:3000` as a stopgap to prove our translation
layer end-to-end without touching the captcha path.** Make that an explicit,
off-by-default config option (`"upstream_base"`), never a silent fallback.

## Risks to document in the README

* captcha fragility (~2/3 success, breaks on any Aliyun fingerprint change);
* 3012 risk-score accumulation — the recon recommends using a throwaway account;
* 3009 concurrency slots;
* the JWT has **no `exp`** — expiry is server-side and silent;
* quota window observed expiring 2026-09-28 23:59;
* the leaked API key (rotate);
* AGPL provenance — this file is the audit trail.

## Deliverables

* `clients/zcode/{zcode.go,config.go,auth.go,pool.go,anthropic.go,sse.go,body.go,system.go}`
* `clients/zcode/zcode_test.go` — offline tests: OpenAI↔Anthropic body mapping,
  Anthropic SSE → `core.Event` decoding, error-code classification, credential
  load/save, region-from-endpoint (with a fake transport).
* `clients/zcode/README.md` — including an explicit **provenance statement**
  (what was read, what was written from scratch).

## Definition of done

* `go vet ./... && go build ./...` clean; `go test ./clients/zcode/...` green
  offline with **no Node required**.
* `GET /v1/models` lists GLM-5.3 / GLM-5.3-Flash.
* A live streaming and non-streaming chat succeed — via the API-key channel if
  one is available, otherwise via the documented local stopgap — or `Status()`
  states precisely what is missing.
