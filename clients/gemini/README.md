# Gemini (Google Cloud Code Assist)

`gemini/<model>` wraps the free line used by Google's Antigravity IDE. It is a
native Google account pool: credentials are Google OAuth tokens, the model is
served through Cloud Code Assist, and quota is the vendor's rolling 5-hour and
weekly windows rather than a credit balance.

## Configuration

```json
{
  "clients": {
    "gemini": {
      "project": "",
      "login_port": 0,
      "login_timeout": "6m",
      "chat_timeout": "10m",
      "quota_timeout": "30s",
      "cooldown": "60s",
      "quota_cooldown": "12h",
      "max_in_flight": 2,
      "extra_headers": {}
    }
  }
}
```

| key | type | default | meaning |
|---|---|---|---|
| `project` | string | `""` | Override the project id. Empty probes `loadCodeAssist`, then falls back to `aicode-consumers`. |
| `client_id` / `client_secret` | string | required | OAuth client registered for this deployment. Can also be supplied as `CLIENT2API_GEMINI_CLIENT_ID` / `CLIENT2API_GEMINI_CLIENT_SECRET`; the repository deliberately carries neither a default id nor secret. |
| `login_port` | int | `0` | Loopback callback port. `0` asks the kernel for a free port. |
| `login_timeout` | duration | `6m` | How long a browser sign-in may wait for the callback. |
| `chat_timeout` | duration | `10m` | Per-request chat deadline. |
| `quota_timeout` | duration | `30s` | Quota-window probe deadline. |
| `cooldown` | duration | `60s` | Account cooldown after a transient upstream failure. |
| `quota_cooldown` | duration | `12h` | Account cooldown after a quota refusal. |
| `max_in_flight` | int | `2` | Per-account concurrent request ceiling. |
| `extra_headers` | object | `{}` | Additional upstream headers. Identity and authentication headers are reserved. |

## Accounts

The Accounts view signs in with the browser OAuth flow and stores tokens in
`data/gemini/accounts.json`. `RefreshAccount` renews an expired OAuth token, and
`AccountBalance` reports the 5-hour and weekly quota windows. The module also
accepts the usual persisted account shape in `clients.gemini.accounts` for
bundle imports.

## Routing and wire format

The served model is `gemini/gemini-3.8-flash`. The upstream admission key must
carry an effort tier: `gemini-3.8-flash-low`, `gemini-3.8-flash-medium`,
`gemini-3.8-flash-high`, or `gemini-3.8-flash-tiered`. A bare
`gemini-3.8-flash` is not a valid upstream id.

Requests accept the normal OpenAI chat shape plus `reasoning_effort`. Thinking
is always requested (`includeThoughts: true`), because the vendor bills the
thought budget regardless. The module streams Cloud Code SSE back as the
gateway's OpenAI stream and preserves thought signatures across tool turns.

Identity headers are fixed to the Antigravity client shape; no `x-goog-api-key`
or `x-goog-api-client` header is sent. Context window is advertised as
`1,000,000`, maximum output as `64,000`.

## Notes

* This repository does not embed a Google OAuth client. Set `client_id` and
  `client_secret` in the Gemini module config, or provide the two environment
  variables above, before using browser sign-in or token refresh.
* Sign-in opens Google's consent page in the default browser and waits for the
  loopback callback. A headless install needs an operator-supplied token import
  instead.
* A quota refusal parks that account for `quota_cooldown` and tries the next
  account. Session affinity keeps a conversation on one account while it is
  healthy.
