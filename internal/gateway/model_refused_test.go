package gateway

import (
	"net/http"
	"testing"

	"client2api/internal/core"
)

// A model-scoped refusal ("this account type cannot use this model", code
// 11158 from workbuddy) must fail over to the next platform in a bare-model /
// model-group chain: the account is healthy, the model exists elsewhere.
func TestModelRefusedKindFailsOver(t *testing.T) {
	err := core.Fail("workbuddy", "acct", core.FailureModelRefused, http.StatusBadRequest, errString("model [gpt-6-astra] is not allowed for user type"))
	if !modelRefusal(err) {
		t.Fatalf("modelRefusal = false, want true for a FailureModelRefused 400")
	}
	if core.Retryable(core.FailureModelRefused) {
		t.Fatal("FailureModelRefused must not be retryable on the same platform")
	}
}

type errString string

func (e errString) Error() string { return string(e) }