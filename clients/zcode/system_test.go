package zcode

import (
	"encoding/json"
	"strings"
	"testing"
)

// The vendor risk-controls the SHAPE of the system array, not just its text.
// The official 3.16.3 client sends exactly three blocks and puts the
// "- You are powered by the model named ..." line INSIDE the environment block.
// Appending it as a fourth block -- which buildSystem used to do -- is answered
// with HTTP 405 envelope code 3012, the same "unusual activity" refusal as a WAF
// block, on a request that is otherwise byte-identical to a working one.
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

// A user-supplied system message is appended as its own block (that is the
// caller text, not the vendor preamble), so the invariant is "no synthetic
// powered-by block", not a hard 3.
func TestBuildSystemAppendsCallerTextAsBlock(t *testing.T) {
	var cfg Config
	blocks := buildSystem(&cfg, []string{"be brief"}, "GLM-5.3-Flash")
	if len(blocks) != 4 {
		t.Fatalf("blocks = %d, want 4 (3 preamble + 1 caller)", len(blocks))
	}
	last := string(blocks[3])
	if !strings.Contains(last, "be brief") {
		t.Errorf("caller text missing: %s", last)
	}
}
