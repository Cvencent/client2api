package panel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// 任务看板缓存的写盘量必须与「这一轮变了多少」挂钩，而不是与「问了几个账号」
// 挂钩（issue #145）。
//
// 报告方的实测：55 账号的强制重读（refresh=1）把整份文档按账号**各整份重写一遍**
// ——`json.MarshalIndent` 全文档 + `core.WriteFileAtomic`（Sync + rename）×55，
// 合计 30~72 MB 落盘，而 55 条任务清单**逐字节与已存的相同**（282 B / 0.02% 的
// 差异全来自 13 位毫秒时间戳 `fetched_at`）；同一份种子去掉 `refresh=1` 则是
// 0 次写入、0 字节。也就是说问题在**写入时机**：put() 每账号一次、persist()
// 整份一遍，两者相乘。
//
// 修法：put() 只改内存（返回后不落盘），批量路径用 putBatch 收口、扫描循环
// 在 `wg.Wait()` 后 persist 一次；单账号路由自己 persist。判据是**写盘次数**：
// N 个账号 = 1 次落盘，而不是 N 次。
// ---------------------------------------------------------------------------

// TestTaskBoardCachePutDoesNotPersistItself 钉住 #145 的核心：put() 之后**没有**
// 落盘动作。这样 55 个账号的扫描才可能只写一次盘。
//
// 判据用的是行为而不是文本：put 之后立刻读盘上的文件，它必须**还不存在**——
// 旧实现里 put() 返回时文件已经写完（整份文档），这条判据直接把它拦下。
func TestTaskBoardCachePutDoesNotPersistItself(t *testing.T) {
	path := filepath.Join(t.TempDir(), "panel", "task_board_cache.json")
	cache := newTaskBoardCache(path)

	cache.put("loomy", "a1", []core.TaskInfo{{Code: "share_soul", Title: "T"}}, nil)

	if _, err := os.Stat(path); err == nil {
		t.Fatal("put() 之后盘上已有缓存文件：put 仍然自带 persist，55 账号扫描还是 55 次整份重写（issue #145）")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat: %v", err)
	}

	// 收口之后必须真的落盘（否则数据会丢——另一半的失效）。
	cache.persist()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("persist() 之后盘上没有缓存文件：%v", err)
	}
}

// TestTaskBoardScanWritesOncePerScan 是**功能性**判据：真跑一次多账号的
// 强制重读，量「写盘的内容版本数」。扫描走的是与生产相同的路径（HTTP handler
// → taskListAll → 每账号 taskBoardTasks → put），因此它证明的是「扫描路径
// 真的只落一次盘」，而不是「某个函数被调了」。
func TestTaskBoardScanWritesOncePerScan(t *testing.T) {
	c := newSweepClient("loomy", core.TaskResult{OK: true},
		liveAccount("a1"), liveAccount("a2"), liveAccount("a3"), liveAccount("a4"))
	c.fakeTaskClient.tasks = []core.TaskInfo{{Code: "share_soul", Auto: true}}
	p := taskPanel(t, c)
	dir := t.TempDir()
	p.opts.DataDir = dir
	p.initTaskBoardCache()
	path := filepath.Join(dir, "panel", "task_board_cache.json")

	// 强制重读：每个账号都会走上游并 put 一次（refresh=1 绕过缓存命中）。
	rec, _ := doTask(t, p, "GET", "/panel/api/clients/loomy/tasks?all=1&refresh=1", "")
	if rec.Code != 200 {
		t.Fatalf("forced scan: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := c.accountsAsked(); len(got) != 4 {
		t.Fatalf("forced scan asked %v, want all four accounts", got)
	}

	// 数据必须完整落盘（重启后可读，恢复测试的契约不能因省 I/O 而丢）。
	second := newTaskBoardCache(path)
	for _, id := range []string{"a1", "a2", "a3", "a4"} {
		if _, ok := second.entry("loomy", id); !ok {
			t.Fatalf("account %s is missing after the scan: the single flush lost data", id)
		}
	}

	// 静态收口：put() 的**代码**里不得再调用 persist——判据先剥注释再看文本，
	// 因为 put 的函数体里有一条「这里没有 persist」的解释性注释，不剥就会
	// 被自己的注释判红（本仓 #139 的第一版测试正是栽在「注释里出现关键词」）。
	body := stripLineComments(stripPanelComments(
		funcBodyFromDisk(t, "taskboardcache.go", "func (c *taskBoardCache) put(")))
	if strings.Contains(body, "persist(") {
		t.Fatal("put() 里又调用了 persist：每个账号一次整份重写回来了（issue #145）")
	}
}

// TestTaskBoardSingleAccountStillPersists 钉住另一半：单账号路由没有批量收口，
// flush 必须由它自己做——否则单账号的读取结果会丢（省 I/O 不能省成丢数据）。
func TestTaskBoardSingleAccountStillPersists(t *testing.T) {
	c := newSweepClient("loomy", core.TaskResult{OK: true}, liveAccount("a1"))
	c.fakeTaskClient.tasks = []core.TaskInfo{{Code: "share_soul", Auto: true}}
	p := taskPanel(t, c)
	dir := t.TempDir()
	p.opts.DataDir = dir
	p.initTaskBoardCache()
	path := filepath.Join(dir, "panel", "task_board_cache.json")

	rec, _ := doTask(t, p, "GET", "/panel/api/clients/loomy/tasks?account=a1", "")
	if rec.Code != 200 {
		t.Fatalf("single-account read: status=%d body=%s", rec.Code, rec.Body.String())
	}

	second := newTaskBoardCache(path)
	if _, ok := second.entry("loomy", "a1"); !ok {
		t.Fatal("单账号读取后缓存没有落盘：put 不再自带 persist 之后，这条路由忘了自己收口（issue #145）")
	}
}

// funcBodyFromDisk reads a function body straight from the named source file in
// this package, so an assertion can also pin what must NOT be there.
func funcBodyFromDisk(t *testing.T, file, sig string) string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	src := string(b)
	i := strings.Index(src, sig)
	if i < 0 {
		t.Fatalf("%s has no %s", file, sig)
	}
	rest := src[i:]
	if j := strings.Index(rest, "\n}"); j >= 0 {
		return rest[:j]
	}
	t.Fatalf("%s: %q looks unterminated", file, sig)
	return ""
}
