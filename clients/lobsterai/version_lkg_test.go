package lobsterai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"client2api/internal/core"
)

// 钉住 version 的取值优先级（issue #178）：
//
//	本进程取到的动态值 > 上一进程的持久化 last-known-good > 配置常量。
//
// 「取不到就发 0.1.0」曾持续一周（244 次请求）：版本服务一不可达，
// 整个进程生命周期都自称一个从未发布过的版本号——比 stale 更可疑。
func TestVersionFallsBackToThePersistedLastKnownGood(t *testing.T) {
	dir := t.TempDir()

	// 上一进程留下 LKG：version.json 指向 2026.10.30。
	pv := persistedVersion{Version: "2026.10.30", SavedAt: "2026-11-04T00:00:00Z"}
	raw, err := json.Marshal(pv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, versionFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	// 版本端点不可达（模拟 issue 里的网络 EOF）：LKG 必须顶上。
	c := newVersionClient(t, dir, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			http.Error(w, "unreachable", http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	if got := c.clientVersion(t.Context()); got != "2026.10.30" {
		t.Fatalf("clientVersion = %q, want the persisted 2026.10.30（失败时必须用 last-known-good 而不是 0.1.0，issue #178）", got)
	}

	// 配置常量只在没有 LKG 时兜底。
	c2 := newVersionClient(t, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unreachable", http.StatusBadGateway)
	})
	if got := c2.clientVersion(t.Context()); got != defaultClientVersion {
		t.Fatalf("clientVersion = %q, want the configured fallback %q", got, defaultClientVersion)
	}
}

func TestVersionPersistAndSeedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := newVersionClient(t, dir, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"data":{"value":{"version":"2026.11.04"}}}`))
	})

	// 本进程取到动态值并落盘。
	c.refreshVersionIfStale(t.Context())
	if got := c.clientVersion(t.Context()); got != "2026.11.04" {
		t.Fatalf("clientVersion = %q, want the fetched 2026.11.04", got)
	}
	rawOnDisk, err := os.ReadFile(filepath.Join(dir, versionFile))
	if err != nil {
		t.Fatalf("version.json not persisted: %v", err)
	}
	var pv persistedVersion
	if err := json.Unmarshal(rawOnDisk, &pv); err != nil || pv.Version != "2026.11.04" {
		t.Fatalf("persisted = %s (%v), want version 2026.11.04", rawOnDisk, err)
	}

	// 新进程（同目录、端点不可达）：LKG 复活。
	c2 := newVersionClient(t, dir, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unreachable", http.StatusBadGateway)
	})
	if got := c2.clientVersion(t.Context()); got != "2026.11.04" {
		t.Fatalf("new process clientVersion = %q, want 2026.11.04（跨重启保持 last-known-good，issue #178）", got)
	}

	// 内存缓存新鲜时优先于磁盘（同进程内动态值不会倒退）。
	c.storeVersion("2026.11.05")
	if got := c.clientVersion(t.Context()); got != "2026.11.05" {
		t.Fatalf("clientVersion = %q, want the freshest 2026.11.05", got)
	}
}

func TestVersionLoadIgnoresCorruptFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, versionFile), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := newVersionClient(t, dir, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unreachable", http.StatusBadGateway)
	})
	if got := c.clientVersion(t.Context()); got != defaultClientVersion {
		t.Fatalf("corrupt file fell through to %q, want the fallback", got)
	}
}

// newVersionClient builds a Client with the given DataDir and upstream; the
// handler answers every request the constructor or the version refresh makes.
func newVersionClient(t *testing.T, dataDir string, handler func(w http.ResponseWriter, r *http.Request)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(srv.Close)
	cfg := map[string]any{
		"base_url":    srv.URL,
		"portal_url":  srv.URL,
		"version_url": srv.URL + "/version",
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	got, err := New(core.Deps{
		DataDir:    dataDir,
		Config:     raw,
		HTTPClient: srv.Client(),
		Logf:       func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c, ok := got.(*Client)
	if !ok {
		t.Fatalf("New returned %T, want *Client", got)
	}
	return c
}
