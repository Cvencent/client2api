# doubao (Doubao)

Doubao is ByteDance's mainland consumer assistant. The desktop client is a
Chromium-family shell around the `www.doubao.com` web app; this module speaks
that same web API, so the gateway can route `doubao/<model>` requests through a
Doubao account.

## How it authenticates

The credential is the browser's own session cookie jar (`sessionid`,
`sessionid_ss`, `sid_guard`, `ttwid`, `msToken`, ...). These cookies are
HttpOnly, so no page script can read them and the vendor never hands them out
over an API. There are therefore two ways to get one in:

1. **Web login (preferred).** The account page's "web login" button launches a
   throwaway Chromium profile (Edge or Chrome) on `https://www.doubao.com`.
   Scan the QR code or enter a phone number in that window; the module reads the
   session cookies back over CDP, verifies them against the account endpoint, and
   stores them. The browser is closed once the login succeeds.
2. **Paste a cookie.** Copy the Cookie request header from a signed-in
   `www.doubao.com` tab and paste it into the account form's 会话 Cookie field.
   Use this when no Chromium-family browser is installed.

Nothing in this module reads the Doubao desktop client's own cookie database:
that would need SQLite plus Windows DPAPI and AES-GCM decryption, which the
module deliberately does not ship. The web login replaces it.

## Capabilities

| Capability | What it does |
|---|---|
| Chat | `POST /samantha/chat/completion`, streamed to the OpenAI surface |
| Web login | controlled browser + CDP cookie read (`LoginProvider`) |
| Account management | list / add / enable / disable / test / remove |
| Balance | credits from `/commerce/benefit_supply/credit/*` |
| Health + pool stats | standard panel reporting |
| Revive | clears a local cooldown/park |
| Conversation stickiness | pins a conversation to the account that first served it |

Not implemented, so the panel hides the control rather than offering a button
that cannot work: local-client import, check-in, the task centre, SMS / auto
login, and package/voucher reporting.

## Configuration

`clients.doubao` in `configs/client2api.json`:

```json
{
  "web_base": "https://www.doubao.com",
  "browser_path": "",
  "browser_headless": false,
  "login_timeout_seconds": 600,
  "request_timeout": "30s",
  "chat_timeout": "5m",
  "probe_timeout": "20s",
  "cooldown": "60s",
  "auth_cooldown": "30m",
  "models_ttl": "30m",
  "chat_bot_id": "",
  "models": [],
  "cookie": "",
  "label": "",
  "accounts": []
}
```

Every key is optional; the defaults are exactly the values above. `cookie` and
`accounts[].cookie` are a fallback for a headless host with no browser, but the
web login is the intended path. Environment overrides:
`CLIENT2API_DOUBAO_WEB_BASE`, `CLIENT2API_DOUBAO_BROWSER_PATH`,
`CLIENT2API_DOUBAO_CHAT_BOT_ID`, `CLIENT2API_DOUBAO_COOKIE`.

The model catalogue is the platform's own model ids; add more under `models`.

A vendor rejection marks the session `invalid` for `auth_cooldown`; the row
shows the retry deadline, and the cooldown expiry clears the verdict and returns
the account to rotation. A transient failure uses the shorter `cooldown`
instead. `revive` clears either park immediately.

## Notes

- No request signature (`a_bogus`/`X-Bogus`) is required: the desktop bundle's
  own common-header builder is empty and authentication is the cookie alone.
- Credentials are stored only under the module's data directory and never appear
  in logs, `Status`, the panel, or error messages.
