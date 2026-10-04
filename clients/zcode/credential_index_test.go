package zcode

import (
	"testing"
)

// TestCredentialIndexPrefersTheCompleteSecretOverTheTruncatedCopy pins the
// discovery order rule: config.json carries only the id half, credentials.json
// carries the complete id.secret.  Both have the same id, so a first-wins index
// would hand the pool a 32-character key that cannot authenticate and then
// discard the complete one as a duplicate.
func TestCredentialIndexPrefersTheCompleteSecretOverTheTruncatedCopy(t *testing.T) {
	const complete = "61161790588087632.abcdefghijklmnop"
	idx := providerSecrets{byID: map[string]string{"61161790588087632": "61161790588087632"}}
	mergeProviderSecret(&idx, "61161790588087632", complete, "61161790588087632")
	if got := idx.byID["61161790588087632"]; got != complete {
		t.Fatalf("byID = %q, want the complete secret", got)
	}
}
