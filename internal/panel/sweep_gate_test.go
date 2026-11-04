package panel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// 批量巡检下发接口的方法闸与队列纪律（issue #173）。
//
// 四个下发 handler（batchStart/recoveryStart/dailyBalanceStart/balanceAll）曾
// 经一个都不看 r.Method：写来源闸只在 isWriteMethod（POST/PATCH/PUT/DELETE）为
// 真时运行，于是 GET/HEAD/TRACE 直接绕过 #103 的整道闸——一张跨站 <img> 就能
// 给全池排进一轮真实上游巡检。30 个 GET 在 88ms 内全部 202，队列无去重、无上限、
// 无取消。修法三件套：非 POST 一律 405；(client, batch) 去重回 409（前端注释
// 承诺过的那道保护）；队列长度上限超限回 429。
// ---------------------------------------------------------------------------

// gatePanel builds a panel whose batch queue's executor blocks forever: the
// first enqueued item parks the worker on it (a sweep "in progress"), so
// everything the test enqueues after that stays queued and observable instead
// of racing a worker that drains the queue.  The park is released on cleanup.
func gatePanel(t *testing.T, clients ...core.Client) *panel {
	t.Helper()
	p := batchPanel(t, clients...)
	block := make(chan struct{})
	p.queue.run = func(ctx context.Context, q queuedSweep) {
		<-block
	}
	t.Cleanup(func() { close(block) })
	return p
}

func TestSweepHandlersRejectNonPost(t *testing.T) {
	c := newSweepClient("zcode", core.TaskResult{OK: true}, liveAccount("a1"))
	p := batchPanel(t, c)
	p.opts.Registry = registryOf(c)

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodTrace, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		p.batchStart(rec, httptest.NewRequest(method, "/panel/api/clients/zcode/batches/checkin/run", nil), c, "checkin")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /batches/checkin/run = %d, want 405（GET 有副作用=最贵的空操作，issue #173）", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
			t.Errorf("%s 的 Allow 头 = %q, want POST", method, allow)
		}
	}

	// balanceAll：一次 GET 曾直接 200 并发起全平台余额刷新。
	rec := httptest.NewRecorder()
	p.balanceAll(rec, httptest.NewRequest(http.MethodGet, "/panel/api/balance_all", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /balance_all = %d, want 405（issue #173）", rec.Code)
	}
}

func TestDuplicateSweepIsA409NotASecondQueuedItem(t *testing.T) {
	c := newSweepClient("zcode", core.TaskResult{OK: true}, liveAccount("a1"))
	p := gatePanel(t, c)
	p.opts.Registry = registryOf(c)

	post := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		p.batchStart(rec, httptest.NewRequest(http.MethodPost, "/panel/api/clients/zcode/batches/checkin/run", nil), c, "checkin")
		return rec
	}

	first := post()
	if first.Code != http.StatusAccepted {
		t.Fatalf("first POST = %d, want 202: %s", first.Code, first.Body.String())
	}
	for i := 2; i <= 5; i++ {
		rec := post()
		if rec.Code != http.StatusConflict {
			t.Fatalf("duplicate POST #%d = %d, want 409（前端注释承诺的「已经有一轮在跑」就是这道，issue #173）", i, rec.Code)
		}
	}
	// 第一个 item 可能已被 worker 取走（current），pending 只数还在排队的。
	// pending 自己拿锁，所以这里只读字段、不持锁调它。
	p.queue.mu.Lock()
	running := p.queue.running
	hasCurrent := p.queue.current != nil
	p.queue.mu.Unlock()
	depth := len(p.queue.pending())
	if running && hasCurrent {
		depth++
	}
	if depth != 1 {
		t.Fatalf("queue depth = %d, want 1：同一个 (client, batch) 不得重复排队（issue #173）", depth)
	}
}

func TestQueueRefusesPastItsCap(t *testing.T) {
	c := newSweepClient("zcode", core.TaskResult{OK: true}, liveAccount("a1"))
	p := gatePanel(t, c)
	p.opts.Registry = registryOf(c)

	// 上限与去重是两道独立的闸：这里直接用 enqueue 塞满（不同 client 名），
	// 再走 handler 验证 429。
	for i := 0; i < batchQueueMax; i++ {
		if err := p.queue.enqueue(queuedSweep{runID: "seed", client: "other-" + string(rune('a'+i)), batch: fastBatch("checkin", "claim")}); err != nil {
			t.Fatalf("seed enqueue %d: %v", i, err)
		}
	}
	rec := httptest.NewRecorder()
	p.batchStart(rec, httptest.NewRequest(http.MethodPost, "/panel/api/clients/zcode/batches/checkin/run", nil), c, "checkin")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-cap POST = %d (%s), want 429（超限要显式拒绝而不是静默堆积，issue #173）", rec.Code, rec.Body.String())
	}
	// cap 检查把 worker 手里那个也算一格；worker 是否已把第一个取走是
	// 调度时序（本地快、CI 慢），所以只断言「pending + 运行中 ≤ cap」且
	// 第 9 个请求已被拒——429 本身就是上限生效的证据。
	p.queue.mu.Lock()
	pending, running, occupied := len(p.queue.items), p.queue.running, p.queue.current != nil
	p.queue.mu.Unlock()
	if occupied := pending + 1; running && occupied > batchQueueMax {
		t.Fatalf("pending=%d + running 1 = %d, past the cap %d", pending, occupied, batchQueueMax)
	}
	_ = occupied
}

func TestEnqueueErrorsTranslateToTheirStatusCodes(t *testing.T) {
	p := batchPanel(t, newSweepClient("zcode", core.TaskResult{OK: true}, liveAccount("a1")))

	rec := httptest.NewRecorder()
	if !p.enqueueOr4xx(rec, queuedSweep{client: "zcode", batch: fastBatch("checkin", "claim")}) {
		t.Fatal("first enqueue should succeed")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("a successful enqueue wrote a response: %d %s", rec.Code, rec.Body.String())
	}
	if p.enqueueOr4xx(rec, queuedSweep{client: "zcode", batch: fastBatch("checkin", "claim")}) {
		t.Fatal("duplicate enqueue should be refused")
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate = %d, want 409", rec.Code)
	}
}

func TestRefusedSweepDoesNotLingerAsQueued(t *testing.T) {
	c := newSweepClient("zcode", core.TaskResult{OK: true}, liveAccount("a1"))
	p := gatePanel(t, c)
	p.opts.Registry = registryOf(c)

	// Fill the queue to the cap so the next start is refused at enqueue time.
	for i := 0; i < batchQueueMax; i++ {
		if err := p.queue.enqueue(queuedSweep{runID: "seed", client: "other-" + string(rune('a'+i)), batch: fastBatch("checkin", "claim")}); err != nil {
			t.Fatalf("seed enqueue %d: %v", i, err)
		}
	}
	rec := httptest.NewRecorder()
	p.batchStart(rec, httptest.NewRequest(http.MethodPost, "/panel/api/clients/zcode/batches/checkin/run", nil), c, "checkin")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-cap POST = %d, want 429", rec.Code)
	}
	for _, run := range p.sweeps.forClient("zcode", 10) {
		if run.State == batchQueued {
			t.Fatalf("refused sweep %s lingers as queued：拒绝项不得留在日志里冒充排队（issue #173）", run.ID)
		}
	}
}
