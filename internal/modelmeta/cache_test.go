package modelmeta

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestStoreMissingFileIsAnEmptyCache(t *testing.T) {
	dir := t.TempDir()
	s := OpenStore(dir, "workbuddy")
	if m, ok := s.Get("glm-5.2"); ok {
		t.Fatalf("Get on a fresh store = %+v, want not-found", m)
	}
	if s.Len() != 0 {
		t.Fatalf("Len() = %d, want 0", s.Len())
	}
	if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
		t.Fatalf("a read must not create the cache file: %v", err)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := OpenStore(dir, "workbuddy")
	if !s.Put("glm-5.2", Meta{
		ContextLength:   1000000,
		MaxOutputTokens: 131072,
		Efforts:         []string{"high", "xhigh"},
		DefaultEffort:   "high",
		Source:          SourceStatic,
	}) {
		t.Fatal("Put reported no change on an empty cache")
	}
	m, ok := s.Get("glm-5.2")
	if !ok {
		t.Fatal("the value did not come back")
	}
	if m.ContextLength != 1000000 || m.MaxOutputTokens != 131072 {
		t.Fatalf("round trip = %d/%d", m.ContextLength, m.MaxOutputTokens)
	}
	if len(m.Efforts) != 2 || m.DefaultEffort != "high" {
		t.Fatalf("efforts did not survive the round trip: %+v", m)
	}
	if m.Source != SourceCache || m.SourceOf(FieldContextLength) != SourceCache {
		t.Fatalf("a cache hit must report %q, got %q/%q", SourceCache, m.Source, m.SourceOf(FieldContextLength))
	}

	// A second Store over the same file sees the persisted value.
	reopened := OpenStore(dir, "workbuddy")
	if got, ok := reopened.Get("glm-5.2"); !ok || got.ContextLength != 1000000 {
		t.Fatalf("reopened store = %+v ok=%v", got, ok)
	}
	// And re-Putting the same value is a no-op, so a lookup cannot hammer the disk.
	if reopened.Put("glm-5.2", Meta{ContextLength: 1000000, MaxOutputTokens: 131072, Efforts: []string{"high", "xhigh"}, DefaultEffort: "high", Source: SourceStatic}) {
		t.Fatal("Put of an identical value should report no change")
	}
}

func TestStoreDefaultEffortMustBeSupported(t *testing.T) {
	dir := t.TempDir()
	s := OpenStore(dir, "x")
	s.Put("m", Meta{Efforts: []string{"low"}, DefaultEffort: "high", Source: SourceStatic})
	m, ok := s.Get("m")
	if !ok {
		t.Fatal("value missing")
	}
	if m.DefaultEffort != "" {
		t.Fatalf("DefaultEffort = %q, want it dropped (not in %v)", m.DefaultEffort, m.Efforts)
	}
}

func TestStoreCorruptFileIsIgnoredAndHeals(t *testing.T) {
	dir := t.TempDir()
	s := OpenStore(dir, "workbuddy")
	if err := os.WriteFile(s.Path(), []byte("{{{ this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var warns atomic.Int64
	s.SetWarnf(func(string, ...any) { warns.Add(1) })

	if m, ok := s.Get("glm-5.2"); ok {
		t.Fatalf("a corrupt cache reported %+v, want not-found", m)
	}
	if warns.Load() == 0 {
		t.Fatal("a corrupt cache should warn")
	}
	// The store must still be usable and must overwrite the bad file.
	s.Put("new-model", Meta{ContextLength: 10, Source: SourceStatic})
	if got, ok := s.Get("new-model"); !ok || got.ContextLength != 10 {
		t.Fatalf("the store did not recover: %+v ok=%v", got, ok)
	}
	if reopened := OpenStore(dir, "workbuddy"); func() bool { m, ok := reopened.Get("new-model"); return !ok || m.ContextLength != 10 }() {
		t.Fatal("the healed file did not persist")
	}
}

func TestStoreWrongVersionIsIgnored(t *testing.T) {
	dir := t.TempDir()
	s := OpenStore(dir, "x")
	if err := os.WriteFile(s.Path(), []byte(`{"version":99,"client":"x","models":{"m":{"context_length":5}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if m, ok := s.Get("m"); ok {
		t.Fatalf("a future cache version was read anyway: %+v", m)
	}
}

func TestStoreUnreadablePathIsAbsentNotFatal(t *testing.T) {
	dir := t.TempDir()
	// A directory where the file should be: reading it fails with something other
	// than "not exist", which must degrade to an empty cache.
	if err := os.MkdirAll(filepath.Join(dir, CacheFileName("x")), 0o700); err != nil {
		t.Fatal(err)
	}
	s := OpenStore(dir, "x")
	s.SetWarnf(func(string, ...any) {})
	if m, ok := s.Get("m"); ok {
		t.Fatalf("Get = %+v, want not-found", m)
	}
}

func TestStoreAtomicWriteLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	s := OpenStore(dir, "workbuddy")
	s.Put("a", Meta{ContextLength: 1, Source: SourceStatic})
	s.Put("b", Meta{ContextLength: 2, Source: SourceStatic})
	s.Put("b", Meta{ContextLength: 3, Source: SourceStatic})

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("cache dir holds %v, want exactly the cache file", names)
	}
	if got, want := entries[0].Name(), CacheFileName("workbuddy"); got != want {
		t.Fatalf("cache file = %q, want %q", got, want)
	}
	if strings.Contains(strings.ToLower(entries[0].Name()), ".tmp") {
		t.Fatalf("a temp file survived: %q", entries[0].Name())
	}

	if runtime.GOOS != "windows" {
		// Windows only maps the read-only bit, so the mode is not portable there.
		info, err := os.Stat(s.Path())
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("cache mode = %o, want 600", perm)
		}
	}
}

func TestStoreDelete(t *testing.T) {
	dir := t.TempDir()
	s := OpenStore(dir, "x")
	s.Put("m", Meta{ContextLength: 1, Source: SourceStatic})
	if !s.Delete("m") {
		t.Fatal("Delete reported nothing removed")
	}
	if s.Delete("m") {
		t.Fatal("Delete of a removed key reported a removal")
	}
	if _, ok := s.Get("m"); ok {
		t.Fatal("the value survived deletion")
	}
}

func TestStoreMemoryOnly(t *testing.T) {
	s := OpenStore("", "x")
	if s.Path() != "" {
		t.Fatalf("Path() = %q, want empty for a memory-only store", s.Path())
	}
	if _, err := os.Stat(CacheFileName("x")); err == nil {
		t.Fatal("a memory-only store wrote to the working directory")
	}
	s.Put("m", Meta{ContextLength: 5, Source: SourceStatic})
	if m, ok := s.Get("m"); !ok || m.ContextLength != 5 {
		t.Fatalf("memory-only store = %+v ok=%v", m, ok)
	}
}

func TestStoreRejectsZeroMeta(t *testing.T) {
	s := OpenStore("", "x")
	if s.Put("m", Meta{}) {
		t.Fatal("Put of a zero Meta should be a no-op")
	}
	if _, ok := s.Get("m"); ok {
		t.Fatal("a zero Meta was cached")
	}
}

func TestStoreNilSafe(t *testing.T) {
	var s *Store
	if m, ok := s.Get("m"); ok {
		t.Fatalf("nil store returned %+v", m)
	}
	if s.Put("m", Meta{ContextLength: 1}) {
		t.Fatal("nil store reported a change")
	}
	if s.Len() != 0 || s.Path() != "" || s.Delete("m") {
		t.Fatal("nil store is not inert")
	}
	s.SetWarnf(func(string, ...any) {})
}

func TestStoreConcurrentUse(t *testing.T) {
	dir := t.TempDir()
	s := OpenStore(dir, "x")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.Put("shared", Meta{ContextLength: int64(i + 1), Source: SourceStatic})
			s.Get("shared")
			s.Len()
		}(i)
	}
	wg.Wait()
	if m, ok := s.Get("shared"); !ok || m.ContextLength <= 0 {
		t.Fatalf("concurrent use left the store in a bad state: %+v ok=%v", m, ok)
	}
}

func TestCacheFileNameSanitises(t *testing.T) {
	cases := map[string]string{
		"workbuddy": "modelmeta-workbuddy.json",
		"":          "modelmeta-default.json",
		"..":        "modelmeta-default.json",
		"a/b\\c:d":  "modelmeta-a_b_c_d.json",
		"kimi 2":    "modelmeta-kimi_2.json",
	}
	for in, want := range cases {
		if got := CacheFileName(in); got != want {
			t.Errorf("CacheFileName(%q) = %q, want %q", in, got, want)
		}
	}
}
