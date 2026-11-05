package core

import (
	"context"
	"testing"
	"time"
)

// hangingClient 的 Models() 会阻塞到 ctx 到期——模拟「厂商 /models 挂住」。
type hangingClient struct {
	fakeClient
}

func (h *hangingClient) Models(ctx context.Context) ([]Model, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// 钉住裸名解析的并发预算（issue #214）：一个模块的目录卡住不再拖住整条
// 裸名路由——解析必须毫秒级返回，卡住的模块按「目录不可用」走 fallback。
func TestBareNameResolveSurvivesAHangingCatalogue(t *testing.T) {
	hanging := &hangingClient{fakeClient: fakeClient{name: "hung"}}
	fast := &fakeClient{name: "fast", models: []Model{{ID: "m1"}}}
	reg := NewRegistry()
	reg.Add(hanging)
	reg.Add(fast)

	start := time.Now()
	_, err := reg.ResolveCandidates(context.Background(), "m1")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("resolve = %v, want the fast module's candidates", err)
	}
	// 预算本身是 5s（与面板的 modelsClientTimeout 同量级）；断言上限=预算+余量，
	// 而非毫秒级——issue 的目标是从「所有模块预算之和」降到「一个短预算」。
	if elapsed > catalogueBudget+time.Second {
		t.Fatalf("bare-name resolve took %s: 超出单模块预算，卡住的目录还在拖（issue #214）", elapsed)
	}
}

// 不存在的名字也必须毫秒级 404（曾陪跑满卡住模块的预算）。
func TestUnknownBareNameSurvivesAHangingCatalogue(t *testing.T) {
	hanging := &hangingClient{fakeClient: fakeClient{name: "hung"}}
	reg := NewRegistry()
	reg.Add(hanging)

	start := time.Now()
	_, err := reg.ResolveCandidates(context.Background(), "zzz-typo")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("unknown name resolved, want a 404-style error")
	}
	if elapsed > catalogueBudget+time.Second {
		t.Fatalf("unknown-name resolve took %s: 超出单模块预算（issue #214）", elapsed)
	}
}
