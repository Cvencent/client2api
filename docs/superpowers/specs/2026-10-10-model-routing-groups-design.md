# Model routing groups design

## Goal

Add two operator-facing routing controls to client2api:

1. A platform priority can be overridden for one model group.
2. Several platform-specific model ids can be declared equivalent, so any bare member model name or the group name routes across all available members.

## Configuration

Add a top-level `model_groups` object to `configs/client2api.json`:

```json
"model_groups": {
  "deepseek-v4.1-flash": {
    "members": [
      "opencode/deepseek-v4.1-flash",
      "cline/cline-free/deepseek-v4.1-flash",
      "workbuddy/cn:deepseek-v4.1-flash"
    ],
    "platform_priorities": {
      "opencode": 10,
      "cline": 20,
      "workbuddy": 30
    }
  }
}
```

Rules:

- Group names are case-insensitive and act as virtual model names.
- A group name must not contain `/`.
- Members use `client/model` and each member may belong to only one group.
- A group name cannot collide with an alias; aliases remain single-target maps.
- `platform_priorities` values are whole numbers, including negatives. Lower wins.
- A platform with no group-specific priority falls back to the existing global `platforms.<name>.priority` and `priority_schedule`.
- Missing clients or catalogue entries are ignored at request time, so dynamic catalogues do not make the config invalid.
- The existing `platforms` and `aliases` sections keep their current meaning.

## Routing semantics

Resolution order becomes:

1. Existing alias resolution.
2. Existing `Auto/` prefix handling.
3. An explicit `client/model` request still locks that client and is never expanded through a group (decision A).
4. A group name or an unqualified member model id resolves to every available member of that group.
5. All other requests keep the existing catalogue and canonical-name routing path.

For group candidates, availability, cooldown, free preference and failover keep the current order. The only change is that the platform priority is first looked up in the group's `platform_priorities`, then the existing global platform priority is used as fallback.

Each group member is resolved against its platform catalogue by exact model id (case-insensitive). A missing catalogue entry is skipped. If no member is currently available, the request returns a clear model-group error.

## Catalogue and panel

- `/v1/models` advertises each group name as a virtual model with `owned_by: "group"` and a `members` extra field.
- The existing platform-specific and `Auto/` entries remain unchanged.
- The Platforms page gains a model-routing section above the platform cards.
- Each group editor edits the group name, member rows, and one priority field per member platform.
- Add/remove group and member operations are local until Save & Apply, which uses the existing `PATCH /panel/api/config` plus `POST /panel/api/reload` path.
- Deleting a group sends `null` for that key; deleting one priority sends `null` for that priority key so the recursive config merge cannot leave stale values behind.

## Validation and errors

Panel writes reject:

- non-object `model_groups`;
- empty or slash-containing group names;
- empty member lists or blank members;
- members without `client/model`;
- duplicate members, including case-only duplicates;
- duplicate group names and alias/group collisions;
- platform priority keys not present among the group's members;
- non-integer priorities.

Runtime routing treats a configured member that is absent from a live catalogue as temporarily unavailable rather than as a configuration error.

## Testing

- Core tests cover group resolution, member-name resolution, explicit-platform locking, group-specific priority fallback, and missing members.
- Gateway tests cover the virtual group catalogue entry and a chat routed through the group.
- Main-package tests cover JSON projection into the registry.
- Panel tests cover validation, config ordering and the UI contract strings.
- README and the example config explain the new section.
