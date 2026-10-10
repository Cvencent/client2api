# Dynamic relay platforms

The operator can define independent OpenAI-compatible platforms, each with a
stable routing ID, display label, API root and multiple API-key accounts. A
platform is available immediately after saving and reloading the configuration.
Existing gateway authentication, platform policies, account priorities, model
groups, usage attribution and failover apply to these platforms unchanged.

## Configuration and compatibility

`sources` is an object keyed by a lowercase routing ID. Each entry carries
`label`, `base_url`, optional `disabled`, and `max_tokens_field`. IDs use ASCII
letters, digits and hyphens, cannot collide with compiled modules, and cannot
be Windows reserved directory names. API roots must be HTTP(S), without
embedded credentials, query or fragment. Keys are saved in each source's own
`data/<id>/accounts.json`, so instance bundles include them automatically.

The existing `openai-compat` module remains addressable with its original
configuration, storage and qualified routes. Its panel label becomes
`onmiRoute`. Platform IDs remain distinct from labels everywhere in the panel.

## Runtime and accounts

The generic source factory is registered by the OpenAI-compatible module;
core and main do not import concrete client modules. Runtime reconciliation
adds, updates and removes sources without restarting the gateway. Existing
client instances retain open streams and pool state across definition edits.
Deleted/disabled sources stop accepting new routes; credentials remain on disk.

Accounts contain a generated ID, label, API key, enabled flag and model list.
An explicit scan calls authenticated GET `/models` for each enabled account,
parses the standard OpenAI `data[].id` response and persists the account's
catalogue. The public catalogue is the union of enabled accounts. Failed scans
preserve the previous list. Successful empty lists remove stale models. Models
with slashes keep their upstream spelling. URL changes invalidate old model
lists; scans started before a URL/key change cannot overwrite the new state.

Chat selects accounts that advertise the requested model, ordered by the
gateway account priority, rotating equally ranked accounts. Admission uses
existing gateway account slots. Retryable pre-stream failures may try another
eligible key, with a bounded number of attempts and cooldowns. Stream errors
retain source/account attribution and do not replay emitted output.

## Panel and verification

The platform page provides add/edit/delete, enabled state, model scan and
account-pool access. Existing forms manage API keys. All selectors and policy
forms derive source IDs dynamically and show labels independently. Dynamic
sources must not be edited as compiled module settings or restart-only flags.

Tests cover validation, lifecycle, per-key model permissions, scan persistence
and failure retention, rotation, priority, error attribution, legacy operation,
panel configuration and bundle round trips. Build and full Go tests, targeted
race checks and isolated browser interaction checks are required. The deployed
service and its configuration/data are never changed during implementation.
