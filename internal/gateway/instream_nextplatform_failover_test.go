package gateway

import (
	"errors"
	"io"
	"log"
	"net/http"
	"testing"
	"time"

	"client2api/internal/core"
	"client2api/internal/livecfg"
)

// ---------------------------------------------------------------------------
// #245：nextPlatform 类失败（image_refused、model_refused）包在 HTTP 200 流的
// 错误帧里到达时，必须与 Chat 返回路径对称地改道——否则 #243 承诺的「一次
// 平台改道」只对「响应头前被拒」成立，错误帧晚到一步就回到人肉切平台。
// ---------------------------------------------------------------------------

// A：图片拒包在 200 流内（officeace 形状）→ 换到 healthy 平台服务。
func TestAInStreamImageRefusalFailsOverToNextPlatform(t *testing.T) {
	filtered := &testClient{
		name: "filtered",
		events: []core.Event{
			{Type: core.EventError, Err: core.Fail("filtered", "a1", core.FailureImageRefused, http.StatusBadRequest,
				errors.New("image_url.url field is unsafe")),
			},
		},
	}
	healthy := &testClient{
		name:     "healthy",
		servedBy: "h1",
		events:   usageEvents(3, 4),
	}

	reg := core.NewRegistry()
	reg.Add(filtered)
	reg.Add(healthy)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"filtered": {Priority: 1},
		"healthy":  {Priority: 2},
	})

	srv := NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(io.Discard, "", 0),
		Stats:    NewStats(),
		Usage:    NewUsageStore(10),
		Live: livecfg.New(livecfg.Snapshot{
			MaxRotate:         1,
			RotateBackoffBase: time.Nanosecond,
		}),
	})

	rec := chat(t, srv, bufferedBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if healthy.seen == nil {
		t.Fatal("the image-refused stream did not fail over: the healthy platform never received the request (issue #245)")
	}
}

// B：ModelRefused 的流内形态（「model not found」包在 200 流里）同缺口同修。
func TestBInStreamModelRefusalFailsOverToNextPlatform(t *testing.T) {
	refused := &testClient{
		name: "refused",
		events: []core.Event{
			{Type: core.EventError, Err: core.Fail("refused", "a1", core.FailureModelRefused, http.StatusNotFound,
				errors.New("model not found on this relay")),
			},
		},
	}
	healthy := &testClient{
		name:     "healthy",
		servedBy: "h1",
		events:   usageEvents(3, 4),
	}

	reg := core.NewRegistry()
	reg.Add(refused)
	reg.Add(healthy)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"refused": {Priority: 1},
		"healthy": {Priority: 2},
	})

	srv := NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(io.Discard, "", 0),
		Stats:    NewStats(),
		Usage:    NewUsageStore(10),
		Live: livecfg.New(livecfg.Snapshot{
			MaxRotate:         1,
			RotateBackoffBase: time.Nanosecond,
		}),
	})

	rec := chat(t, srv, bufferedBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if healthy.seen == nil {
		t.Fatal("the model-refused stream did not fail over (issue #245: same gap, same fix)")
	}
}

// C：反向钉——真正的文本内容拦截（content_blocked，degrade-and-retry 语义）
// 在流内到达时必须原样回放给调用方，不得被 #245 的改道吃掉。
func TestCInStreamTextContentBlockStillReachesTheCaller(t *testing.T) {
	blocked := &testClient{
		name: "blocked",
		events: []core.Event{
			{Type: core.EventError, Err: core.Fail("blocked", "a1", core.FailureContentBlocked, http.StatusBadRequest,
				errors.New("this text violates the content policy")),
			},
		},
	}
	other := &testClient{name: "other"}

	reg := core.NewRegistry()
	reg.Add(blocked)
	reg.Add(other)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"blocked": {Priority: 1},
		"other":   {Priority: 2},
	})

	srv := NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(io.Discard, "", 0),
		Stats:    NewStats(),
		Usage:    NewUsageStore(10),
		Live: livecfg.New(livecfg.Snapshot{
			MaxRotate:         1,
			RotateBackoffBase: time.Nanosecond,
		}),
	})

	rec := chat(t, srv, bufferedBody)
	if other.seen != nil {
		t.Fatal("a text content block was replayed to another platform: the degrade-and-retry semantics must not be eaten (issue #245 反向钉)")
	}
	if rec.Code == http.StatusOK && len(rec.Body.String()) == 0 {
		t.Fatal("the content block never reached the caller")
	}
}
