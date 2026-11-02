package tabbit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"client2api/internal/core"
)

// timeouts.cache_seconds 的读取点（issue #115）。
//
// 这个键有字段、有注释、有 README 行，`cacheTTL()` 也照着定义好了，但全仓零调用：
// 面板每 5 秒读一次 /panel/api/status，每一拍都真的重打 sidecar 的 /health，尽管
// 答案在这个窗口里不可能变。对照组是同一 struct 的 models_cache_seconds，它真的被
// 用来判断目录缓存是否过期。

// countingSidecar 与 fakeSidecar 同形，但记录 /health 命中次数，并允许测试中途
// 切换健康状态。
func countingSidecar(t *testing.T) (*httptest.Server, *atomic.Int32, *atomic.Bool) {
	t.Helper()
	var hits atomic.Int32
	var healthy atomic.Bool
	healthy.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		hits.Add(1)
		if !healthy.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"version":"9.9.9","models":3,"web_host":"web.tabbit.ai"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits, &healthy
}

func clientForCacheTest(t *testing.T, baseURL string, cacheSeconds float64) *Client {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"base_url": baseURL,
		"timeouts": map[string]any{"cache_seconds": cacheSeconds},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	got, err := New(core.Deps{
		DataDir: t.TempDir(),
		Config:  body,
		Logf:    func(string, ...any) {},
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

// TestStatusReusesAHealthProbeWithinCacheSeconds 钉住主症状：window 内多次读状态
// 只应打一次 sidecar。
func TestStatusReusesAHealthProbeWithinCacheSeconds(t *testing.T) {
	srv, hits, _ := countingSidecar(t)
	c := clientForCacheTest(t, srv.URL, 300) // 300s << 一轮测试的时间

	for i := 0; i < 8; i++ {
		if st := c.Status(context.Background()); !st.Ready {
			t.Fatalf("状态 %d 未就绪：%s", i, st.Detail)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("8 次状态读取打了 %d 次 /health，期望 1 次（cache_seconds=300 必须生效，issue #115）", got)
	}
}

// TestStatusProbesAgainAfterTheCacheExpires 钉住窗口过期后必须重新问，否则
// 「sidecar 挂了」会永远看不到。
func TestStatusProbesAgainAfterTheCacheExpires(t *testing.T) {
	srv, hits, _ := countingSidecar(t)
	c := clientForCacheTest(t, srv.URL, 0.05) // 50ms

	_ = c.Status(context.Background())
	time.Sleep(90 * time.Millisecond)
	_ = c.Status(context.Background())

	if got := hits.Load(); got != 2 {
		t.Fatalf("/health 命中 %d 次，期望 2 次（窗口过后必须重新探测）", got)
	}
}

// TestFailureIsNeverCached 钉住只缓存成功：sidecar 恢复必须下一拍就能看到。
func TestFailureIsNeverCached(t *testing.T) {
	srv, hits, healthy := countingSidecar(t)
	c := clientForCacheTest(t, srv.URL, 300)

	// 先让它坏着（countingSidecar 默认是健康的）。
	healthy.Store(false)
	if st := c.Status(context.Background()); st.Ready {
		t.Fatal("sidecar 回了 500 却被报成 ready")
	}
	// 失败不进缓存：修好后下一拍就该 ready，而不是等满 300 秒。
	healthy.Store(true)
	if st := c.Status(context.Background()); !st.Ready {
		t.Fatalf("sidecar 已恢复但状态仍是未就绪：失败被缓存了（hits=%d）", hits.Load())
	}
}

// TestCacheIsKeyedOnTheSidecarAddress 钉住换 sidecar 立刻生效：不该读到上一个
// sidecar 的判决。
func TestCacheIsKeyedOnTheSidecarAddress(t *testing.T) {
	first, _, _ := countingSidecar(t)
	second, secondHits, _ := countingSidecar(t)

	c := clientForCacheTest(t, first.URL, 300)
	if st := c.Status(context.Background()); !st.Ready {
		t.Fatalf("首个 sidecar 未就绪：%s", st.Detail)
	}
	// 直接把模块指向另一个 sidecar（模拟配置改动后热加载）。
	c.mu.Lock()
	c.cfg.BaseURL = second.URL
	c.mu.Unlock()

	_ = c.Status(context.Background())
	if got := secondHits.Load(); got == 0 {
		t.Fatal("换了 sidecar 却直接复用了旧地址的探测结果：缓存没有按地址分键（issue #115）")
	}
}

// TestReadinessPathsBypassTheCache 钉住缓存只管读数：登录/就绪探测必须看到当下
// 真相，不能被缓存喂成假成功。
func TestReadinessPathsBypassTheCache(t *testing.T) {
	srv, hits, _ := countingSidecar(t)
	c := clientForCacheTest(t, srv.URL, 300)

	// 先让缓存里有一份 ok。
	if st := c.Status(context.Background()); !st.Ready {
		t.Fatalf("状态未就绪：%s", st.Detail)
	}
	before := hits.Load()
	// 直接走 probe（就绪 / 登录路径用的就是它），必须真的再打一次。
	loc := c.locate()
	if st := c.probe(context.Background(), loc, c.cfg.Timeouts.healthBudget()); !st.ok {
		t.Fatal("probe 直接探测失败")
	}
	if hits.Load() != before+1 {
		t.Fatalf("probe 被缓存接管了：命中数 %d → %d，就绪路径必须绕过缓存（issue #115）", before, hits.Load())
	}
}
