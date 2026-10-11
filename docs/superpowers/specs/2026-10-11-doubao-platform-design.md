# Doubao Platform Module - Design

**Date:** 2026-10-11
**Status:** Approved for implementation

## Goal

Add a `doubao` client module so the gateway can serve the mainland Doubao
(豆包) consumer product through the same OpenAI-compatible surface as every
other platform. The operator signs in through a **web login** flow (scan the
QR code / enter a phone number inside a browser this module drives), not by
pasting a cookie by hand. The module exposes the same account-management,
balance, health, stickiness and status capabilities the other modules expose,
but only where Doubao really supports them.

## Background and verified facts

- The Doubao desktop client (2.31.4) is a Chromium-family shell around the
  `www.doubao.com` web app; its bundle is the authoritative description of the
  web API this module speaks.
- Verified from the on-disk bundle: the chat call is
  `POST https://www.doubao.com/samantha/chat/completion` with
  `Content-Type: application/json`, `Agw-Js-Conv: str`,
  `credentials: same-origin`, and a JSON body of `{messages, completion_option}`.
  The bundle's common-header builder is literally `u9 = () => ({})`, so no
  `a_bogus`/`X-Bogus` request signature is required; authentication is the
  session cookie jar. A repo-wide and app-wide search for `a_bogus` finds
  nothing in the client assets.
- Credentials are `.doubao.com` cookies (`sessionid`, `sessionid_ss`,
  `sid_guard`, `sid_tt`, `uid_tt`, `d_ticket`, `ttwid`, `msToken`, ...). They are
  HttpOnly, so only a browser (or a CDP client attached to one) can read them.
- Verified endpoints used by this module:
  - chat: `/samantha/chat/completion`
  - account probe: `/alice/user/get_account_info`
  - credits: `/commerce/benefit_supply/credit/get_credit_num_optional_tasks`,
    `/commerce/benefit_supply/credit/get_credit_usage_history`
  - login pages: `/passport/web/user/login/`, `/passport/web/get_qrcode/`
- Project rules: only `D:\client2api-src` is edited; the running service at
  `D:\client2api` is never stopped, restarted or installed over. Credentials
  never appear in logs, `Status`, panel responses or error text; all
  human-facing strings pass through `core.Redact`.

## Capability scope

Implemented because Doubao supports them:

| Capability | Basis |
|---|---|
| `core.Client` (chat, models, status) | `/samantha/chat/completion` SSE in the bundle |
| `LoginProvider` (web login) | controlled Chromium + CDP cookie read |
| `AccountManager` | cookie sessions are listable/addable/removable/testable |
| `BalanceProvider` | `/commerce/benefit_supply/credit/*` |
| `ModelRefresher` | catalogue is config/account-derived and re-readable |
| `HealthProvider`, `PoolStatsReporter` | pool state exists |
| `Reviver` | cooldown/penalty state exists |
| `ConversationBinder` | sticky conversation->account binding |

Deliberately not implemented, so the panel hides the control rather than
offering a button that cannot work:

- `CredentialImporter` (local desktop-client import): needs SQLite + Windows
  DPAPI + AES-GCM and the module ships no SQLite dependency. The web login
  replaces it.
- `CheckinProvider`/`TaskProvider`/`TaskClaimer`/`TaskAutoRunner`: the
  task-centre endpoints need device-side context that is not confirmable
  offline.
- `SMSProvider`/`AutoLoginProvider`: no one-time-SMS platform integration.
- `PackageProvider`/`VoucherProvider`: not confirmed for this product.

## Architecture

```
internal/browser (shared CDP driver)
        ^  Page.Cookies(ctx, urls)   <- new, narrow, additive
        |
clients/doubao
  config.go      schema + defaults + example
  doubao.go      Client, New, Name, Models, Status, capability assertions
  accounts.go    store (accounts.json/state.json) + AccountManager + pool
  login.go       LoginProvider state machine
  browserlogin.go  controlled-browser launch, poll, cookie capture
  web.go         HTTP transport: common query params, cookie header, JSON/SSE
  chat.go        /samantha/chat/completion -> core.Stream
  models.go      catalogue + RefreshModels
  balance.go     credits -> core.Balance
  health.go      Health + PoolStats
  revive.go      Reviver
  affinity.go    conversation->account stickiness (ConversationBinder)
  README.md      behaviour and config documentation
  config.example.json
```

`clients/all/all.go` gains exactly one blank import.

### Shared change: `internal/browser.Page.Cookies`

```go
type Cookie struct {
    Name     string
    Value    string
    Domain   string
    Path     string
    Expires  float64
    HTTPOnly bool
    Secure   bool
}
func (p *Page) Cookies(ctx context.Context, urls []string) ([]Cookie, error)
```

It calls `Network.getCookies` (falling back to `Storage.getCookies` when the
URL list is empty) over the existing flattened session. It does not alter any
existing login flow and touches no other file.

## Data flow

### Web login

1. Panel -> `POST /panel/api/clients/doubao/login`.
2. `StartLogin` launches a throwaway Chromium profile (Edge/Chrome via
   `browser.FindBrowser`) with a visible window on `https://www.doubao.com`, then
   returns `{state: pending, url, message}`. `LocalApp` is false, so the panel
   keeps its ordinary waiting rendering and polls.
3. The operator scans the QR code / enters the phone number in that window.
4. `PollLogin` calls `Page.Cookies` for `https://www.doubao.com/`, requires the
   core session cookies, and verifies them with `GET /alice/user/get_account_info`.
5. On success the cookie jar is stored as one account in `accounts.json`, the
   throwaway browser is closed and the state becomes `success`.
6. If the browser cannot be launched the state stays `pending` with a message
   naming the manual fallback (paste the cookie in the account form).

### Chat

1. Gateway strips `doubao/` and calls `Chat(ctx, req)`.
2. The pool picks an enabled, non-cooled account; `core.NoteServedBy` records it
   and `req.AcquireAccountSlot` enforces the per-account ceiling.
3. The module builds `{messages, completion_option}` and POSTs to
   `/samantha/chat/completion` with the cookie header plus the common query
   params.
4. The SSE reader maps vendor frames (CMPL -> delta, FIN -> done, ERR -> error)
   to `core.Event`s, emitting usage when the frame carries it.

## Error handling

- No account -> `core.ErrNotConfigured` (503).
- Cookie rejected (login redirect or 401) -> park the account `invalid`.
- Rate limit / WAF -> park `cooling` for the configured cooldown.
- Transport errors -> the shared degraded/breaker path.
- All panel-visible strings pass through `core.Redact`.

## Testing

- `internal/browser`: a unit test that `Cookies` issues the right CDP command
  and decodes the reply over a fake CDP server (no real browser).
- `clients/doubao`: pure-unit tests for config defaults, store round-trip,
  cookie->account mapping, request-body construction, SSE frame parsing, error
  classification, and `Status`/`Health` on an empty and a populated pool.
  Browser launch is injected, so no test starts a real browser.
- `go build ./...` and `go test ./...` must pass with the offline toolchain.

## Rollout

Source-only change: no version bump and no installer unless the user asks for a
package. `README.md` gains a Doubao section; `clients/doubao/README.md`
documents configuration and behaviour.
