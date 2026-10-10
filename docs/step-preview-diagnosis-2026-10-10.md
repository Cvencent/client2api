# Step Preview diagnosis, 2026-10-10

The live deployment was neither stopped nor upgraded. Cline and OpenCode
source fixes described below have been implemented and covered by regression
tests. They take effect after the user installs an updated build.

## Cline: tools were silently dropped

WorkBuddy's stored assistant messages at 11:16:22, 11:16:50 and 11:17:46
(Asia/Shanghai) contain XML-like `<tool_call>` text rather than structured
function calls. The completed requests at 11:16:50 and 11:17:46 ended with
HTTP 200, `finish_reason=stop`, and no WorkBuddy stream error.

`clients/cline/body.go` treated omitted, null and blank `tool_choice` as
equivalent to `"none"`, so it removed the caller's declared tools.

A live comparison sent the same prompt and Read tool through the gateway:

| Tool choice | Result |
| --- | --- |
| Omitted | `stop`, `[DONE]`; model reported no Read function was available |
| `"auto"` | `tool_calls`, `[DONE]`; structured Read call with `diagnostic.txt` |

The source now preserves tools for the default choice and omits the empty
choice field. Explicit `"none"` still suppresses tools.
`TestBuildBodyKeepsToolsWithDefaultChoice` failed for all three default forms
before the fix, then passed. Existing explicit-auto and explicit-none tests
also pass.

The earlier `max_tokens=200` probe did exhaust its budget in reasoning, while
4000 allowed a complete answer. That is a separate reproducible behavior,
not proof that WorkBuddy's original requests had a small token limit.
WorkBuddy's custom-model code omits the limit when no explicit
`maxOutputTokens` is configured.

## OpenCode: local concurrency and upstream availability

| Probe | Observed result |
| --- | --- |
| Step Free, 12 sequential requests through the gateway | 12 HTTP 200 responses |
| Step Free, 6 concurrent requests through the gateway | 1 HTTP 200; 5 immediate local in-flight-limit 429 responses |
| Step Free, 3 concurrent requests directly to Zen | 2 HTTP 200 streams with `[DONE]` and finish reasons; 1 HTTP 429 with `server_error: Endpoint is unavailable` |

The deployed platform config has `max_in_flight_per_account=1`.
With only the anonymous account eligible, the gateway admits one active
request and rejects overlapping requests. This is a local admission limit.

The direct Step probe proves the upstream can also reject a request. Its
error describes endpoint availability, so the HTTP 429 alone does not
establish an account's requests-per-minute or daily quota.

An earlier six-concurrent MiMo probe reported HTTP 200 for every response,
including two streams taking about 112 seconds. It retained only the first
line, which was a keep-alive for those two streams. Those results do not
prove successful completion or a fixed upstream queue/concurrency rule.

The public OpenCode dev source contains IP/day and API-key/minute limiter
implementations. Free limits are read from server-side `ZEN_LIMITS`; the
handler also has a proxy inference path. The exact live Step Free daily
quota and concurrency threshold were not established by these samples.

## OpenCode OAuth: valid Console login, invalid inference credential

The stored OAuth access token successfully authenticated these Console calls:

- `/console/api/user`: HTTP 200.
- `/console/api/orgs`: HTTP 200.
- `/console/api/config`, with `x-org-id`: HTTP 200.

The workspace config was:

```json
{
  "config": {
    "enterprise": {"url": "https://opencode.ai/console"},
    "disabled_providers": ["opencode"],
    "provider": {}
  }
}
```

The same token sent to Zen inference as a Bearer credential returned
`AuthError: Invalid API key`. Repeating with the configured OpenCode
User-Agent, `x-opencode-org-id`, `x-org-id`, and client identification
headers did not change that outcome.

`clients/opencode/pool.go: authSpec` sends the Console access token directly
as the inference Bearer key. `clients/opencode/login.go` accepts an empty
workspace provider config and marks the login successful anyway.
The subsequent 401 is then handled by `noteFailure` as an auth failure,
which disables the local account.

Thus the observed local disabled flag does not demonstrate a vendor account
ban or expired login. The Console login is valid, but this workspace exposes
no usable Zen provider configuration, and the module is using the wrong
credential for its chosen inference endpoint.

The OAuth implementation now distinguishes Console login success from Zen
inference readiness and consumes workspace inference configuration/key instead
of treating the login token as a Zen API key.
For this account, Zen availability must first be enabled/configured in the
workspace; changing the User-Agent cannot supply that missing configuration.
No workspace settings, billing, credentials or live concurrency config were
changed during this diagnosis.

## Implemented OpenCode corrections

- Console login remains saved when Zen is disabled, absent or unconfigured.
  Health/status report not ready and explain why, without sending a login
  token to the default inference endpoint and disabling it after a bogus 401.
- Workspace inference configuration reads provider `api`, `options.apiKey`,
  `options.baseURL` and `options.headers`. Console-token environment references
  require an explicit workspace inference address distinct from public Zen.
  Per-model provider objects decode correctly and are excluded from this
  adapter's supported workspace model list.
- Refresh/test re-fetch workspace configuration for legacy accounts and clear
  stale keys/model ids after revocation. Refresh preserves manual enable state;
  previously disabled accounts require explicitly enabling after configuration.
- Local account saturation waits at most `queue_timeout` (default 60 seconds),
  honors cancellation and returns all pool/gateway leases on abandoned attempts.
  Retries share one queue deadline. Busy affinity accounts can rotate to
  another available account; an upstream failure is not repeatedly sent to the
  same account within one Chat call to force it into cooldown.
- Free requests skip Console accounts; anonymous accounts skip paid models.
- HTTP 429 with `server_error` is classified as upstream availability failure,
  not an authoritative account rate-limit verdict. One such failure does not
  park the account. Expired cooldowns show ready despite historical errors.
- Empty supported workspace model sets are not treated as unrestricted.
  Console-token endpoint checks normalize equivalent public Zen URL spellings.
- Error messages scrub both Console credentials and inference keys/headers,
  including transport errors, SSE error frames and token-refresh refusals.

## Verification

- `go test ./clients/cline -count=1`: passed.
- `go test ./...`: passed.
- `go build ./...`: passed.
- `go test -race ./clients/opencode -count=1`: passed.
- Independent read-only review findings were reproduced in tests and corrected.
- No installer was built or executed.

## Evidence locations

- `D:\client2api-src\clients\cline\body.go`
- `D:\client2api-src\clients\cline\cline_test.go`
- `D:\client2api-src\clients\opencode\pool.go`
- `D:\client2api-src\clients\opencode\login.go`
- `D:\client2api-src\clients\opencode\error.go`
- `D:\client2api\configs\client2api.json`
- `C:\Users\vencent\.workbuddy\logs\2026-10-10\2026-10-07-11-08-06__aa3947634478162bcee813a2164ab661.log`
- `C:\Users\vencent\.workbuddy\projects\c-Users-vencent-WorkBuddy-2026-10-07-11-08-06\2c536e4a-76c4-4948-afda-83eccbb98924.jsonl`

Public reference sources inspected:

```text
https://opencode.ai/docs/zen/
https://raw.githubusercontent.com/anomalyco/opencode/dev/packages/console/app/src/routes/zen/util/handler.ts
https://raw.githubusercontent.com/anomalyco/opencode/dev/packages/console/app/src/routes/zen/util/ipRateLimiter.ts
https://raw.githubusercontent.com/anomalyco/opencode/dev/packages/console/app/src/routes/zen/util/keyRateLimiter.ts
https://raw.githubusercontent.com/anomalyco/opencode/dev/packages/console/core/src/subscription.ts
```
