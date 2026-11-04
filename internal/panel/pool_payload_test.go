package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// 钉住 /panel/api/pool 的三件事（issue #192）：
//
//  1. 每账号载荷只含读数键——base_url/source 这类静态配置字段不许再出现在
//     5 秒轮询里（页面零读取，曾占响应字节的 1/3）；
//  2. 平台的每账号上限只说一次（client 级 in_flight_limit），不再重复 N 遍；
//     账号自己报了更紧的上限时才随该账号走；
//  3. 强 ETag + If-None-Match：空闲时（没有账号的 in_flight 变化）回 304
//     零正文。
func TestPoolPollCarriesOnlyReadingKeys(t *testing.T) {
	// 一个中转站形状的模块：extra 里塞满静态配置 + 唯一的读数键。
	accts := []core.AccountStatus{
		{ID: "acct-1", Enabled: true, State: "ready", Extra: map[string]any{
			"base_url":  "https://relay.example/v1",
			"source":    "configured",
			"in_flight": 1,
		}},
		{ID: "acct-2", Enabled: true, State: "ready", Extra: map[string]any{
			"base_url":  "https://relay.example/v1",
			"source":    "configured",
			"in_flight": 0,
		}},
	}
	c := &fakeClient{name: "relay", status: core.Status{Name: "relay", Ready: true, Accounts: accts}}
	reg := registryOf(c)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{"relay": {MaxInFlightPerAccount: 3}})
	h := New(Options{
		Registry: reg,
		Started:  time.Now(),
		InFlight: fakeTracker{"relay\x1facct-1": 1, "relay\x1facct-2": 0},
	})

	rec := get(t, h, "/panel/api/pool")
	if rec.Code != http.StatusOK {
		t.Fatalf("pool = %d (%s)", rec.Code, rec.Body.String())
	}
	var snap struct {
		Clients []struct {
			Name          string `json:"name"`
			InFlightLimit *int   `json:"in_flight_limit"`
			Account       []struct {
				ID    string         `json:"id"`
				Extra map[string]any `json:"extra"`
			} `json:"accounts"`
		} `json:"clients"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(snap.Clients) != 1 {
		t.Fatalf("clients = %d, want 1", len(snap.Clients))
	}
	row := snap.Clients[0]

	// client 级上限只说一次。
	if row.InFlightLimit == nil || *row.InFlightLimit != 3 {
		t.Fatalf("client in_flight_limit = %v, want 3（平台上限只说一次，issue #192）", row.InFlightLimit)
	}

	allowed := map[string]bool{"in_flight": true, "in_flight_limit": true, "model_cooldowns": true, "realm": true}
	for _, a := range row.Account {
		for k := range a.Extra {
			if !allowed[k] {
				t.Errorf("账号 %q 的 pool extra 带了非读数键 %q：静态字段不许进 5 秒轮询（issue #192）", a.ID, k)
			}
		}
		if a.Extra["in_flight"] == nil {
			t.Errorf("账号 %q 丢了 in_flight 读数", a.ID)
		}
		// 平台级上限不逐账号重复。
		if v, ok := a.Extra["in_flight_limit"]; ok {
			if v != float64(3) {
				t.Errorf("账号 %q 的 in_flight_limit = %v：平台级数字不许逐账号重复（issue #192）", a.ID, v)
			}
		}
	}
}

func TestPoolHonorsIfNoneMatch(t *testing.T) {
	accts := []core.AccountStatus{
		{ID: "acct-1", Enabled: true, State: "ready", Extra: map[string]any{"in_flight": 1}},
	}
	c := &fakeClient{name: "zcode", status: core.Status{Name: "zcode", Ready: true, Accounts: accts}}
	reg := registryOf(c)
	h := New(Options{Registry: reg, Started: time.Now(), InFlight: fakeTracker{"zcode\x1facct-1": 1}})

	first := get(t, h, "/panel/api/pool")
	if first.Code != http.StatusOK {
		t.Fatalf("first poll = %d", first.Code)
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("pool 没有 ETag：空闲时 5 秒一拍的完整 body 白传（issue #192）")
	}

	req := httptest.NewRequest(http.MethodGet, "/panel/api/pool", nil)
	req.Header.Set("If-None-Match", etag)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match = %d, want 304（空闲时必须零正文，issue #192）body=%s", rec.Code, rec.Body.String())
	}
}

func TestPoolPayloadShrinksOnRelayShapes(t *testing.T) {
	// 守护：100 个 relay 形状账号的 pool 载荷里，静态字段必须为 0 次出现。
	accts := make([]core.AccountStatus, 0, 100)
	for i := 0; i < 100; i++ {
		accts = append(accts, core.AccountStatus{ID: "acct-" + strconv.Itoa(i), Enabled: true, State: "ready",
			Extra: map[string]any{"base_url": "https://relay.example/v1", "source": "configured", "in_flight": 0}})
	}
	c := &fakeClient{name: "relay", status: core.Status{Name: "relay", Ready: true, Accounts: accts}}
	reg := registryOf(c)
	h := New(Options{Registry: reg, Started: time.Now(), InFlight: fakeTracker{}})
	rec := get(t, h, "/panel/api/pool")
	body := rec.Body.String()
	if strings.Count(body, "base_url") != 0 || strings.Count(body, `"source"`) != 0 {
		t.Errorf("pool 载荷仍含静态键：base_url/source 出现在 5 秒轮询里（issue #192）")
	}
}
