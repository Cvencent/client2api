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
| `client_id` / `client_secret` | string | embedded | Override for the Cloud Code OAuth client. Left empty, the module signs in with the IDE's own installed-app client, so browser sign-in works on a stock install; `CLIENT2API_GEMINI_CLIENT_ID` / `CLIENT2API_GEMINI_CLIENT_SECRET` are the environment spelling of the same override. |
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

## Models

The catalogue is live, not compiled in. The module asks the vendor's own
`POST /v1internal:fetchAvailableModels` for the account's project and serves
what comes back, so a model Google adds or retires needs no code change. When
that call is refused the module falls back to the ids named by
`POST /v1internal:retrieveUserQuota`; when both fail it keeps the last good
list; with no credential at all it serves the compiled-in `gemini-3.8-flash`
row. Every row carries `extra.source` (`upstream` or `fallback`), and rows
discovered through the quota fallback also carry `extra.discovery: "quota"`.

* `Models` answers from a five-minute TTL cache and never makes the caller wait
  on the network: a stale cache starts exactly one background refresh, and the
  panel's `Status` poll triggers the same refresh, so the list is live within
  seconds of startup. A failed refresh is throttled for the same TTL.
* The panel's refresh button calls `RefreshModels`, which bypasses the TTL. A
  failed refresh reports the error and still returns the last good list, so the
  picker never goes blank.
* Ids whose last word is `-low` / `-medium` / `-high` fold into one row: the
  base id, advertised with `supported_efforts` and `default_effort`. A
  `-tiered` id stays its own row because the vendor serves it as its own model.
* Rows the vendor marks `disabled`, and the IDE's own `chat_*` / `tab_*`
  helpers, are filtered out. The vendor's `agentModelSorts` order is preserved
  for the rest of the list, which is then sorted by id.

## Routing and wire format

A request's model id must be in the catalogue. `gemini-3.8-flash` stays
admissible even after a live refresh, so an existing client does not break when
the vendor renames its list; with no catalogue yet it behaves as before, with
the tier suffix on the wire (`gemini-3.8-flash-low`, `-medium`, `-high`,
`-tiered`).

For a folded effort family the row is the base id and `reasoning_effort`
picks the variant that goes on the wire. With no usable effort the module sends
the vendor's own bare id when the catalogue lists one, else the `high` variant
(else the highest the family has). A `-tiered` row is forwarded verbatim and
never carries a `thinkingBudget`; so is any other catalogue id, with a budget
attached only when the vendor's metadata advertises `supportsThinking` for it
or when the id itself already names a level (`...-high`, `...-medium`).

Requests accept the normal OpenAI chat shape plus `reasoning_effort`. Thinking
is always requested (`includeThoughts: true`), because the vendor bills the
thought budget regardless. The module streams Cloud Code SSE back as the
gateway's OpenAI stream and preserves thought signatures across tool turns.

Identity headers are fixed to the Antigravity client shape; no `x-goog-api-key`
or `x-goog-api-client` header is sent. Context and output limits come from the
vendor's own metadata when it carries them, and fall back to `1,000,000` /
`64,000`.

## Notes

* Browser sign-in and token refresh use the Cloud Code IDE's own installed-app
  OAuth client, which is embedded in the module, so a stock install needs no
  configuration. `client_id` / `client_secret`, or the two environment
  variables above, point the module at a client this deployment registered
  instead. An installed-app client cannot keep a secret (RFC 8252), and the
  embedded pair is the one the vendor ships inside its own binaries.
* Sign-in opens Google's consent page in the default browser and waits for the
  loopback callback. A headless install needs an operator-supplied token import
  instead.
* A quota refusal parks that account for `quota_cooldown` and tries the next
  account. Session affinity keeps a conversation on one account while it is
  healthy.
