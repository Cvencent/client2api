package workbuddy

import "testing"

// 11158 "model [x] is not allowed for user type [personal_free_user]" is the
// vendor's answer for a model this ACCOUNT TYPE cannot use. It is a
// model-scoped refusal — the account is healthy and the model exists on the
// platform for paying tiers — so it must classify as ErrModelBlocked (parked
// per-model, hint "switch model or retry on another account"), never as a
// generic client error that the gateway cannot fail over on.
func TestClassifyUserTypeModelBlock(t *testing.T) {
	body := `{"code":11158,"msg":"model [gpt-6-astra] is not allowed for user type [personal_free_user]","requestId":"x"}`
	kind := Classify(400, body)
	if kind != ErrModelBlocked {
		t.Fatalf("Classify(400, 11158 user-type block) = %v, want ErrModelBlocked", kind)
	}
}
