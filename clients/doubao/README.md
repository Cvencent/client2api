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
   throwaway Chromium profile (Edge or Chrome) on `https://www.doubao.com`, and
   raises that window to the front so it is not lost behind other windows.
   **Sign in inside that window** — scan the QR code or enter a phone number
   there. Signing in on your own browser does not work: the session cookies are
   HttpOnly and the module can only read them from the browser it drives. The
   module reads the cookies back over CDP, verifies them against the account and
   profile endpoints, and stores them; the browser is closed once the login
   succeeds. While it waits, the panel reports how many cookies the window
   currently has and whether the `sessionid` is still missing, so "still waiting"
   never hides a window that was never signed in.
2. **Paste a cookie.** Copy the Cookie request header from a signed-in
   `www.doubao.com` tab and paste it into the account form's 会话 Cookie field.
   Use this when no Chromium-family browser is installed, or when the hand-off
   window cannot be found.
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
| Balance | credits plus the package's fixed / rolling quota meters and the growth campaign's granted 额度 |
| **Daily check-in** | `POST /alice/ug/campaign/landing/checkin` (`CheckinProvider`) |
| **Reward board** | the PC reward centre's chores and milestones (`TaskProvider`); claims a reward the operator finished in the client |
| Health + pool stats | standard panel reporting |
| Revive | clears a local cooldown/park |
| Conversation stickiness | pins a conversation to the account that first served it |

Not implemented, so the panel hides the control rather than offering a button
that cannot work: SMS / auto login and package/voucher reporting.  The reward
board's work chores cannot be *performed* by the gateway (they need the desktop
client), only *claimed* once done — the board says so per row.

## Growth: check-in, reward board and 额度

Doubao moved its reward centre to a quota ("额度") model: one daily check-in
plus a board of chores, each paying out a quota grant.  The module speaks the
PC client's own endpoints, read out of the running desktop client and confirmed
live (2026-11-13):

| Call | Route |
|---|---|
| campaign board (tasks + check-in + milestones) | `POST /alice/ug/campaign/v3/detail/` `{"campaign_group_code":"pc_incentive_task_mvp_v3"}` |
| daily check-in | `POST /alice/ug/campaign/landing/checkin` `{"scene":"<group>"}` |
| claim a reward | `POST /alice/ug/campaign/item/reward/claim/` `{"campaign_group_code":..,"campaign_item_code":<task code>,"task_id":""}` |
| grant ledger | `POST /alice/ug/campaign/quota/records/` |
| quota meters | `POST /alice/commerce/sale/subscription/quota/summary/` |

Two facts are load-bearing:

- **The growth endpoints key on the PC product identity (`aid=497858`).**  The
  same request under the web aid (`582478`) is refused with `710010702 system
  error`.  The model menu has the same split: under the PC aid the v2 conf is
  served and lists every model the client shows, while under the web aid the
  same account answers with no v2 conf at all (so the catalogue silently
  collapsed to a two-entry fallback).  The module now tries the PC identity
  first for both, keeping the web identity as a fallback.
- **A reward is claimed by `campaign_code`,** with an empty `task_id`.

`task_status` is the client's own `CampaignTaskStatus` enum:
`0 unknown, 1 created, 2 completed, 3 rewarded, 4 in progress`.  A row is
claimable when it is `completed` (2) — the check-in row is the exception, where
"created" means "not checked in today" and is claimed through the check-in
route.  A `1` work chore must be performed in the desktop client first; the
module refuses it with that reason rather than spending a claim the vendor
would reject.  A repeated check-in (`idempotent:true`) is a success.

The campaign group code is versioned by the vendor (`_v3`, `_v2`, …); the
module tries the known codes in order, so a vendor switch does not blank the
board.

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

The same summary carries `incentive_quota_section` — the **growth campaign's
granted 额度** ("活动赠送额度"), which is what the reward board pays into.  It is
surfaced as an extra meter; `total_amount`/`used_amount` are numeric strings on
the wire.

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
