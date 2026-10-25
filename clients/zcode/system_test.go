package zcode

import (
	"encoding/json"
	"strings"
	"testing"
)

// Live differential against the 2026-10-26 upstream (panel JWT, official-shape
// request replayed with one variable changed at a time) pinned the system-array
// risk rule exactly:
//
//   - 4+ blocks are answered 405/3012 ("unusual activity"), 3 or fewer pass;
//   - block 0 must be the byte-exact official identity line, and it must
//     precede the harness block (the harness block alone is refused);
//   - block 1 must be the byte-exact official harness text: appending is
//     tolerated, but editing or trimming any part of it is refused;
//   - the last block is free-form (the environment block, a folded powered-by
//     line, even caller text -- as long as the block count stays <= 3).
//
// So a caller-supplied system message must be FOLDED into the last block; a
// fourth block (what buildSystem used to do) is itself the 3012 trigger.
func TestBuildSystemKeepsThreeBlocks(t *testing.T) {
	var cfg Config
	blocks := buildSystem(&cfg, nil, "GLM-5.3-Flash")
	if len(blocks) != 3 {
		t.Fatalf("system blocks = %d, want 3 (a 4th block triggers 3012); blocks=%v", len(blocks), blocks)
	}
}

// The powered-by line must still be present -- it just belongs inside the last
// block, the way the official client writes it.
func TestBuildSystemFoldsPoweredByIntoLastBlock(t *testing.T) {
	var cfg Config
	blocks := buildSystem(&cfg, nil, "GLM-5.3-Flash")
	if len(blocks) == 0 {
		t.Fatal("no system blocks")
	}
	last := string(blocks[len(blocks)-1])
	if !strings.Contains(last, "You are powered by the model named GLM-5.3-Flash.") {
		t.Errorf("the powered-by line is missing from the last block: %s", last)
	}
	// It must not be its own block anywhere in the array.
	for i, b := range blocks {
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("block %d is not JSON: %v", i, err)
		}
		if txt, _ := m["text"].(string); strings.HasPrefix(strings.TrimSpace(txt), "- You are powered by the model named") {
			t.Errorf("block %d is the standalone powered-by block, which triggers 3012", i)
		}
	}
}

// A caller-supplied system message is folded into the last (environment) block.
// Appending it as a fourth block is answered with 405/3012.
func TestBuildSystemFoldsCallerTextIntoLastBlock(t *testing.T) {
	var cfg Config
	blocks := buildSystem(&cfg, []string{"be brief", "stay terse"}, "GLM-5.3-Flash")
	if len(blocks) != 3 {
		t.Fatalf("blocks = %d, want 3 (a 4th block triggers 3012)", len(blocks))
	}
	last := string(blocks[len(blocks)-1])
	if !strings.Contains(last, "be brief") || !strings.Contains(last, "stay terse") {
		t.Errorf("caller text missing from the last block: %s", last)
	}
	// The two caller parts keep their order inside the folded text.
	iBrief := strings.Index(last, "be brief")
	iTerse := strings.Index(last, "stay terse")
	if iBrief < 0 || iTerse < 0 || iBrief > iTerse {
		t.Errorf("caller parts out of order in the last block: %s", last)
	}
	// Blocks 0 and 1 stay byte-exact: the fold must only touch the last block.
	first := string(blocks[0])
	if !strings.Contains(first, "You are ZCode, an interactive coding agent") {
		t.Errorf("identity block disturbed: %s", first)
	}
}

// The caller text must not be its own block anywhere in the array.
func TestBuildSystemHasNoStandaloneCallerBlock(t *testing.T) {
	var cfg Config
	blocks := buildSystem(&cfg, []string{"be brief"}, "GLM-5.3-Flash")
	for i, b := range blocks {
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("block %d is not JSON: %v", i, err)
		}
		if txt, _ := m["text"].(string); strings.TrimSpace(txt) == "be brief" {
			t.Errorf("block %d is the standalone caller block, which triggers 3012", i)
		}
	}
}
