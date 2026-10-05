package modelmeta

import "strings"

// extraIntKeys reads the first positive integer from a list of candidate keys.
// The caller's configured key comes first, then the common spellings emitted by
// other client modules. A missing or unusable configured value therefore falls
// through to a compatible alias instead of losing the live metadata.
func extraIntKeys(extra map[string]any, keys ...string) int64 {
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if v := extraInt(extra[key]); v > 0 {
			return v
		}
	}
	return 0
}
