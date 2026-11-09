package loomy

import (
	"context"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// #253：keepalive 心跳对 expired（14 天本地钟到期）号跳过——闸词表补
// acc.expired(now)。expired 号无论心跳成功与否都改变不了池子认知：成功不清
// 钟（只有 funded balance sweep 能复活，issue #169），失败出 refused 行
// （16 次/4 天加速）。消息按原因分岔，操作员一眼分清 14 天钟与人工停车。
// ---------------------------------------------------------------------------

func TestKeepaliveSkipsExpiredAccounts(t *testing.T) {
	c := &Client{store: &store{}}
	c.store.all = append(c.store.all, &account{
		ID:          "loomy-expired",
		AccessToken: "tok",
		ExpiresAtMS: time.Now().Add(-time.Hour).UnixMilli(), // 已到期
		Enabled:     true,
	})
	res, err := c.runScheduledKeepalive(context.Background(), "loomy-expired")
	if err != nil {
		t.Fatalf("runScheduledKeepalive: %v", err)
	}
	if !res.Skipped {
		t.Fatalf("an expired account was not skipped: %+v (issue #253)", res)
	}
	if !strings.Contains(res.Message, "14-day local clock") {
		t.Errorf("skip message does not name the clock: %q (issue #253)", res.Message)
	}
	if strings.Contains(res.Message, "parked or disabled") {
		t.Errorf("expired skip reuses the parked wording: %q (operator cannot tell them apart)", res.Message)
	}
}

func TestKeepaliveStillSkipsParkedAccounts(t *testing.T) {
	c := &Client{store: &store{}}
	c.store.all = append(c.store.all, &account{
		ID:          "loomy-parked",
		AccessToken: "tok",
		Enabled:     false, // 操作员停车
	})
	res, err := c.runScheduledKeepalive(context.Background(), "loomy-parked")
	if err != nil {
		t.Fatalf("runScheduledKeepalive: %v", err)
	}
	if !res.Skipped {
		t.Fatalf("a parked account was not skipped: %+v", res)
	}
	if !strings.Contains(res.Message, "parked or disabled") {
		t.Errorf("parked skip lost its wording: %q (issue #253 反向钉)", res.Message)
	}
	if strings.Contains(res.Message, "14-day") {
		t.Errorf("parked skip borrows the clock wording: %q", res.Message)
	}
}
