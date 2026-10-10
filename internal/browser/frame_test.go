package browser

import (
	"encoding/json"
	"testing"
)

func TestFlattenFramesKeepsParentsBeforeChildren(t *testing.T) {
	tree := frameTree{
		Frame: struct {
			ID   string `json:"id"`
			URL  string `json:"url"`
			Name string `json:"name"`
		}{ID: "root", URL: "https://example.test/"},
	}
	child := frameTree{}
	child.Frame.ID = "child"
	child.Frame.URL = "https://passport.example.test/login"
	tree.ChildFrames = []frameTree{child}

	var got []Frame
	flattenFrames(tree, &got)
	if len(got) != 2 {
		t.Fatalf("frames = %#v, want root and child", got)
	}
	if got[0].ID != "root" || got[1].ID != "child" {
		t.Fatalf("order = %#v, want parent before child", got)
	}
}

func TestDecodeEvalResultReportsPageExceptions(t *testing.T) {
	raw := json.RawMessage(`{
		"exceptionDetails": {
			"text": "Uncaught",
			"exception": {"description": "ReferenceError: nope\n    at <anonymous>:1:1"}
		}
	}`)
	if _, err := decodeEvalResult(raw); err == nil {
		t.Fatal("decodeEvalResult accepted a page exception")
	}
}
