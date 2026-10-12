# doubao (Doubao)

Doubao is ByteDance's mainland consumer assistant. The desktop client is a
Chromium-family shell around the `www.doubao.com` web app; this module speaks
that same web API, so the gateway can route `doubao/<model>` requests through a
Doubao account.

## How it authenticates

The credential is the browser's own session cookie jar (`sessionid`,
`sessionid_ss`, `sid_guard`, `ttwid`, `msToken`, ...). These cookies are
HttpOnly, so no page script can read them and the vendor never hands them out
over an API. There are therefore three ways to get one in:

1. **Web login (preferred).** The account page's "web login" button launches a
   throwaway Chromium profile (Edge or Chrome) on `https://www.doubao.com`.
   Scan the QR code or enter a phone number in that window; the module reads the
   session cookies back over CDP, verifies them against the account and profile
   endpoints, and stores them. The browser is closed once the login succeeds.
2. **Paste a cookie.** Copy the Cookie request header from a signed-in
   `www.doubao.com` tab and paste it into the account form's 会话 Cookie field.
   Use this when no Chromium-family browser is installed.
3. **Import from the desktop client.** The panel's 导入凭据 tab reads the
   already-signed-in Doubao desktop client directly. The client is an ordinary
   Chromium shell, so the module opens its `Cookies` database read-only, unwraps
   the AES key from `Local State` with Windows DPAPI, and recovers the same
   session cookie jar. It never writes to the client's profile, so a running
   client is not disturbed.

## Models

There is no separate local model API in the desktop client: it renders the
vendor server's own menu. The catalogue is therefore the model menu the
signed-in account's desktop client is showing
`menu_conf_v2` mode/model list on the v2
build, and the legacy `action_bar_menu_config.menu_item_list` when the account
has not rolled into v2). The two are never merged: `action_bar_v3/brief_list`
carries `use_menu_conf_v2`, and once that is true the v2 menu is the only
source. An enabled but empty v2 menu is an error rather than permission to fall
back to v1.

The module follows the client's own v2 branches. When
`use_mode_model_list=true` each mode owns its model list and the flat list is
used only for a mode that has no list of its own; when it is false the flat list
is the candidate set and `support_models` assigns each model to every mode that
can route it. A model item's key is `model_item_key` then `item_id` in v2 (the
v2 picker never reads `menu_type`) and `model_item_key` then `menu_type` then
`item_id` in v1. The visible mode/model pair is kept as-is; only the vendor's
`reply_mode` compatibility rule rewrites the old mode 2 / model 4 pair to
`reply_mode=3`.

`GET /v1/models` and the panel picker list one entry per menu item, named
`mode<mode_id>_model<model_item_key>`; a chat request for one of those ids
carries the matching `reply_mode` / `mode_id` / `model_item_key` routing triple
on the message `ext`, exactly as the client sends it. Before the first refresh
-- or for an id the account has not advertised -- the module falls back to the
client's own default (`mode_id=3`, `model_item_key=0`, "快速回答"). Two accounts
can see different menus; the catalogue is their union and each request routes
against the account it lands on.

## Capabilities

| Capability | What it does |
|---|---|
| Chat | `POST /samantha/chat/completion`, streamed to the OpenAI surface |
| Web login | controlled browser + CDP cookie read (`LoginProvider`) |
| Client import | read-only `Discover` / `Import` of the desktop client's signed-in profiles (`CredentialImporter`) |
| Model menu | pulls the account's client model menu and routes each model |
| Account management | list / add / enable / disable / test / remove |
| Balance | credits plus the package's fixed / rolling quota meters |
| Health + pool stats | standard panel reporting |
| Revive | clears a local cooldown/park |
| Conversation stickiness | pins a conversation to the account that first served it |

Not implemented, so the panel hides the control rather than offering a button
that cannot work: check-in, the task centre, SMS / auto login, and
package/voucher reporting.

## Balance

The primary number is `data.credit_info.total_credit_num` from
`POST /commerce/benefit_supply/credit/get_credit_num_optional_tasks`
(`{"need_tasks":false}`), labelled with the vendor's own `credit_text` or
`积分` when it is absent.

The desktop client also shows the membership package's usage windows under the
credit balance, so the module reads
`/alice/commerce/sale/subscription/quota/summary/` (with
`/alice/commerce/sale/subscription/overview/` as a compatibility fallback) and
adds one extra meter per window: `window_type=1` renders as `固定额度`,
`window_type=2` as `循环额度`. The meter carries the vendor's `used_percent`,
the derived remaining percent and, when present, the reset time from
`end_time`. A zero fixed window with no dates is the vendor's empty
placeholder and is omitted. The quota read is optional: when it fails the
credit balance is still shown.

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
  "client_user_data_dir": "",
  "client_profile": "",
  "default_mode_id": "3",
  "default_model_item_key": "0",
  "default_reply_mode": "0",
  "default_reasoning_effort": "",
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

`client_user_data_dir` overrides where the client import looks for the desktop
client (empty means `%LOCALAPPDATA%\Doubao\User Data`; the environment variable
`CLIENT2API_DOUBAO_USER_DATA_DIR` does the same), and `client_profile` pins a
specific profile directory instead of importing every signed-in one. The
`default_*` keys are the routing triple used only when a requested model is not
in the account's menu; they mirror the client's own default and rarely need to
change.

The catalogue comes from the accounts' client model menus. `models` is only a
fallback for a host where no menu can be read.

A vendor rejection marks the session `invalid` for `auth_cooldown`; the row
shows the retry deadline, and the cooldown expiry clears the verdict and returns
the account to rotation. A transient failure uses the shorter `cooldown`
instead. `revive` clears either park immediately.

## Notes

- No request signature (`a_bogus`/`X-Bogus`) is required: the desktop bundle's
  own common-header builder is empty and authentication is the cookie alone.
- Credentials are stored only under the module's data directory and never appear
  in logs, `Status`, the panel, or error messages.
