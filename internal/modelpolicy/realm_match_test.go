package modelpolicy

import "testing"

// TestRealmQualifiedIDsAreGatedByABareBlacklist pins the fix for a silent
// policy failure: a realm-aware module (qoder, workbuddy) publishes
// "<realm>:<model>" on the wire, but its account-model dialog lists the BARE id
// for the account's own realm.  The operator's blacklist therefore holds "auto"
// while the gateway asks about "global:auto"; before this the store only
// compared exact strings and unchecking a model did nothing.
func TestRealmQualifiedIDsAreGatedByABareBlacklist(t *testing.T) {
	s, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("qoder", "qoder-acct-1", []string{"auto"}); err != nil {
		t.Fatal(err)
	}

	for _, qualified := range []string{"global:auto", "cn:auto"} {
		if !s.IsDisabled("qoder", "qoder-acct-1", qualified) {
			t.Errorf("a blacklist holding %q did NOT gate %q: unchecking a model would leave it routable", "auto", qualified)
		}
	}
	// The bare id itself still matches.
	if !s.IsDisabled("qoder", "qoder-acct-1", "auto") {
		t.Error("the bare id did not gate itself")
	}
	// A different model is untouched.
	if s.IsDisabled("qoder", "qoder-acct-1", "global:efficient") {
		t.Error("a blacklist for one model gated another")
	}
}

// TestRealmBlacklistGatesTheBareSpelling is the reverse spelling: if the
// operator somehow stored the qualified id, the bare wire id must still be
// gated (and vice versa).  Both spellings are honoured.
func TestRealmBlacklistGatesTheBareSpelling(t *testing.T) {
	s, _ := Open("")
	_ = s.Set("qoder", "c1", []string{"global:auto"})
	if !s.IsDisabled("qoder", "c1", "auto") {
		t.Error("a blacklist holding the qualified id did not gate the bare id")
	}
}

// TestAccountIDPrefixIsStrippedButRealmPrefixIsNot pins what the store DOES
// strip: the account id qualification ("<account>/<model>"), which is the
// openaicompat convention.  A realm prefix is stripped too now, but the account
// prefix rule is unchanged.
func TestAccountIDPrefixIsStrippedButRealmPrefixIsNot(t *testing.T) {
	s, _ := Open("")
	_ = s.Set("openaicompat", "prov", []string{"gpt-x"})
	if !s.IsDisabled("openaicompat", "prov", "prov/gpt-x") {
		t.Error("the <account>/ qualification was not stripped, so a provider's own blacklist cannot gate its model")
	}
	// A blacklist must not leak across accounts on the same platform.
	if s.IsDisabled("openaicompat", "other", "other/gpt-x") {
		t.Error("another account's blacklist gated this one")
	}
}

// TestAQualifierInTheMiddleIsNotStripped keeps the rule narrow: only a LEADING
// qualifier is removed, so a model id that contains a colon is still compared by
// its full leaf name and is never conflated with a different model.
func TestAQualifierInTheMiddleIsNotStripped(t *testing.T) {
	s, _ := Open("")
	_ = s.Set("qoder", "c1", []string{"nvidia/nemotron:free"})

	// Same model, realm-qualified on the wire: it IS the same model, so it is
	// gated.
	if !s.IsDisabled("qoder", "c1", "cn:nvidia/nemotron:free") {
		t.Error("a realm-qualified spelling of a blacklisted model was not gated")
	}
	// Same model, exact spelling: gated.
	if !s.IsDisabled("qoder", "c1", "nvidia/nemotron:free") {
		t.Error("the exact id did not gate itself")
	}
	// A DIFFERENT leaf with the same qualifier must not be gated: the internal
	// colon is not a realm separator, so only the leading "cn:" is ever removed.
	if s.IsDisabled("qoder", "c1", "cn:nemotron:free") {
		t.Error("a different model was gated because an interior colon was mistaken for a realm separator")
	}
	if s.IsDisabled("qoder", "c1", "cn:free") {
		t.Error("a different model was gated by a suffix match")
	}
}
