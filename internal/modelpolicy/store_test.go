package modelpolicy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStoreNormalizesAndAnswersLookups pins the one contract the gateway leans
// on: blacklists compare trimmed, lower-cased and deduplicated, and an unknown
// platform/account answers "not disabled" rather than erroring.
func TestStoreNormalizesAndAnswersLookups(t *testing.T) {
	s, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Set("lobsterai", "acct-1", []string{" Model-B ", "model-a", "MODEL-B", "", "  "}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Set("openaicompat", "groq", []string{"model-a"}); err != nil {
		t.Fatalf("Set openaicompat: %v", err)
	}
	got := s.Disabled("lobsterai", "acct-1")
	if strings.Join(got, ",") != "model-a,model-b" {
		t.Fatalf("Disabled = %v, want [model-a model-b]", got)
	}
	for _, tc := range []struct {
		platform, account, model string
		want                     bool
	}{
		{"lobsterai", "acct-1", "model-a", true},
		{"lobsterai", "acct-1", "  MODEL-A  ", true},
		{"lobsterai", "acct-1", "model-c", false},
		{"lobsterai", "acct-2", "model-a", false},
		{"zcode", "acct-1", "model-a", false},
		{"lobsterai", "acct-1", "", false},
		{"", "acct-1", "model-a", false},
		// openaicompat: the account record lists the bare model, the request
		// carries "<provider>/<model>", and both must match.
		{"openaicompat", "groq", "groq/model-a", true},
		{"openaicompat", "groq", "cerebras/model-a", false},
		{"openaicompat", "groq", "model-a", true},
	} {
		if got := s.IsDisabled(tc.platform, tc.account, tc.model); got != tc.want {
			t.Errorf("IsDisabled(%q, %q, %q) = %v, want %v", tc.platform, tc.account, tc.model, got, tc.want)
		}
	}
	// The caller must not be able to mutate the store through the returned slice.
	got[0] = "zzz"
	if again := s.Disabled("lobsterai", "acct-1"); again[0] != "model-a" {
		t.Errorf("Disabled returned a shared slice: %v", again)
	}
}

// TestStoreSurvivesRestart is the operator's actual requirement: a toggle made
// in the panel still holds after the process restarts, and turning every model
// back on leaves no entry behind.
func TestStoreSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account_model_policies.json")
	first, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := first.Set("trae", "acct-1", []string{"model-a"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := first.Set("trae", "acct-2", []string{"model-b"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !second.IsDisabled("trae", "acct-1", "model-a") || !second.IsDisabled("trae", "acct-2", "model-b") {
		t.Fatalf("policy did not survive reopen: %+v", second.Snapshot())
	}
	if err := second.Set("trae", "acct-1", nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(raw), "acct-1") {
		t.Errorf("cleared account still present in %s", raw)
	}
	third, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after clear: %v", err)
	}
	if third.IsDisabled("trae", "acct-1", "model-a") {
		t.Error("cleared account came back disabled")
	}
}

// TestStoreIgnoresCorruptFile: a mangled file must not stop the process from
// starting, and the operator's next save must be able to overwrite it.
func TestStoreIgnoresCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account_model_policies.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var warned string
	s, err := Open(path, func(format string, args ...any) {
		warned = format
		_ = args
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if warned == "" {
		t.Error("corrupt file was not reported through the warning function")
	}
	if s.IsDisabled("trae", "acct-1", "model-a") {
		t.Error("corrupt file produced a disabled entry")
	}
	if err := s.Set("trae", "acct-1", []string{"model-a"}); err != nil {
		t.Fatalf("Set after corrupt file: %v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !reopened.IsDisabled("trae", "acct-1", "model-a") {
		t.Error("save after a corrupt file did not take")
	}
}

// TestStoreReplaceIsAuthoritative: the bundle restore replaces the whole
// document, so entries the incoming snapshot lacks must go away.
func TestStoreReplaceIsAuthoritative(t *testing.T) {
	s, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Set("trae", "old", []string{"model-a"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Replace(map[string]map[string][]string{
		"trae": {"new": {"model-b"}},
	}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if s.IsDisabled("trae", "old", "model-a") {
		t.Error("Replace kept an entry the snapshot dropped")
	}
	if !s.IsDisabled("trae", "new", "model-b") {
		t.Error("Replace lost the incoming entry")
	}
	if snap := s.Snapshot(); len(snap) != 1 || len(snap["trae"]) != 1 {
		t.Errorf("Snapshot = %+v", snap)
	}
}
