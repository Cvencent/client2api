package panel

import (
	"testing"

	"client2api/internal/core"
)

// TestDecorateAccountNotesReadsTheRegistry pins the panel half of the operator's
// own per-account label.  The vendor's user-info reply carries a nickname, not
// the phone number or e-mail the credential was issued to, so the operator
// records that identity themselves and the panel has to put it on the record it
// serves -- the row and the re-login button both read it.
//
// The module's own Label must survive: the frontend decides which of the two to
// show, so the decorator only adds OperatorNote and never rewrites Label.
func TestDecorateAccountNotesReadsTheRegistry(t *testing.T) {
	r := core.NewRegistry()
	r.SetPlatformConfigs(map[string]core.PlatformConfig{
		"trae": {AccountNotes: map[string]string{
			"a1": "  13800138000  ",
			"a2": "ops@example.com",
			"a4": "   ",
		}},
	})
	p := &panel{opts: Options{Registry: r}}
	list := []core.AccountRecord{
		{ID: "a1", Label: "用户47324619215"},
		{ID: "a2"},
		{ID: "a3", Label: "keep me"},
		{ID: "a4"},
	}
	p.decorateAccountNotes("trae", list)

	if list[0].OperatorNote != "13800138000" {
		t.Errorf("a1 operator note = %q, want the trimmed 13800138000", list[0].OperatorNote)
	}
	if list[0].Label != "用户47324619215" {
		t.Errorf("the module's label was rewritten to %q; the decorator must only add OperatorNote", list[0].Label)
	}
	if list[1].OperatorNote != "ops@example.com" {
		t.Errorf("a2 operator note = %q, want ops@example.com", list[1].OperatorNote)
	}
	if list[2].OperatorNote != "" || list[2].Label != "keep me" {
		t.Errorf("a3 has no note configured and must be untouched: %+v", list[2])
	}
	if list[3].OperatorNote != "" {
		t.Errorf("a blank note must be treated as absent, got %q", list[3].OperatorNote)
	}
}

// A module the config never mentions, or a panel with no registry at all, must
// stay exactly as it was: this is optional metadata, never a reason to fail a
// listing.
func TestDecorateAccountNotesWithoutARegistryIsANoOp(t *testing.T) {
	list := []core.AccountRecord{{ID: "a1", Label: "unchanged"}}
	(&panel{}).decorateAccountNotes("trae", list)
	if list[0].OperatorNote != "" || list[0].Label != "unchanged" {
		t.Fatalf("panel without a registry mutated the record: %+v", list[0])
	}
}
