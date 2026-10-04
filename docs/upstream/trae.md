# upstream spec — trae

**Route:** `trae/<model>` · **Licence of the reference:** MIT (port freely)

## Reference implementations (on disk, read them)

| Path | What |
|---|---|
| `client2api-lab\_upstream\trae2api-web\` | **Go 1.22, MIT, pure stdlib (go.mod has no `require`)** — port this |
| `client2api-lab\_upstream\trae2api\` (wangqi233) | **Node 22.5+, MIT — use as the written SPEC**; `docs\PROTOCOL.md`, `src\trae-decrypt.js` |

`trae2api-web` HEAD `3aabdaa5b02037469a8b019cf1c41d6261a87f49`. ~4.6K lines
`internal/` + ~0.6K `cmd/`, ~1.1K lines of tests. Key files:

* `internal/upstream/client.go` (412 lines) — `New`@110-125, `ErrKind`@21-48,
  `Classify`@64-92, `RefreshToken`/`refreshLocked`@148-221,
  `ChatStream`@236-261, `FetchModels`@272-320, `Checkin`@323-353,
  `EntUsage`@363-400, `GetUserInfo`@403-431
* `internal/upstream/solosse.go` (430) — SSE frame parsing
* `internal/upstream/payload.go` (183) — body rewriting
* `internal/upstream/headers.go` (58), `constants.go` (23)
* `internal/auth/auth.go` (253), `internal/pool/pool.go` (350)
* `internal/server/{handler.go,login.go,callback.go,admin.html}`

## Upstream

* Chat: `POST https://trae-api-cn.mchost.guru/api/agent/v3/llm_utils_chat`
* Models: `POST https://trae-api-cn.mchost.guru/api/ide/v1/get_detail_param`

The **channel is chosen by the body field `function`**:

* `solo_work_lite` → TRAE SOLO
* `chat_v3` → classic Trae CN

The two catalogs are **mutually exclusive** (38 vs 51 names, only 3 shared).

## Headers (`internal/upstream/headers.go:14-45`)

```
Content-Type: application/json
Accept: text/event-stream
User-Agent: Trae/0.1.52
Authorization: Cloud-IDE-JWT <accessToken>
X-Cloudide-Token / X-Ide-Token / X-Uid
X-App-Id: 6eefa01c-1036-4c7e-9ca5-d891f63bfcd8
X-App-Version / X-App-Version-Code
X-Ide-Version: 0.1.52 / X-Ide-Version-Code: 20260811 / X-Ide-Version-Type: stable
X-Device-Type: windows / X-OS-Version: Windows 11 Pro / X-Device-Brand: 83DG
Request-Traffic-Type: prod
X-Machine-Id / X-Device-Id
```

`trae2api`'s `src/auth.js:1162-1205` documents a superset: also
`x-custom-trace-id`, `x-flow-traceparent: 04-<traceId>-<spanId>-01`,
`x-device-cpu`, `X-Request-ID`, `X-Trae-Request-ID`, `Last-Event-ID`. The SG
variant adds `X-User-Region: CN` (note: the field is named "User-Region" but the
value observed is `CN`).

**Version divergence is unresolved.** `trae2api-web` hardcodes
`X-Ide-Version 0.1.52 / 20260811`; `trae2api` sends the IDE manifest version
(3.3.67 CN / 3.5.51 SG). Make every one of these **configurable** and confirm
with one live request. Do not silently pick one.

## Body rewriting (`internal/upstream/payload.go:21-193`)

* force `stream = true` (the upstream always streams)
* force `function = solo_work_lite` for the SOLO channel
* **every message's `content` must be an array**:
  `"hi"` → `[{"type":"text","text":"hi"}]`.
  A bare string yields HTTP 400 + code 4001
  (`cannot unmarshal string into LLMRawMessageContent`).
* assistant `tool_calls[].function` → `function_call`
* write the model into **both** `model` and `config_name` (default `glm-5.2`)
* `tools[].function.parameters` **must be a JSON string, not an object**
* normalise `tool_choice`: `none|auto|required|function`

## Streaming

Real frames (`solosse.go:1-23`):

```
id:1 event:metadata   {"model":"","session_id":"...","prompt_completion_id":0}
id:2 event:timing_cost {"name":"llm_raw_chat_v2"}
event:output      {"response":"<delta>","reasoning_content":"<think delta>","tool_calls":null|obj}
event:extra_info
event:token_usage {"prompt_tokens":21,"completion_tokens":142,"total_tokens":163,"reasoning_tokens":135}
event:done        {"finish_reason":"stop"}
event:error       {code,message}
```

**`event:error` arrives inside an HTTP 200 SSE stream.** You must parse it, not
look at the status code. `notify_usage` carries `billing_mode: credits` and
`cn_credits_remain_info{ide_credits, work_credits}` — surface the credits in
`Status().Accounts[].Extra`.

**Even `stream:false` returns SSE.** There is no buffered mode upstream.

## Error taxonomy

| code | meaning | handling |
|---|---|---|
| 1005 | plan_limit | **12h cooldown. NEVER in the failover set.** |
| 401 | session_dead | refresh |
| 429 | soft_rate | 60s cooldown |
| 1001 | auth | failover |
| 4001 | param invalid | failover |
| 4008 | quota | failover |
| 4010 | (see `Classify`) | failover |
| 4011 | rate limit | cooldown |
| 9074 | checkin contention | retry later |
| 404 | not_found | do not retry |
| 5xx | server | retry/failover |

**Failover set = {4008, 1001, 4010}.** 1005 must not be in it.

## Token refresh

```
POST <ApiHost|OAuthHost>/cloudide/api/v3/trae/oauth/ExchangeToken
{ "ClientID": ..., "RefreshToken": ..., "ClientSecret": "-", "UserID": "" }
```

→ returns a **rotated** refresh token. You must persist the new one atomically.

* `client.go:226-231` normalises milliseconds → seconds.
* `client.go:171-221`: on failure **never mutate the stored fields**, so the old
  refresh token can still be retried.
* `trae2api` defaults `TRAE_POOL_SELF_RENEW=off` because self-renewal rotates the
  token family and can log the desktop app out. Mirror that: **self-renew off by
  default**, opt-in via config.

## OAuth web login (optional; for the README)

`internal/server/login.go:1-11` builds a login URL and captures the
`/authorize` callback on a **second `http.Server` on `127.0.0.1:18080`**,
reusing the same handler. `callback.go:35-39` builds:

```
https://www.trae.cn/authorization?client_id=en1oxy7wnw8j9n&redirect=0&...
```

then ExchangeToken + GetUserInfo + atomic persist. `login.go:178-179` stores
Domain `trae.cn` / ApiHost `https://api.trae.com.cn`.

⚠️ No OAuth `state`/`nonce` binding was observed → the `/authorize` endpoint is
a CSRF hole (inference, not verified). If you implement login, add `state`.

## tc decryption (`trae2api\src\trae-decrypt.js`, 174 lines)

Container `[6B header][32B random][ciphertext]`, plaintext
`[64B sha512 hash][payload]`. Header `74 63 05 10 00 00` = AES,
`12 57 20 20 02 03` = AES_PRIVATE. key/IV =
`sha512(sha512(random) XOR salt)[0:16]/[16:32]` with **four hardcoded 64-byte
salts**. AES-128-CBC. Source key: `<dataDir>/User/globalStorage/storage.json`,
property `iCubeAuthInfo://icube.cloudide`.

Implement in Go with `crypto/sha512` + `crypto/aes` (~200 lines). Note the
`sha512(random)` is of the *raw 32 bytes*; the XOR is byte-wise against the salt.

## Live credentials on this machine

| File | State |
|---|---|
| `%APPDATA%\Trae CN\User\globalStorage\storage.json` (13936 B, mtime 2026-09-27 10:45:26) | **access token valid to 2026-10-03T18:33:21.390Z**, refreshToken to 2027-03-18, userId `3595881099822378`, host `https://api.trae.cn`, region CN, scope marscode |
| `%APPDATA%\TRAE SOLO CN\User\globalStorage\storage.json` (10289 B) | same account, **access token expired 2026-08-08**, refreshToken valid to 2027-01-21 → refreshable |

Also present: `telemetry.machineId` (64 chars, `cc42f8b3…`),
`telemetry.devDeviceId` `14fa1417-379e-407e-8abb-db063f76669d`,
`iCubeAuthInfo://icube-dc:2235771921399404`.

**Negative evidence — do not waste time:** `~\.trae-cn\trae-jwt-token` (925 B,
`iss=trae`, exp 2026-10-03) is **not** a chat token; its only reader is
`~\.trae-cn\builtin_skills\TRAE-generate-mini-app\scripts\preview-server.js`.
`%APPDATA%\Trae` and `%APPDATA%\TRAE` storage.json are 589 B 2025-03-03 shells.
`~\.trae` and `~\.trae-aicc` hold no credentials.

## Device fingerprint — fragile, read this

A **forged or unregistered device id is silently dropped**: TLS connects but the
upstream never responds (see `trae2api\src\auth.js:1163-1168`). You must reuse
the real `machineId` / `devDeviceId` from the local storage.json. Make them
config-overridable but default to discovery.

## Scope

`trae2api-web` only covers **CN SOLO, single realm, single product**. That is
acceptable for v1. `chat_v3` (classic Trae CN) is a documented second channel —
add it behind config if cheap, otherwise leave a TODO.

Not done in either reference: `create_agent_task`. TRAE Work channel is not
reproducible. `extract-token.sh` references
`scripts/log-analysis/extract-completion-jwt.js`, which is **not in the repo**.

## Deliverables

* `clients/trae/{trae.go,auth.go,pool.go,upstream.go,payload.go,sse.go,tcrypt.go}`
* `clients/trae/trae_test.go` — offline tests for payload rewriting (especially
  the content-array and parameters-as-string rules), the SSE frame parser
  (including `event:error` inside a 200), the error classifier + failover set,
  and the tc container decryptor against a recorded fixture.
* `clients/trae/README.md`

## Definition of done

* `go vet ./... && go build ./...` clean; `go test ./clients/trae/...` green
  offline.
* `GET /v1/models` lists the SOLO catalog (with an offline fallback).
* Live streaming and non-streaming chat succeed against
  `https://trae-api-cn.mchost.guru` using the `Trae CN` credential, or
  `Status()` says precisely why not.
