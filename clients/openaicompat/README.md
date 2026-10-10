# onmiRoute / OpenAI-compatible relay transport

The legacy module retains the routing ID `openai-compat`, configuration and
account store; the panel displays it as `onmiRoute`. This package also registers
the dynamic source factory: top-level `sources.<id>` definitions create
independent live platforms with their own multi-key account pools and persisted
`GET /models` catalogues. See the root README's custom-relay section for setup.
New platforms use `<id>/<upstream-model>` rather than the legacy nested prefix.

One module, many vendors. `openai-compat` brings the free tiers of any
OpenAI-compatible HTTP API into the gateway as a single client. Groq, Cerebras,
SiliconFlow, Mistral, NVIDIA NIM, Together, Fireworks, DeepInfra, Chutes and
HuggingFace ship with a built-in base URL; any other OpenAI-shaped endpoint
works by supplying its `base_url` explicitly.

This is the piece worth taking from OmniRoute's registry design: a source is
**configuration, not code**. Adding a provider is a config row, and the routing
prefix is the row's `id`.

## Routing

Callers of the gateway qualify a model with this module's own prefix first,
then the provider: `openai-compat/<provider>/<model>`.  Inside the module
(and in `GET /v1/models` once the gateway has added its prefix) the form is
`<provider>/<model>`:

```
groq/llama-3.3-70b-versatile
cerebras/gpt-oss-120b
groq/openai/gpt-oss-120b        # model ids may contain slashes
mistral/mistral-large-latest
```

The prefix selects the provider row; the rest is sent upstream verbatim as the
`model` field. A model whose prefix names no configured provider is refused with
`ErrUnsupported` before any network call.

## Configuration

```jsonc
{
  "clients": {
    "openai-compat": {
      "providers": [
        {
          "id": "groq",
          "base_url": "https://api.groq.com/openai/v1", // optional for known ids
          "api_key": "gsk_...",
          "label": "personal groq key",                  // optional
          "disabled": false,                             // optional
          "models": ["llama-3.3-70b-versatile"],         // optional; "" = built-in table
          "extra_headers": { "X-My-Header": "value" }    // optional
        }
      ],
      "extra_models": [],
      "chat_timeout": "10m",
      "models_timeout": "30s",
      "stream_idle_timeout": "90s",
      "models_ttl": "10m",
      "cooldown": "60s",
      "rate_cooldown": "2m",
      "quota_cooldown": "30m",
      "auth_cooldown": "30m",
      "max_in_flight": 4,
      "max_tokens_field": "max_tokens"
    }
  }
}
```

See `config.example.json` for a ready-to-edit file. The whole block is
optional: with no config the module still starts, lists the catalogues of the
providers added through the panel, and reports "no provider configured".

### Fields

| field | meaning |
|---|---|
| `id` | Routing prefix and the built-in default lookup key. Required. |
| `api_key` | Bearer credential. Required unless `base_url` is a loopback address (`127.0.0.1`, `localhost`, `::1`), where a keyless local service such as OmniRoute is expected; a row that is neither keyed nor loopback is dropped. |
| `base_url` | OpenAI-compatible API root, no trailing slash. Empty = built-in table. |
| `label` | Display name for the panel. Defaults to `id`. |
| `disabled` | Parks the row without deleting it. |
| `models` | Explicit served list. Empty = the built-in table for the id, or `extra_models`. |
| `extra_headers` | Appended to every request. `Authorization`, `Content-Type`, `Accept` and the other transport headers cannot be overridden. |

Config-level knobs: the timeout/cooldown durations, `max_in_flight`,
`max_tokens_field` (`max_tokens`, the default, or `max_completion_tokens`), and
`extra_models` (appended to every provider whose own list is empty).

### Built-in providers

| id | base URL | sample models in the cold-start table |
|---|---|---|
| `groq` | `https://api.groq.com/openai/v1` | `llama-3.3-70b-versatile`, `openai/gpt-oss-120b` |
| `cerebras` | `https://api.cerebras.ai/v1` | `gpt-oss-120b`, `zai-glm-4.7` |
| `siliconflow` | `https://api.siliconflow.com/v1` | `deepseek-ai/DeepSeek-V3`, `Qwen/Qwen3-32B` |
| `mistral` | `https://api.mistral.ai/v1` | `mistral-large-latest`, `codestral-latest` |
| `nvidia` | `https://integrate.api.nvidia.com/v1` | `meta/llama-3.3-70b-instruct` |
| `together` | `https://api.together.xyz/v1` | `meta-llama/Llama-3.3-70B-Instruct-Turbo` |
| `fireworks` | `https://api.fireworks.ai/inference/v1` | `accounts/fireworks/models/llama-v3p3-70b-instruct` |
| `deepinfra` | `https://api.deepinfra.com/v1/openai` | `meta-llama/Meta-Llama-3.3-70B-Instruct` |
| `chutes` | `https://llm.chutes.ai/v1` | `deepseek-ai/DeepSeek-V3` |
| `huggingface` | `https://router.huggingface.co/v1` | `meta-llama/Llama-3.3-70B-Instruct` |

The cold-start tables are deliberately short: they are the ids the vendor
documents, so a fresh process can serve something before any live listing
succeeds. An operator list in `models` always wins.

Cloudflare Workers AI, OpenRouter-compatible gateways, and local servers
(`http://127.0.0.1:11434/v1`, ...) all work by giving them an explicit
`base_url`; they are not in the built-in table because their root is
account-specific.

## Panel: accounts = providers

The module implements `core.AccountManager`, so the panel can add, list,
enable/disable, test and refresh providers. Each account is one provider row:

- **Add** collects `provider`, `api_key`, and optionally `base_url`, `models`
  and `label`. Rows added here are written to this module's own
  `data/openai-compat/accounts.json`, never to the main config.
- **Get a key** points the operator at each vendor's key page.  The module
  implements `core.KeyPageProvider`, so the panel renders a "create an API
  key" link under the provider picker and follows the selection.  That is the
  whole add flow: no browser login tab, because a key pasted here is the
  operator's own account and is the only credential these vendors issue for
  a third-party tool.
- **One-click local sources** are the shortcut past that form.  The module
  implements `core.QuickConnectProvider` and advertises OmniRoute: the panel
  probes `http://127.0.0.1:20128/v1/models`, and a single click creates the
  `omniroute` row (model `auto`, no API key) and tests it.  Probe is
  read-only, so "check" never turns into "create" behind the operator's back.
- **Remove** and **enable/disable** only act on panel-owned rows. A provider
  written into `clients.openai-compat.providers` is shown but reported as
  unremovable, because the durable copy lives in the config file the panel must
  not edit. A panel row whose id collides with a config row is dropped: the
  config file always wins.
- **Test** sends one short streaming completion and reports the reply or the
  vendor's own refusal. It does not take an in-flight slot.
- **Refresh** re-validates a key with `GET /models`. A vendor without that
  endpoint reports a per-provider error rather than failing the batch.

The API key is never returned to the panel; account records carry only the
source, base URL, model list and in-flight count.

## Conversation stickiness

Both halves of this module keep a conversation on the credential that first
served it. The legacy client pins a qualified-model conversation to its one
provider row; a dynamic `sources.<id>` relay pins the conversation to the key
that warmed the upstream prompt cache. The key comes from the usual spellings
(`conversation_id` / `conversationId` / `prompt_cache_key` / the request user),
and falls back to `core.DeriveConversationKey` so a client that sends no id at
all still stays put instead of rotating and re-billing the whole prefix. A
binding is ignored while its source is disabled, its key is disabled or cooling
down, or the key's scanned catalogue does not advertise the requested model, so
stickiness never serves a parked key. `session_sticky.enabled` / `ttl` /
`gc_interval` retune it live.

A streaming relay that drops the connection before a terminal frame (`[DONE]`
or a chunk carrying `finish_reason`) is reported as an upstream failure rather
than a clean end of stream, so a truncated answer fails over to another key
instead of being handed to the caller as a successful completion.

## What this module deliberately does not do

- It does not ship, install or supervise OmniRoute.  The button connects to a
  copy the operator already installed and started; when that process is down
  the row fails like any other unreachable upstream, and the probe says so.
- No OAuth, device-code, PKCE or credential-file import. Vendors whose free
  access needs an interactive login stay their own module.
- No per-account balance or usage API: the OpenAI-compatible surface does not
  define one, and guessing per vendor would be wrong more often than right.
- No cross-provider automatic failover on `Chat`: the prefix picks one
  provider. Failover between providers belongs to the gateway's platform
  routing, not to this module.
- No proprietary request knobs. Anything the wire format does not define is
  passed through `options.<name>` only when the module understands it
  (`presence_penalty`, `frequency_penalty`, `top_k`, `seed`,
  `parallel_tool_calls`).

## Verification status

The wire behaviour (SSE framing, tool-call deltas, usage, error
classification, the `/models` envelope) is pinned by `httptest` fixtures. No
live call has been made against any vendor from this machine, so each vendor's
exact error shapes remain `[unverified]`; the classification is deliberately
keyword-tolerant for that reason.
