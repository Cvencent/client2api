package lobsterai

import (
	"testing"
)

// The vendor answers an exhausted free quota with HTTP 200 and this phrase
// inside the SSE payload. It must be classified as a hard credit failure so
// the account is parked and the gateway can fail over to a sibling platform.
func TestChineseFreeQuotaExhaustedIsHardCredit(t *testing.T) {
	got := classifyFailure(200, 0, "免费额度已用完，请升级套餐")
	if got != kindHardCredit {
		t.Fatalf("classifyFailure = %q, want %q", got, kindHardCredit)
	}
}
