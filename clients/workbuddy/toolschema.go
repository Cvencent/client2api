package workbuddy

import (
	"encoding/json"
	"strings"
)

// WorkBuddy parses `tools[].function.parameters` into a strict typed model and
// answers HTTP 400 / business code 11129 ("invalid function call parameters")
// when a schema does not fit it.  The constructs rewritten here are valid JSON
// Schema that its parser refuses, so each one is turned into the equivalent
// shape it accepts:
//
//   - `items: [a, b]` (draft-4 tuple form; an empty array too) is rejected
//     outright, while `items: {anyOf: [...]}` is accepted;
//   - a root schema without `type` is rejected even when it declares
//     `properties`, and so is a root that is only a `$ref` or a union;
//   - `items: null` and a `properties` entry whose value is null are rejected;
//   - draft-4 boolean `exclusiveMinimum` / `exclusiveMaximum` are rejected,
//     the numeric draft-6 form is accepted;
//   - `required` must be an array of strings.
//
// The rewrites preserve what the caller declared; nothing is narrowed that
// would make the model emit arguments the tool cannot use.

// maxSchemaDepth bounds recursion.  A schema deeper than this is already
// pathological, and the vendor would not accept it either.
const maxSchemaDepth = 64

// normalizeToolSchema returns the WorkBuddy-accepted form of a decoded tool
// schema.  A schema that is not a JSON object becomes the canonical empty
// parameters, because a function whose parameters are not an object cannot be
// expressed on the wire at all.
func normalizeToolSchema(params any) any {
	root, ok := params.(map[string]any)
	if !ok || root == nil || len(root) == 0 {
		return emptyToolParameters()
	}
	root = resolveRootRef(root)
	root = mergeRootUnion(root)
	normalizeSchemaNode(root, 0)
	ensureRootType(root)
	// An object schema with no constraints left is exactly the canonical empty
	// parameters; publishing it that way keeps one wire shape for "no
	// arguments".
	if len(root) == 1 && hasTypeField(root) {
		if t, _ := root["type"].(string); strings.EqualFold(strings.TrimSpace(t), "object") {
			return emptyToolParameters()
		}
	}
	if len(root) == 0 {
		return emptyToolParameters()
	}
	return root
}

// resolveRootRef inlines a root-level `$ref`.  Nested references resolve
// upstream; only the root one is refused, so only it is rewritten.
func resolveRootRef(root map[string]any) map[string]any {
	ref, _ := root["$ref"].(string)
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return root
	}
	target, ok := lookupLocalRef(root, ref)
	if !ok {
		delete(root, "$ref")
		return root
	}
	merged := make(map[string]any, len(target)+len(root))
	for k, v := range target {
		merged[k] = v
	}
	for k, v := range root {
		if k == "$ref" {
			continue
		}
		merged[k] = v
	}
	return merged
}

// lookupLocalRef resolves a reference into the same document.  Anything else
// (a remote URI, a pointer through an array) is left unresolved and the caller
// drops it.
func lookupLocalRef(root map[string]any, ref string) (map[string]any, bool) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	var cur any = root
	for _, raw := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		seg := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[seg]
		if !ok {
			return nil, false
		}
	}
	obj, ok := cur.(map[string]any)
	return obj, ok
}

// mergeRootUnion folds a root-level `$ref`-sibling union into the root.  A root
// `anyOf`/`oneOf` cannot be expressed: the first object branch is the one that
// can actually describe a tool call, so it wins.  `allOf` is a conjunction, so
// every branch is merged.
func mergeRootUnion(root map[string]any) map[string]any {
	if branches, ok := root["allOf"].([]any); ok {
		out := make(map[string]any, len(root)+8)
		for k, v := range root {
			if k != "allOf" {
				out[k] = v
			}
		}
		for _, raw := range branches {
			mergeSchema(out, raw)
		}
		return out
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		branches, ok := root[key].([]any)
		if !ok {
			continue
		}
		out := make(map[string]any, len(root)+8)
		for k, v := range root {
			if k != key {
				out[k] = v
			}
		}
		for _, raw := range branches {
			if branch, ok := raw.(map[string]any); ok && isObjectish(branch) {
				mergeSchema(out, branch)
				break
			}
		}
		return out
	}
	return root
}

// mergeSchema copies a branch's keys into dst without clobbering what dst
// already declares.  `properties` is merged one level deep so an allOf of two
// object schemas keeps every property.
func mergeSchema(dst map[string]any, src any) {
	branch, ok := src.(map[string]any)
	if !ok {
		return
	}
	for k, v := range branch {
		if k == "properties" {
			want, ok := v.(map[string]any)
			if !ok {
				continue
			}
			have, _ := dst["properties"].(map[string]any)
			if have == nil {
				have = make(map[string]any, len(want))
				dst["properties"] = have
			}
			for pk, pv := range want {
				if _, exists := have[pk]; !exists {
					have[pk] = pv
				}
			}
			continue
		}
		if _, exists := dst[k]; !exists {
			dst[k] = v
		}
	}
}

// isObjectish reports whether a union branch describes an object, which is the
// only shape that can back a function call.
func isObjectish(branch map[string]any) bool {
	if t, ok := branch["type"].(string); ok {
		return strings.EqualFold(strings.TrimSpace(t), "object")
	}
	return hasKey(branch, "properties") || hasKey(branch, "required") ||
		hasKey(branch, "additionalProperties")
}

// normalizeSchemaNode rewrites the constructs the vendor's parser refuses, in
// place, for one schema node and everything below it.
func normalizeSchemaNode(node map[string]any, depth int) {
	if node == nil || depth > maxSchemaDepth {
		return
	}

	if raw, ok := node["properties"]; ok {
		if normalized, keep := normalizeProperties(raw, depth); keep {
			node["properties"] = normalized
		} else {
			delete(node, "properties")
		}
	}

	if raw, ok := node["items"]; ok {
		if normalized, keep := normalizeItems(raw, depth); keep {
			node["items"] = normalized
		} else {
			delete(node, "items")
		}
	}

	if raw, ok := node["required"]; ok {
		if normalized, keep := normalizeRequired(raw); keep {
			node["required"] = normalized
		} else {
			delete(node, "required")
		}
	}

	normalizeExclusiveBound(node, "exclusiveMinimum", "minimum")
	normalizeExclusiveBound(node, "exclusiveMaximum", "maximum")

	for _, key := range []string{"anyOf", "oneOf", "allOf", "prefixItems"} {
		if branches, ok := node[key].([]any); ok {
			node[key] = normalizeBranchList(branches, depth)
		}
	}
	for _, key := range []string{"$defs", "definitions", "patternProperties", "dependentSchemas"} {
		if defs, ok := node[key].(map[string]any); ok {
			normalizeSchemaMap(defs, depth)
		}
	}
	for _, key := range []string{"additionalProperties", "propertyNames", "not", "if", "then", "else", "contains"} {
		if child, ok := node[key].(map[string]any); ok {
			normalizeSchemaNode(child, depth+1)
		}
	}
}

// normalizeProperties returns the map with null and non-schema entries removed
// and every remaining subschema normalized.  A `properties` value that is not
// an object has no meaning on the wire and is dropped by the caller.
func normalizeProperties(raw any, depth int) (map[string]any, bool) {
	props, ok := raw.(map[string]any)
	if !ok {
		return nil, false
	}
	out := make(map[string]any, len(props))
	for name, value := range props {
		if name == "" {
			continue
		}
		switch v := value.(type) {
		case nil:
			// A null subschema is refused; the property is unusable.
		case bool:
			// `true` means "any value" and is accepted; `false` would forbid
			// every value, so it is dropped with the property.
			if v {
				out[name] = v
			}
		case map[string]any:
			normalizeSchemaNode(v, depth+1)
			out[name] = v
		}
	}
	return out, true
}

// normalizeItems turns every shape of `items` into one the vendor accepts and
// reports whether the key should survive.
func normalizeItems(raw any, depth int) (any, bool) {
	switch v := raw.(type) {
	case nil:
		return nil, false
	case []any:
		if len(v) == 0 {
			return nil, false
		}
		if len(v) == 1 {
			branch, ok := v[0].(map[string]any)
			if !ok {
				return nil, false
			}
			normalizeSchemaNode(branch, depth+1)
			return branch, true
		}
		branches := normalizeBranchList(v, depth)
		if len(branches) == 0 {
			return nil, false
		}
		return map[string]any{"anyOf": branches}, true
	case map[string]any:
		normalizeSchemaNode(v, depth+1)
		if !hasTypeField(v) && len(v) > 0 {
			switch {
			case hasKey(v, "properties"), hasKey(v, "required"), hasKey(v, "additionalProperties"):
				v["type"] = "object"
			case hasKey(v, "items"):
				v["type"] = "array"
			}
		}
		return v, true
	default:
		return nil, false
	}
}

// normalizeBranchList normalizes the subschemas of a union or tuple, dropping
// entries that are not objects.
func normalizeBranchList(branches []any, depth int) []any {
	out := make([]any, 0, len(branches))
	for _, raw := range branches {
		branch, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		normalizeSchemaNode(branch, depth+1)
		out = append(out, branch)
	}
	return out
}

// normalizeSchemaMap normalizes a map of named subschemas in place.
func normalizeSchemaMap(defs map[string]any, depth int) {
	for name, raw := range defs {
		child, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		normalizeSchemaNode(child, depth+1)
		defs[name] = child
	}
}

// normalizeRequired accepts the draft-4 string shorthand and filters anything
// that is not a property name.
func normalizeRequired(raw any) (any, bool) {
	switch v := raw.(type) {
	case string:
		name := strings.TrimSpace(v)
		if name == "" {
			return nil, false
		}
		return []any{name}, true
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			if name, ok := item.(string); ok && name != "" {
				out = append(out, name)
			}
		}
		if len(out) == 0 {
			return nil, false
		}
		return out, true
	default:
		return nil, false
	}
}

// normalizeExclusiveBound converts the draft-4 boolean form to the numeric form
// the vendor parses.  `true` with a numeric inclusive bound becomes that bound
// as an exclusive one; without one the keyword cannot be expressed and is
// dropped, as is `false`.
func normalizeExclusiveBound(node map[string]any, key, boundKey string) {
	raw, ok := node[key]
	if !ok {
		return
	}
	if flag, ok := raw.(bool); ok {
		if !flag {
			delete(node, key)
			return
		}
		if bound, ok := node[boundKey]; ok {
			if isNumber(bound) {
				node[key] = bound
				delete(node, boundKey)
				return
			}
		}
		delete(node, key)
		return
	}
	if !isNumber(raw) {
		delete(node, key)
	}
}

// ensureRootType gives the root a `type`, which the vendor's parser requires
// even when the schema only declares properties.
func ensureRootType(root map[string]any) {
	if hasTypeField(root) {
		return
	}
	switch {
	case hasKey(root, "properties"), hasKey(root, "required"),
		hasKey(root, "additionalProperties"), hasKey(root, "patternProperties"):
		root["type"] = "object"
	case hasKey(root, "items"):
		root["type"] = "array"
	default:
		root["type"] = "object"
	}
}

func hasKey(node map[string]any, key string) bool {
	_, ok := node[key]
	return ok
}

// hasTypeField reports whether a node declares a usable `type`.  A null or
// empty type is treated as absent.
func hasTypeField(node map[string]any) bool {
	raw, ok := node["type"]
	if !ok || raw == nil {
		return false
	}
	if s, ok := raw.(string); ok {
		return strings.TrimSpace(s) != ""
	}
	return true
}

func isNumber(v any) bool {
	switch v.(type) {
	case json.Number, float64, int, int64, uint64:
		return true
	}
	return false
}
