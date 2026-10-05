package modelmeta

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOverrideStorePersistsAndDeletes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model_context.json")
	s, err := OpenOverrideStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("zcode", "glm-5.3-flash", 1048576, 131072); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get("zcode", "glm-5.3-flash")
	if !ok || got.ContextLength != 1048576 || got.MaxOutputTokens != 131072 {
		t.Fatalf("got %+v ok=%v", got, ok)
	}

	reopened, err := OpenOverrideStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := reopened.Get("zcode", "glm-5.3-flash"); !ok || got.ContextLength != 1048576 || got.MaxOutputTokens != 131072 {
		t.Fatalf("reopened %+v ok=%v", got, ok)
	}
	if err := reopened.Set("zcode", "glm-5.3-flash", 0, 131072); err != nil {
		t.Fatal(err)
	}
	if got, ok := reopened.Get("zcode", "glm-5.3-flash"); !ok || got.ContextLength != 0 || got.MaxOutputTokens != 131072 {
		t.Fatalf("partial update %+v ok=%v", got, ok)
	}
	if err := reopened.Delete("zcode", "glm-5.3-flash"); err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Get("zcode", "glm-5.3-flash"); ok {
		t.Fatal("delete did not remove entry")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) == "" {
		t.Fatal("store wrote an empty file")
	}
}

func TestOverrideStoreCorruptFileIsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model_context.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var warned bool
	s, err := OpenOverrideStore(path, func(string, ...any) { warned = true })
	if err != nil {
		t.Fatal(err)
	}
	if !warned {
		t.Fatal("corrupt file did not warn")
	}
	if _, ok := s.Get("zcode", "glm-5.3-flash"); ok {
		t.Fatal("corrupt file produced an entry")
	}
}
