# `kimi` client

Wraps the **Kimi Code CLI** (`kimi`) as an OpenAI-compatible backend.

Route models as `kimi/<model>`, e.g. `kimi/kimi`, `kimi/kimi-k2`, `kimi/k3-agent`.

It has **two upstream routes**, and prefers the one that needs nothing installed:

1. **Direct HTTPS (default).** After a panel sign-in the module holds its own
   OAuth token and calls the vendor's OpenAI-compatible endpoint itself:

   ```
   POST https://api.kimi.com/coding/v1/chat/completions
   Authorization: Bearer <token>     Accept: text/event-stream
   ```

   This route spawns nothing, needs no CLI, and streams SSE deltas straight
   through. The API base follows the region that issued the token
   (`api.kimi.com` for mainland-cn, `api.kimi.ai` for global).

2. **The `kimi` CLI (fallback).** When there is no panel token — or the direct
   call fails — the module spawns the local CLI once per request and speaks to
   it over stdio:

   ```
   kimi -p <flattened prompt> --output-format stream-json [<permission flag> <mode>] [-m <model>]
   ```

   Set `prefer_http: false` to skip route 1 entirely.

On the direct route the module never sees the CLI's credential and the CLI never
sees the module's — the two stores are independent, and a panel sign-in is written
to `<DataDir>/kimi-token.json`, never to the CLI's own credential directory.

---

## ⚠️ Read this first: the permission mode

**The wrapped CLI is an autonomous agent, not a chat model.** Depending on how it is
invoked it can run shell commands, write files and call tools on the machine hosting
this gateway. The reference implementation (`kimi2api`) started it with whatever
permission policy the CLI defaults to, which on a Kimi Code install is an
auto-approving agent mode. Reproducing that would turn `POST /v1/chat/completions`
into remote code execution for anything that can reach the gateway.

The reference had the right instinct and the wrong fix — and so did this module, until
the CLI was actually installed here. It passed `--permission-mode <mode>` on every
request, using a flag name guessed while no CLI was present. Verified against
kimi-code **2.1.1** on this machine:

```
$ kimi -p "hi" --output-format stream-json --permission-mode default
error: unknown option '--permission-mode'
$ kimi -p "hi" --output-format stream-json --plan
error: Cannot combine --prompt with --plan.
```

`--yolo` and `--auto` are refused the same way. **Prompt mode (`-p`) accepts no
permission flag at all.** The guessed flag therefore tightened nothing; it killed every
CLI request during argument parsing, before the model was ever reached — strictly worse
than the auto-approval it was meant to prevent.

The module therefore:

- passes **no** permission flag by default (`permission_flag` defaults to `""`), which
  is the only invocation the real CLI accepts;
- keeps `permission_mode` / `permission_flag` configurable for a CLI that really does
  take a policy flag, and passes one only when the operator names it;
- states plainly that **the CLI's prompt-mode policy is not something this module can
  constrain** — see "What this does not protect" below;
- leaves `--skills-dir` **unset** by default, so the user's real skills directory is
  left alone (the reference hard-coded `empty-skills`, silently changing model
  behaviour).

### What this does not protect

Because prompt mode cannot be told to be restrictive, the CLI runs with its own
built-in policy, and this gateway cannot narrow it. The security boundary is therefore
**the gateway's reachability, not a flag**: bind it to loopback, do not port-forward
it, and do not run it as a user whose shell you would not hand to anyone who can reach
the port. If you need a hard boundary, run the gateway in a container or under a
restricted account; the module cannot provide one for you.

---

## Prerequisites

**The CLI is optional.** The module has two independent routes and prefers the
one that needs nothing installed:

1. **Sign in from the panel (default).** `POST /panel/api/clients/kimi/login`
   runs the same RFC 8628 device-code OAuth flow the CLI uses, directly against
   `https://auth.kimi.com` (client id `17e5f671-d194-4dfb-9706-5516cb48c098`).
   The panel shows the verification URL and the user code, polls until the
   operator confirms in a browser, and stores the token in its **own** file,
   `<DataDir>/kimi-token.json`. Chat then goes straight to
   `https://api.kimi.com/coding/v1/chat/completions` over HTTPS with
   `Authorization: Bearer …`. **No CLI, no keyring, no child process.**
2. **The `kimi` CLI (fallback).** Used only when there is no panel token, or
   when the direct HTTPS call fails. Install (Windows):
   `irm https://code.kimi.com/kimi-code/install.ps1 | iex`, then `kimi login`.
   The CLI keeps its token in the OS keyring, falling back to
   `~/.kimi/credentials/kimi-code.json`.

Set `login_mode: "cli"` to force route 2, or `prefer_http: false` to stop the
direct HTTPS path from being tried first.

The module **never** installs or downloads anything, and it never writes to the
CLI's credential store — a panel sign-in lands in the module's own `DataDir`, so
it cannot disturb a CLI login that is already there. If neither route is usable
it degrades honestly: `New` still succeeds, `GET /v1/models` still serves the
built-in catalog, `GET /v1/status` says in one line what is missing, and `Chat`
returns `core.ErrNotConfigured` naming the missing prerequisite (the gateway maps
that to HTTP 503).

That last part is why the panel login is tracked while it is passed over. When
`kimi-web` is stored but parked — the vendor refused the grant, so the account is
marked `dead` — or switched off in the panel, `Chat` falls back to the CLI and
then names **the panel login and the action that revives it** ("sign in again
from the panel" / "re-enable it in the panel"), instead of the CLI-only *"run
`kimi login`"* message. The CLI instruction is still exactly what is returned
when no panel login was ever made, because then the CLI really is the only route.

---

## Config

`Deps.Config` is the raw JSON under `"clients"."kimi"`. Everything is optional; a
missing or `null` config is valid. See `config.example.json`.

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `binary` | string | auto | Explicit path to the `kimi` executable. Empty ⇒ `exec.LookPath` then well-known install dirs. |
| `default_model` | string | `"kimi"` | Model used when the request does not name one. |
| `permission_mode` | string \| null | `"default"` | The policy name to pass **when** `permission_flag` names a flag. `""` ⇒ pass nothing. `null`/absent ⇒ `"default"`. Inert by default, because no flag is configured. |
| `permission_flag` | string | `""` (none) | The flag carrying `permission_mode`. Empty by default because the real kimi-code CLI (2.1.1) rejects **every** permission flag in prompt mode — see "Read this first". Set it only for a CLI that really takes one. |
| `skills_dir` | string | `""` | When set, passed as `--skills-dir <dir>`. Empty ⇒ no flag, user's own skills untouched. |
| `extra_args` | []string | `[]` | Appended verbatim to the CLI command line. |
| `cwd` | string | gateway cwd | Working directory for the child process. |
| `env` | object | `{}` | Extra environment variables for the child (merged case-insensitively on Windows). |
| `max_concurrency` | int | `2` | Cap on simultaneous child processes. Requests block until a slot frees. |
| `timeout_seconds` | int | `600` | Wall-clock budget per request; on expiry the child is killed. |
| `media_dir` | string | `<DataDir>/media` | Where extracted images are written before being referenced as `@/path`. Must stay inside `DataDir`. |
| `allow_image_download` | bool | `false` | Permit fetching `http(s)` image URLs from the request. Off by default (SSRF surface). `data:` URLs are always accepted. |
| `models` | []string | built-in | Overrides the advertised catalog. |
| `credential_files` | []string | built-in list | Overrides the credential files probed for the login check. |
| `assume_logged_in` | bool | `false` | Skip the credential-file check and let `Chat` try the CLI anyway. Useful when the token lives only in the OS keyring. |
| `login_mode` | string | `"device"` | Which sign-in route the panel's "Sign in" button uses. `"device"` ⇒ the RFC 8628 device grant against `auth.kimi.com` (no CLI). `"cli"` ⇒ the original flow that tells the operator to run `kimi login` and then watches for the credential file. An unrecognised value falls back to the default rather than removing the button. |
| `oauth_host` | string | `https://auth.kimi.com` | Override the OAuth host. `KIMI_CODE_OAUTH_HOST` / `KIMI_OAUTH_HOST` are consulted first. |
| `oauth_client_id` | string | kimi-code client id | Override the OAuth client id. Only useful if the vendor reissues it. |
| `prefer_http` | bool | `true` | Try the direct HTTPS path before the CLI. `false` forces everything through the CLI, and the direct path is then never contacted at all. |
| `api_base` | string | region-derived | Override the coding API base. Defaults to `https://api.kimi.com/coding/v1` for a mainland-cn sign-in and `https://api.kimi.ai/coding/v1` for a global one — the region follows whoever issued the token. |
| `device_id` | string | the CLI's own | Override the uuid sent as `x-msh-device-id`. Empty ⇒ reuse the id the installed CLI persisted at `<home_dir>/device_id`, else one minted into `<DataDir>/device_id` and kept. See "Header identity". |
| `home_dir` | string | `~/.kimi-code` | Where the CLI keeps its state, and with it where we look for a persisted device id. |
| `thinking` | string | `"enabled"` | The vendor's private thinking extension. Anything other than `off` sends `{"type":"enabled","keep":"all"}`, which is what the CLI sends; `off` omits the field. |
| `prompt_cache_key` | string | `session_<uuid>` | Pin the cache-routing hint sent as `prompt_cache_key`. Empty ⇒ one stable value per process. |
| `max_completion_tokens` | int | `0` (omit) | Output ceiling sent when the caller's request sets none. The CLI always sends one (262144 in the capture); `0` lets the vendor choose. A caller-supplied ceiling still wins. |
| `tls_profile` | string | `""` (off) | Imitate the official client's TLS handshake. `"node"` reproduces the captured kimi CLI ClientHello (JA3 `bd93ef21…`); `"chrome"`, `"firefox"`, `"safari"`, `"edge"` and friends come from `internal/fingerprint`. Empty ⇒ the standard library's own handshake, i.e. exactly the behaviour before this key existed. |
| `tls_protocol` | string | `"auto"` | `"auto"` offers h2 then http/1.1 and downgrades; `"h2"` demands HTTP/2; `"http/1.1"` offers only that. The CLI itself offers **http/1.1 only**, so `"http/1.1"` is the faithful choice for `tls_profile: "node"`. A configured proxy forces `http/1.1` regardless (see `internal/fingerprint`). |

Malformed JSON is a hard error from `New` (the gateway surfaces it as a startup
problem) — this is the one place where a bad config does not degrade quietly.

### Model catalog (built-in fallback)

`kimi`, `kimi-k2`, `k3-agent`, `k3-agent-ultra`, `k2d6-agent` — served from
`builtinCatalog()` entirely offline, so `GET /v1/models` works with no CLI and no
network. Any other model id in the request is passed straight through to `-m`, so the
catalog is a convenience, not a whitelist.

---

## Header identity

A hand-rolled `net/http` request announces itself: `User-Agent: Go-http-client/1.1`,
no device block, no SDK headers. That is the single clearest tell that a request
did not come from the official client, and it is worth removing, because the
coding endpoint is one the vendor actively polices.

So the direct HTTPS path sends what the CLI sends. Every value below was
observed on the wire from `kimi.exe` 2.1.1 talking to `api.kimi.com`; nothing
here is guessed from a name.

| Header | Value | Source |
| --- | --- | --- |
| `User-Agent` | `kimi-code-cli/2.1.1` | `createKimiUserAgent()` = `${product}/${version}`, no suffix |
| `x-msh-platform` | `kimi_code_cli` | `KIMI_CODE_PLATFORM` |
| `x-msh-version` | `2.1.1` | the CLI's own version |
| `x-msh-device-name` | `os.hostname()` | e.g. `DESKTOP-NMJNSLH` |
| `x-msh-device-model` | `"<platform> <release> <arch>"` | e.g. `Windows 10.0.26200 x64` |
| `x-msh-os-version` | `os.release()` | e.g. `10.0.26200` |
| `x-msh-device-id` | a version-4 uuid | see below |
| `accept-language` | `*` | — |
| `sec-fetch-mode` | `cors` | — |
| `x-stainless-*` | `retry-count: 0`, `lang: js`, `package-version: 6.34.0`, `os`, `arch`, `runtime: node`, `runtime-version: v24.15.0` | the OpenAI Node SDK the CLI bundles |

`x-stainless-os` and `x-stainless-arch` are the Node spellings (`Windows`, `x64`),
not Go's — `nodePlatform()` and `nodeArch()` do the mapping, and there is a test
pinning `amd64 → x64`.

The CLI hand-writes its OAuth form posts instead of going through its SDK, so
those carry the device block and the product User-Agent but **none** of the
`x-stainless-*` set. `applyIdentityHeaders(h, id, withSDK)` encodes that split,
and both the chat path and the model-list path use `withSDK=true`.

### The device id

The CLI mints a uuid once and keeps it at `<home>/device_id`; the gateway looks
in the same place and presents the same id, so one machine looks like one
installation rather than two. Resolution order:

1. `device_id` from the module config, if set;
2. `<home_dir>/device_id`, the CLI's own file — the normal case;
3. `<DataDir>/device_id`, from a previous run;
4. mint a version-4 uuid and persist it to `<DataDir>/device_id`.

The `<home_dir>/device_id` file is read but never written, so the gateway cannot
mutate the CLI's state.

### What is deliberately *not* reproduced

Node's undici writes lower-case header names in insertion order; `net/http`
writes canonical names in sorted order and owns `Host`, `Content-Length` and
`Connection` itself. A fingerprint that lower-cases names before hashing — the
usual practice, because HTTP/2 mandates lower case — sees the same header *set*
in a different *order*. Reproducing the order means writing requests by hand
onto a raw connection, which is a much larger change than the remaining signal
justifies. We use canonical case throughout: mixing cases would look like a
third client that is neither Node nor an ordinary Go program, which is worse
than either.

### The request body

Field order is load-bearing, because `encoding/json` emits keys in declaration
order and the CLI's SDK marshals the same way. `openAIRequest` is declared to
reproduce the captured order `model, messages, tools, stream, stream_options,
prompt_cache_key, thinking, max_completion_tokens`, with the fields the CLI never
sends (`temperature`, `top_p`, `stop`, `user`, `tool_choice`) slotted between
them as `omitempty` — so a request that does not set them serialises in exactly
the captured order.

Three shapes differ from a plain OpenAI client, all of them deliberate:

- **`max_completion_tokens`, not `max_tokens`.** The CLI sends the modern
  spelling. The caller's ceiling travels under that name; when the caller set
  none we send none (the CLI always sends one, derived from the model's context
  window — pin it with the `max_completion_tokens` key if byte-fidelity matters
  more than not guessing a ceiling).
- **`stream_options: {"include_usage": true}`** on every request. The CLI always
  asks for the usage block; it is the only place a streamed turn reports token
  counts.
- **`thinking: {"type":"enabled","keep":"all"}`**, a Kimi private extension that
  is not part of the OpenAI schema. On by default, `thinking: "off"` removes it.

### The TLS handshake

Headers are only half of it: a request that *looks* right but offers Go's
cipher-suite list and ALPN is still trivially sorted out. Setting
`tls_profile: "node"` reproduces the captured kimi CLI ClientHello exactly —
52 cipher suites, 12 extensions, `supported_groups` `4588-29-23-30-24-25-256-257`,
point formats `0-1-2`, **no GREASE** — with a JA3 of
`bd93ef214c72b1c71f7c56e85fc6ee89` to match. `internal/fingerprint` owns the
implementation and its test recomputes that md5 from a real handshake.

The CLI offers **only `http/1.1`** in ALPN, so `tls_protocol: "http/1.1"` is the
faithful pairing. `"auto"` offers h2 first and is more Chrome-like than
Node-like.

### Known residual differences

Honest list; each is a real signal that remains.

- **HTTP/2 frame shape is not imitated.** With `tls_protocol: "auto"` the
  SETTINGS payload and pseudo-header order are `golang.org/x/net/http2`'s. The
  node profile's `http/1.1`-only ALPN avoids this entirely, which is one reason
  it is the recommended pairing.
- **Header case and order** differ, as described above.
- **`x-msh-device-model`'s release word on non-Windows** is `runtime.GOOS` rather
  than a kernel version, because Go exposes no portable equivalent of Node's
  `os.release()`. Windows, the platform the CLI ships for, reads the same
  registry values Node's `RtlGetVersion` does.
- **The success response was never observed.** The capture used a synthetic
  token and got `401`, so `/chat/completions`'s 200/SSE shape is inferred from
  the OpenAI schema, not recorded. The fields we parse (`choices[].delta`,
  `reasoning_content`, `tool_calls`, `usage`) are the ones the CLI's own client
  reads. The live gateway reaches the vendor and is answered at the application
  layer — the handshake and the header block are accepted — but the account it
  holds a token for has no active Kimi Code subscription, so every turn ends in
  `403 access_terminated_error`. That is an account state, not a protocol
  failure: the same token gets the same `403` with *no* identity headers at all,
  while a bogus token gets `401 invalid_authentication_error` and
  `GET /coding/v1/models` returns `200` with the vendor's real catalog. A
  deployment needs a subscribed account before any of this can be called
  end-to-end verified.

---

## Credential discovery

Read-only, never writes, never refreshes, never prints a token.

- Environment variables (a non-empty value counts as a credential):
  `KIMI_API_KEY`, `KIMI_CODE_TOKEN`, `KIMI_TOKEN`.
- Credential files, first hit wins: `~/.kimi/credentials/kimi-code.json`,
  `~/.kimi-code/credentials/kimi-code.json`, `~/.kimi/credentials.json`,
  `~/.config/kimi/credentials.json`, and the `%APPDATA%` equivalents.
  A file counts only if it contains one of `access_token`, `accessToken`, `token`,
  `api_key`, `apiKey`, `id_token`.
- Expiry is read from `expires_at` / `expiresAt` / `expiry` / `expires` / `exp` as
  RFC 3339, `2006-01-02 15:04:05`, or a unix timestamp in seconds or milliseconds.

If nothing is found the module reports **"not logged in"** — but note the CLI may
still be logged in via the OS keyring, which the standard library cannot read. Set
`assume_logged_in: true` in that case.

---

## Status fields

`Status()` never makes a blocking network call and returns in well under a second
(its discovery caches are TTL'd at 10 s).

- `Ready` — true when a request could plausibly succeed now. Either a live panel
  token exists (the preferred case), **or** the CLI was found **and** a credential
  was found (or `assume_logged_in`).
- `Detail` — one human line. With a panel token: *"signed in from the panel via
  `https://auth.kimi.com`; requests go straight to `https://api.kimi.com/coding/v1`
  over HTTPS, so the kimi CLI is not required"*. Otherwise it names the discovered
  binary path plus the credential source, or the missing prerequisite.
- `Accounts` — the panel token's own row (`kimi-web`) first when one exists, then
  one `core.AccountStatus` per credential source, `State` ∈
  `ready | cooling | exhausted | invalid | unknown`, plus one entry for the CLI itself.
  The state is the *usable* state, not the probe's own opinion: a row whose stored
  health is `dead` reads `invalid` and one that is `cooling` reads `cooling`, so the
  panel can never show a green row for an account `Chat` would refuse.
- `Models` — bare model ids (no `kimi/` prefix).

---

## Panel account management

The module implements all three optional capability interfaces —
`core.AccountManager`, `core.CredentialImporter` and `core.LoginProvider` — in
`accounts.go`. They are advertised through `core.CapabilitiesOf`, so the panel
enables the account, discover and login screens for `kimi`.

The governing fact is that **the CLI's credential is not this module's to
store**. The CLI's account is its own RFC 8628 device-code OAuth token, which the
CLI keeps in the OS keyring (or in `~/.kimi/credentials/kimi-code.json`) and
refreshes itself on every prompt. So the panel can **observe** that account and
**steer** it, but it can never **authenticate with** it or **create** it — the
one thing this module reads out of the CLI's credential is *which Kimi account it
belongs to*, and that is the only part that is ever published (see
"One account, several rows" below). The one credential the
panel *can* create is its own sign-in — `kimi-web`, kept in
`<DataDir>/kimi-token.json` — which needs no CLI at all. Everything below follows
from that.

### `AccountFields` is deliberately empty

`AccountFields()` returns an empty, non-nil slice. There is nothing an operator
could usefully type into a form:

- A token pasted into a panel form would be a *second*, unmanaged copy of a
  credential the CLI already owns — and the panel is a loopback HTTP surface, so
  it would be the wrong place for a secret to land.
- The module cannot mint a login either: only the CLI can complete the device-code
  flow.

An empty field list is a contract-legal answer ("listable and testable, nothing
to type"), and it is the honest one. `AddAccount` therefore always fails rather
than pretending a form submission did something (see below).

### What the panel gets, per action

| Panel action | Behaviour |
| --- | --- |
| `accounts` (list) | Leads with `kimi-web` — this module's own panel sign-in — when one exists, then `cli-login` — the CLI's own login — then one row per credential source actually found (`file:<base>`, `env:<name>`), then one row per imported binding. `State` is `ready` / `invalid` (expired, or the stored health is `dead`) / `cooling` / `unknown` (nothing found; the CLI may still hold a keyring token), so the row always matches what `Chat` would do. The CLI missing gives `state=invalid` (or `unknown` with no login) and a note carrying the install + `kimi login` guidance. **No keyring read, no network.** A credential file *is* read, and only to learn which account it belongs to — see below. |
| `accounts` (add) | Refused. Returns an error wrapping `core.ErrUnsupported`: *"kimi's credentials are owned by the CLI … run `kimi login`"*. Nothing is written. Use the panel's **Sign in** button instead — that is the supported way to add a credential. |
| `accounts/<id>` (delete) | Deleting a row that exists always succeeds, and never destroys anything the operator cannot get back. `kimi-web` is a **sign-out**: its token in `<DataDir>/kimi-token.json` is removed, which is the only credential this module owns. `cli-login` and the `file:` / `env:` evidence rows belong to the CLI, so they are **forgotten** — the row leaves the table and the credential on disk is left byte for byte alone (this module does not delete another program's login). A forgotten row comes back by itself as soon as its evidence moves: a credential file rewritten, an environment value changed, a different CLI binary, `assume_logged_in` switched on. An imported **binding** is a real removal (this module's own record only; the executable is left alone). Deleting the same row twice is not an error; an unknown id is. |
| `accounts/<id>/enabled` | Toggles this module's own switch in `<DataDir>/accounts.json`. Meaningful: `enabled=false` makes `Chat` refuse and `Status` go `ready=false` — the operator can park a login without logging the CLI out. Works for `kimi-web` as well, so a panel sign-in can be parked without forgetting the token. Only an *explicit* choice gates anything. |
| `accounts/<id>/test` | Probes the CLI (found? where?) and the login (credential source? expiry?). For `kimi-web` it reports whether a live token exists and when it expires. It **sends no conversation**, so it is fast and costs nothing. Honest failures: `kimi CLI not found …` with the install line, or the "found but not logged in, run `kimi login`" message. |
| `accounts/refresh` | Returns one `RefreshResult{OK:false, …}` per account. Kimi has no renewal this module can trigger — the CLI refreshes its own token — so the error says exactly that and points at `kimi login`. A `kimi-web` token carries a refresh token, so signing in again is the renewal path. |
| `discover` | A **read-only** scan. Reports CLI-shaped executables on `PATH` and in the known install locations as `Importable`; reports `~/.kimi-work/bin` (importable only if the name is CLI-shaped), `~/.kimi-webbridge/identity.json` and `daemon.pid` and `bin/kimi-webbridge*`, `%APPDATA%/kimi-desktop` plus its `bridge-store/token-store.json` and `kimi-agent/kimi-work-models-cache.json`, and any credential files, each with a note saying **why** it is not importable (a device id; a pid file; a daemon that cannot serve a chat; DPAPI-encrypted storage; a model-id cache; contents that are never copied). |
| `import` | The only writing action. Binds CLI-shaped executables into `<DataDir>/accounts.json` as explicit `binary` bindings. Non-importable paths are rejected with a reason; re-importing the same path is idempotent; importing nothing is not an error. |
| `login` (start) | Default (`login_mode: "device"`): requests a device code from `auth.kimi.com` and returns `pending` with the verification URL **and** the user code to confirm. No CLI is involved. With `login_mode: "cli"`: already logged in → `success` immediately; CLI present → `pending` with *"run `kimi login` in a terminal"*; CLI absent → `failed` with the install guidance. When a panel login is already stored, the device message also names it (`kimi-web` at `<DataDir>/kimi-token.json`), because confirming a new code replaces that token in place — it never adds a second row. |
| `login/<session>` (poll) | Device mode: polls the token endpoint on the vendor's own interval, honouring RFC 8628 `slow_down` (interval +5s) and answering `pending` locally — without a request — when the panel polls faster than allowed. On success the token is written to `<DataDir>/kimi-token.json`. CLI mode: `success` once a credential source appears that was not in the session's baseline; it can also infer success from a changed `~/.kimi-webbridge/identity.json`, and when it does the message **says the success is inferred, not verified**. |
| `login/<session>` (cancel) | Drops the session and its baseline. It never logs the CLI out and never touches the CLI's files. |

### One account, several rows

`kimi-web` and `cli-login` are usually **the same Kimi user reached two ways**,
and before this the panel drew them as two accounts. An operator could not tell
whether parking one row killed the account or left it working.

So each record carries `core.AccountRecord.Identity`: the Kimi account the
credential belongs to. It is read out of the credential's own token payload
(`user_id`, else `userId`, `uid`, `sub`) — the vendor's account number, which is
not a secret — and never out of the file's name, which says nothing about who
owns it. A credential that is not a JWT (a bare API key) names nobody, and then
the row reports **no** identity.

The rules, all of them in `credentialReport.identity()` and `credentialIdentity()`:

* Every found credential that names an account must name the **same** one for
  `cli-login` to be grouped. Two credentials naming *different* users say nothing
  about the account, so they are kept apart rather than merged on a guess.
* A credential that names nobody does not veto the ones that do.
* A credential that was **not found** is not evidence, and never contributes an
  identity.
* The token itself is never returned, logged, or stored: `credentialIdentity`
  hands back the account id and nothing else, and `credentialToken` is documented
  as a value callers must not keep.

The panel draws one row per identity, with the individual channels nested under
it and each still separately testable and toggleable.

### What is persisted, and where

One file, `<DataDir>/accounts.json` (`core.Deps.DataDir`, mode 0600 via
`core.WriteJSONAtomic`), version-tagged:

```json
{ "version": 3,
  "enabled":  { "cli-login": false },
  "bindings": [ { "id": "binary:9f2c…", "kind": "binary",
                  "path": "C:\\Users\\you\\.kimi-work\\bin\\kimi.exe",
                  "label": "kimi", "added_at": "2026-…" } ],
  "health":   { "binary:9f2c…": { "state": "cooling", "cause": "process-failed",
                                  "cooldown_until": "2026-…",
                                  "last_error": "kimi: CLI exited with …",
                                  "last_used": "2026-…" } },
  "forgotten": { "cli-login": "bin=C:\\…\\kimi.exe|assume=false|refs=C:\\…\\kimi-code.json" } }
```

- `enabled` holds **explicit** operator choices only. An absent key means "module
  default", so upgrading never silently flips behaviour, and `Status()`'s
  historical defaults are untouched.
- `bindings` holds **paths**, never credentials.
- `forgotten` holds one entry per **deleted row this module does not own**: the
  id, and the evidence that was on display when it was deleted. It is what makes
  a deletion idempotent *and* reversible — the row stays hidden while that
  evidence is unchanged, and returns on its own once it moves. The signature says
  *where* the credential was found and *how big and new* it is (`len=…,mtime=…`
  for a file, `len=…` for an environment value, the binary path plus the
  credential refs for `cli-login`), so **no credential material is ever written
  here**. An explicit enable/revive drops the entry and brings the row back by
  hand; signing in again does the same for `kimi-web`.
- `health` is what this module has *learned* about an account. `state` is
  `healthy` / `cooling` / `dead`; `cause` is a locally observable reason
  (`cli-not-found`, `no-credential`, `process-failed`, `timeout`,
  `credential-refused`, `http-status` or `unclassified`); `cooldown_until` is when
  a cooling account becomes eligible again; `last_error` is a short reason string
  that always goes through `redactSecrets` (never a response body, never a
  token); `last_used` is the last success. **No credential is ever written
  here** — `TestHealthStateFileNeverHoldsASecret` asserts that on the raw bytes of
  the file, not on a struct.
- A `cooling` account is not selected before `cooldown_until`; a `dead` account is
  not selected at all. Time revives a cooldown, and **never** a dead account: what
  revives `dead` is positive evidence that the old verdict is stale (a credential
  appearing again, a successful panel sign-in, the bound executable being there)
  or an operator action (enabling the account again, re-importing the binding).
  A failure this module cannot classify is recorded as a reason **without**
  cooling the account — a guess must not park a working login.
- Health is a cache: a write that fails only logs (`kimi: could not write account
  health to …`) and never breaks the request. A state change writes immediately;
  level updates (a new `last_error`, a new `last_used`) are debounced to at most
  one write per 5 seconds.
- A file written by an older build loads unchanged and simply yields "nothing
  remembered yet" for whatever the old build did not keep: a `"version": 1` file
  has no `health`, and `version` 2 has no `forgotten`. Each bump is honest
  labelling, not a format the old reader cannot survive — an unknown key is
  ignored, and a missing one is read as "the operator has not done this yet".
- A corrupt or unreadable file never bricks the module: `Chat` ignores it,
  `Status` appends `account state unreadable: …` to `Detail`, and `Accounts`
  reports the real state.

Nothing else is written, anywhere. `Discover` in particular is pure read.

### Where a binding fits into binary resolution

`locate()` resolves in this order: `clients.kimi.binary` (if configured) → an
**enabled imported binding** → `PATH` → the built-in candidate paths. An imported
binding therefore outranks `PATH` but can never override an explicit
configuration; a binding whose file has vanished falls through silently rather
than breaking the module, and a binding the operator disabled — or one this
module has watched fail and is cooling down, or has marked dead — is skipped too.
Those rows carry `health`, `health_cause`, `cooldown_until`, `last_error` and
`last_used` in their fields, and their `Note` says why, so an operator can see
the reason in the panel without opening the JSON file. The stored health also
decides the row's `State` (`dead` → `invalid`, `cooling` → `cooling`), because a
row that still reads `ready` after this module gave up on the account is a lie
the operator would act on.

### What is *not* implemented, and why

1. **Driving the CLI's login TUI.** The CLI's login is an interactive device-code
   flow, and scripting its terminal UI would mean guessing at prompts and
   keystrokes. Instead the module implements the *same* RFC 8628 device grant
   itself, in Go, straight against `auth.kimi.com` — which is why the panel's
   Sign in button needs no CLI installed. `LoginState.URL` and `.Code` carry the
   vendor's own verification URL and the user code the operator confirms.
2. **Reading the CLI's token.** Never attempted, in any form: no keyring, no
   DPAPI, no LevelDB, no `token-store.json` decryption. The panel sign-in does not
   need it — it obtains its own token through the OAuth flow — and the module never
   writes to the CLI's credential store either, so the two logins cannot disturb
   each other.
3. **`kimi logout`.** Never invoked. Removing the CLI's login is the operator's
   action in their own terminal; a panel token is parked by disabling `kimi-web`,
   not by logging anything out.
4. **Creating a second, independent panel account.** The device flow yields one
   token per sign-in, and a new sign-in replaces the previous one — there is no
   multi-account pool for the panel route, because the vendor's flow is
   single-session by design.

The one place `Accounts()` deliberately differs from `Status()`: `Status`
returns one row per credential *source* (its historical shape, pinned by tests),
while `Accounts()` leads with `kimi-web` when a panel token exists, then the
`cli-login` row, then the source rows. The panel's account list is about "which
account is this", not "which file was found".

---

## Known gaps / deliberate deviations from the reference

1. **Tool calls are prompt-injected, not native.** As in the reference, the tool list
   is flattened into the prompt and the model is asked to answer with a single
   `{"tool_calls":[…]}` JSON object. Parsing is **structural** (`parseToolEnvelope`:
   the whole trimmed message must be one JSON object with a non-empty `tool_calls`
   array whose entries all have a name) instead of regexing the output. Native
   `tool_calls` records are also understood if a CLI version emits them. **Partial
   tool arguments cannot be streamed**: one complete call is emitted per fragment, so
   `Index` grouping is trivial and no incremental argument assembly is attempted.
2. **Streaming granularity is the CLI's, not the token's.** The reference's SSE was
   per assistant message block. This module emits one `EventDelta` per NDJSON text
   fragment, which is finer, but still not token-by-token.
3. **Usage is real or absent.** If the CLI reports no usage, **no** `EventUsage` is
   emitted. The reference emitted a fake `{0,0,0}`; that is not reproduced.
4. **Prompt mode takes no permission flag, so the CLI's own policy stands.** Verified
   against kimi-code 2.1.1: `--permission-mode` does not exist, and `--plan` / `--yolo`
   / `--auto` are each refused with `error: Cannot combine --prompt with ...`. The
   module therefore passes no flag by default; `permission_mode` / `permission_flag`
   remain configurable for a CLI that really takes one. See "What this does not
   protect" above — the boundary is the gateway's reachability, not a flag.
5. **The CLI path shares kimi-code's session store, which is single-process-locked.**
   Observed live: with `max_concurrency` at its default and a second `kimi` process
   running (a direct probe, or your own Kimi Code desktop app), a request fails with

   ```
   error: failed to run prompt: storage write failed: permission denied
   ```

   and the CLI's own log shows the cause:

   ```
   WARN session index reconciliation failed
     error="LockError: database is locked by another process:
             C:\Users\vencent\.kimi-code\cache\query-store\shard-11"
   ```

   The message says "permission denied" but nothing is wrong with your file
   permissions — `~/.kimi-code/cache/query-store` is a minidb store that only one
   process may write at a time. Remedies, in order: close any other `kimi` / Kimi Code
   instance, and set `max_concurrency: 1` so this module never spawns two CLIs at once.
   The HTTP route (`prefer_http`, the default) does not touch the store at all.
6. **No keyring support.** Stdlib only, so a token held exclusively in the Windows
   Credential Manager / macOS Keychain / libsecret is invisible to the login check.
   Use `assume_logged_in`.
7. **`--skills-dir empty-skills` is not the default** (see the warning above).
8. **Images**: `data:` URLs are decoded and written under `media_dir`; remote `http(s)`
   URLs are only fetched when `allow_image_download` is on. Anything else becomes the
   literal placeholder `[image]`, matching the reference's error path. Media files are
   removed when the stream finishes or is closed.
9. **`Chat` rejects a `nil` request** with an error rather than panicking.
10. One process per request, as in the reference: every request is an independent,
    stateless Kimi session. There is no conversation continuity across requests.
11. `temperature`, `top_p`, `max_tokens`, `stop`, `tool_choice` (other than `"none"`,
    which suppresses the tool block) have **no** CLI equivalent and are silently
    dropped. `Chat` does not return `ErrUnsupported` for them, because the request is
    still expressible — the model just runs with the CLI's own settings.

---

## Tests

`go test ./clients/kimi/...` runs fully **offline**. Child-process tests point
`binary` at a stub script written into `t.TempDir()` (a `.cmd` on Windows, a `sh`
script elsewhere); NDJSON fixtures are inline string literals; credentials are
isolated by redirecting `HOME`/`USERPROFILE`/`APPDATA` to a temp dir. Nothing in the
default run touches the network, the real CLI, or the user's credential files.

A live round trip would need the CLI installed and logged in; there is deliberately no
`*_live_test.go`, because there is nothing here that can be exercised without the real
binary.

`accounts_test.go` covers the panel surface the same way, entirely offline: the empty
field schema, honest account rows with and without a CLI, the refused `AddAccount`,
`RemoveAccount` protecting `cli-login` (and forgetting only its own bindings),
enable/disable persisting across clients and actually gating `Chat`, honest `TestAccount`
results with a stub that would explode if it were ever executed, `RefreshAccount`,
`Discover` reporting each finding with its reason and writing nothing, `Import` binding a
CLI that `PATH` cannot see (and `PATH` still losing to a configured `binary`), and the
login start/poll/cancel paths including the "success is inferred" message. One test,
`TestLivePanelAccountsAgainstTheRealMachine`, runs only under `CLIENT2API_LIVE=1` and is
read-only (`Accounts` + `Discover` + capabilities); it is the single skipped test in the
default run.

---

## Provenance and licence

The shape of this module — spawning `kimi -p … --output-format stream-json`, parsing
NDJSON line by line, prompt-injected tool calls, images written to temp files and
referenced as `@/path` — is ported from **`kimi2api`**, which is **MIT licensed**.
Porting it into Go is therefore permitted; this implementation is a re-derivation, not
a translation, and fixes the defects listed above (unbounded child processes, no
timeout, `--skills-dir empty-skills`, the auto permission mode, fake zero usage,
regex tool-call parsing, cancellation).

This module is stdlib-only. No dependency was added.
