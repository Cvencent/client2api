package panel

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"client2api/internal/gateway"
	"client2api/internal/scheduler"
)

// 两个只读端点的 p99 观测（issue #96）。
//
// 现象：偶发 ~5s 尖刺、瞬时恢复，怀疑与 5 分钟余额扫描的锁面相关——归因需要
// 一周量级的样本。观测本身就是本单的产出：>1s 的响应写进运行日志一行，带耗时
// 与落入 5 分钟 tick 窗口的位置；跑一周即可看尖刺与 sweep tick 的相关性，
// 再决定是否缩小锁面或改原子快照。
//
// 覆盖面在 v0.5.89 扩到 /panel/api/pool：面板自己的 5 秒轮询打的是那个轻端点
// （issue #33 之后），而此前只有 /status 有探针——操作员真正感受到的那条路上
// 一个样本都没记。

func TestStatusSlowResponseIsLoggedWithTheTickOffset(t *testing.T) {
	// The probe keys on elapsed time, which a fast handler never crosses --
	// the wiring, not a slowdown, is what the week of samples depends on.
	b, err := os.ReadFile("panel.go")
	if err != nil {
		t.Skipf("cannot read panel.go: %v", err)
	}
	src := string(b)
	for _, want := range []string{
		`slowReadProbe("/panel/api/status")`,
		`slowReadProbe("/panel/api/pool")`,
		"tickOffset",
		"d <= time.Second",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("慢响应探针缺少 %q（issue #96 的观测线）", want)
		}
	}
	// 两个端点必须共用同一个实现：各写一份的话，格式迟早分叉，而这条观测线的
	// 全部价值就在于「一周的样本能放在一起筛」。
	if n := strings.Count(src, "func (p *panel) slowReadProbe("); n != 1 {
		t.Fatalf("slowReadProbe 有 %d 个定义，期望 1 个", n)
	}

	// 行为半区：哪怕手头 handler 很快，日志环写入的代码路径是真实的——
	// 手工塞一行验证环与格式，一周后在运行日志里能按这一行筛出全部尖刺。
	ring := gateway.NewLogRing(200)
	// New returns the http.Handler; the concrete panel keeps opts, which is
	// where the log ring lives (handleStatus reads p.opts.Logs on the slow path).
	pp := &panel{opts: Options{Version: "test", Logs: ring}}
	pp.wireStoreLogs()
	pp.opts.Logs.Add("panel: slow /panel/api/status: 5s0ms (next balance tick in 2m30s)")
	h := New(Options{Version: "test", Logs: ring})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/logs?lines=500", nil))
	if !strings.Contains(rec.Body.String(), "slow /panel/api/status") {
		t.Fatal("慢响应行没有落进运行日志：观测线在真实面板上不可见（issue #96）")
	}
	if !strings.Contains(rec.Body.String(), "next balance tick in") {
		t.Fatal("慢响应行缺 tick 偏移：无法与 sweep tick 做相关性（issue #96）")
	}
}

// 探针必须同时写**可轮转回看的文件日志**，而不只是内存环（issue #96 的证据面修正）。
//
// 环容量 2000 行，且被每请求的 chat 行占满——真实部署上几分钟就滚一遍，
// 「攒一周样本」在环里根本留不住。这条线于 #96 的第一次归因里就是按文件 grep
// 得到的零命中，而当时的探针只写环、从不落盘，所以那个「零命中」按构造必然，
// 不能用来论证「这段时间没有 >1s 的读」。判据不是「有没有环」，而是
// 「这条诊断有没有一条活过一天的落盘通路」。
func TestTheSlowReadLineReachesTheDurableLogNotOnlyTheRing(t *testing.T) {
	ring := gateway.NewLogRing(200)
	var durable strings.Builder
	// A wired scheduler, like a real process: the line must carry the
	// scheduler-anchored offset, not the wall-clock fallback.
	sched := &fakeScheduler{st: scheduler.Status{
		Enabled: true,
		Next:    map[string]time.Time{scheduler.BalanceTickName: time.Now().Add(2 * time.Minute)},
	}}
	pp := &panel{opts: Options{
		Version:   "test",
		Logs:      ring,
		Scheduler: sched,
		OpsLog:    func(line string) { durable.WriteString(line + "\n") },
	}}

	// A slow /status must land in BOTH surfaces with the same text.
	func() {
		defer pp.slowReadProbe("/panel/api/status")()
		time.Sleep(1100 * time.Millisecond)
	}()
	if !strings.Contains(durable.String(), "slow /panel/api/status") {
		t.Fatal("慢响应没有落到文件日志：环之外没有活过一天的观测面，一周样本无从攒起（issue #96）")
	}
	if !strings.Contains(durable.String(), "next balance tick in") {
		t.Fatal("文件日志那一行缺 tick 偏移：与 sweep tick 的相关性必须在文件里也能算（issue #96）")
	}
	entries := ring.Entries(0)
	if len(entries) == 0 || !strings.Contains(entries[len(entries)-1].Text, "slow /panel/api/status") {
		t.Fatal("慢响应没有进环：面板的实时日志页看不到这条观测（issue #96）")
	}
	// 两个面必须携带同一段文本（环会重新盖时间戳，比对戳之后的部分），
	// 否则按时间对齐两边样本时会分叉。
	payload := strings.TrimPrefix(strings.TrimSpace(durable.String()), "panel: ")
	if !strings.Contains(entries[len(entries)-1].Text, payload) {
		t.Fatalf("两个面的文本不一致：文件 %q / 环 %q", payload, entries[len(entries)-1].Text)
	}

	// 快响应不写任何一面：这条线是异常观测，不是访问日志。
	durable.Reset()
	func() {
		defer pp.slowReadProbe("/panel/api/pool")()
	}()
	if durable.Len() != 0 {
		t.Fatalf("快响应也落了文件日志：观测线会退化成访问日志（%q）", durable.String())
	}
}

// 落盘的诊断行必须已经脱敏：环自己会 Redact，文件写入器**不会**——同一段文本
// 走两条通路时，脱敏只能做一次，而必须在写文件之前。
func TestPanelDiagnosticsAreRedactedBeforeTheDurableLog(t *testing.T) {
	var durable []string
	p := &panel{opts: Options{Logs: gateway.NewLogRing(10), OpsLog: func(l string) { durable = append(durable, l) }}}
	p.opsf("panel: store write failed: %s", `open C:\data\accounts.json: api_key="sk-live-9f3c2a1b8d7e"`)

	if len(durable) != 1 {
		t.Fatalf("写文件通路的调用次数 = %d，期望 1", len(durable))
	}
	if strings.Contains(durable[0], "sk-live-9f3c2a1b8d7e") {
		t.Fatalf("文件日志里出现了未脱敏凭据：%q（环会 Redact，文件不会）", durable[0])
	}
	if !strings.Contains(durable[0], "<redacted>") {
		t.Fatalf("凭据没有被替换成红标：%q", durable[0])
	}
}

// 探针的 tick 偏移必须以**调度器真实的余额 tick** 为原点，不能拿挂钟相位顶替
// （issue #96 的仪器缺陷）。
//
// 旧实现取 `started.UnixNano() % 5min`，也就是 :00/:05/:10 的挂钟相位；而调度器
// 把 tick 锚在**进程启动时刻**（`tickBalance` 里 `r.balNext = r.now().Add(every)`），
// 于是两边相位差可以任意（本机实测 85s），且每次重启还会平移。用错原点的偏移
// 无法与真实 sweep 对齐——正是这条观测线唯一的用途。
func TestTheTickOffsetIsAnchoredToTheSchedulerNotTheWallClock(t *testing.T) {
	src, err := os.ReadFile("panel.go")
	if err != nil {
		t.Skipf("cannot read panel.go: %v", err)
	}
	s := string(src)

	// 必须读调度器发布的下一枚 tick，而不是自己算挂钟相位。
	if !strings.Contains(s, "scheduler.BalanceTickName") {
		t.Fatal("slowReadProbe 没有读 scheduler.BalanceTickName：偏移会退回挂钟相位，与真实 sweep 相位无关（issue #96）")
	}
	// 反证：旧写法必须消失。挂钟取模只允许留在**明确的降级文案**里（并自称
	// wall-clock），不能作为主路径。
	if n := strings.Count(s, "UnixNano()%"); n != 1 {
		t.Errorf("挂钟取模出现 %d 次，期望 1 次（只允许 in tickOffset 的降级分支）", n)
	}

	// 行为：装一个把下一枚 tick 放在 2 分钟后的调度器，探针必须说「next balance
	// tick in 2m0s」——与当时挂在哪个挂钟相位无关。
	sched := &fakeScheduler{st: scheduler.Status{
		Enabled: true,
		Next:    map[string]time.Time{scheduler.BalanceTickName: time.Now().Add(2 * time.Minute)},
	}}
	pp := &panel{opts: Options{Version: "test", Scheduler: sched}}
	got := pp.tickOffset(time.Now())
	if !strings.Contains(got, "next balance tick in") {
		t.Fatalf("tickOffset = %q，期望「next balance tick in ...」（issue #96）", got)
	}
	if !strings.Contains(got, "1m") && !strings.Contains(got, "2m") {
		t.Errorf("tickOffset = %q，期望约 2 分钟（锚在调度器而不是挂钟）", got)
	}

	// 没有任何调度器时（测试/嵌入方）必须**自称**是挂钟相位，绝不冒充调度器锚点。
	bare := &panel{opts: Options{Version: "test"}}
	if got := bare.tickOffset(time.Now()); !strings.Contains(got, "wall-clock") {
		t.Errorf("无调度器时 tickOffset = %q，应显式标注 wall-clock，避免与调度器锚点样本混为一谈（issue #96）", got)
	}
}

// /status 的慢读必须能**指名到模块与阶段**（issue #96）。
//
// dual-poll 对照把尖刺钉在 /status 自己身上（同刻 /pool 只要 5ms，而 /pool 也
// 每模块调同一个 Status()），于是嫌疑落到只有 /status 会做的 per-client 工作：
// capabilities 枚举 + model-park 合并。逐个读这些方法的函数体看不出开销，只能
// 让它在发生的那一刻自报家门——否则下一轮还是靠推理（dual-poll 这种对照很贵）。
func TestASlowStatusNamesTheModuleAndPhase(t *testing.T) {
	ring := gateway.NewLogRing(200)
	var durable strings.Builder
	pp := &panel{opts: Options{
		Version: "test", Logs: ring,
		OpsLog: func(l string) { durable.WriteString(l + "\n") },
	}}

	// Above the threshold: the slowest phase is named, with both numbers.
	pp.logSlowStatus(1500*time.Millisecond, []statusPhase{
		{name: "loomy status", took: 120 * time.Millisecond},
		{name: "workbuddy capabilities", took: 1300 * time.Millisecond},
	})
	got := durable.String()
	for _, want := range []string{"breakdown", "workbuddy capabilities", "1.3s", "1.5s"} {
		if !strings.Contains(got, want) {
			t.Errorf("慢 /status 诊断缺少 %q：定位不到是哪个模块/阶段（issue #96）；got=%q", want, got)
		}
	}
	// 只认最慢的那个阶段：把最慢的写成别的模块等于指错方向。
	if strings.Contains(got, "loomy status") {
		t.Error("breakdown 报了非最慢阶段：模块名会被误导（issue #96）")
	}

	// 快请求不出这条线（否则退化成访问日志）。
	durable.Reset()
	pp.logSlowStatus(200*time.Millisecond, []statusPhase{{name: "x status", took: 150 * time.Millisecond}})
	if durable.Len() != 0 {
		t.Fatalf("快 /status 也打了 breakdown：观测线退化成访问日志（%q）", durable.String())
	}
}

// 计数门限：只有超过 100ms 的 per-client 阶段才进 breakdown，否则每个正常
// 模块（毫秒级）都会灌进来，一行都读不了。
func TestOnlySlowPhasesEnterTheStatusBreakdown(t *testing.T) {
	src, err := os.ReadFile("panel.go")
	if err != nil {
		t.Skipf("cannot read panel.go: %v", err)
	}
	s := string(src)
	if !strings.Contains(s, "100*time.Millisecond") {
		t.Error("per-client 阶段没有 100ms 门限：breakdown 会被毫秒级模块灌满（issue #96）")
	}
	// 两个阶段都要单独计时：合成一个总时长就分不清是模块自己慢还是枚举慢。
	for _, want := range []string{"capsTook", "stTook"} {
		if !strings.Contains(s, want) {
			t.Errorf("per-client 阶段没有分开计时 %q：分不清是 Status() 慢还是 capabilities() 慢（issue #96）", want)
		}
	}
}
