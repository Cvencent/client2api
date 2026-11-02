package gateway

import (
	"fmt"
	"strings"
	"testing"
)

// 日志环的增量查询（issue #113）。
//
// 面板的 4 秒轮询原来每拍重下 500 行（约 95 KB），而真实实例上日志平均 2.2 秒
// 新增一行——「total 未变就早退」几乎不成立。增量游标让那一拍只取新增的那一两行。
// 这里钉住游标的三种情形：能接上、已经接不上（滑出保留窗口）、以及「没有新行」。

func TestLogRingEntriesSinceReturnsOnlyTheTail(t *testing.T) {
	r := NewLogRing(5)
	for i := 1; i <= 5; i++ {
		r.Add(fmt.Sprintf("line %d", i))
	}
	// total=5，保留 5 行，最老的一行序号是 1。
	if got := r.Total(); got != 5 {
		t.Fatalf("total = %d, want 5", got)
	}

	got, ok := r.EntriesSince(3)
	if !ok {
		t.Fatal("EntriesSince(3) 说不完整，但第 4、5 行都还在保留窗口里")
	}
	if len(got) != 2 {
		t.Fatalf("EntriesSince(3) 返回 %d 行，期望 2 行（第 4、5 行）", len(got))
	}
	// 每条都带行首时间戳（Ring 自己盖的），所以只比尾部。
	if !strings.HasSuffix(got[0].Text, "line 4") || !strings.HasSuffix(got[1].Text, "line 5") {
		t.Errorf("增量内容不对：%q / %q", got[0].Text, got[1].Text)
	}

	// 没有新行时返回空而不是报错：调用方据此保持 DOM 不动。
	if res, ok := r.EntriesSince(5); !ok || len(res) != 0 {
		t.Errorf("EntriesSince(total) = %d 行 / ok=%v，期望 0 行 / true", len(res), ok)
	}
}

// TestLogRingEntriesSinceRefusesWhenTheGapIsGone 钉住「游标滑出保留窗口」这条：
// 中间的行走掉之后不能再假装能接上，否则面板会静默跳过一段日志。
func TestLogRingEntriesSinceRefusesWhenTheGapIsGone(t *testing.T) {
	r := NewLogRing(3)
	for i := 1; i <= 6; i++ {
		r.Add(fmt.Sprintf("line %d", i))
	}
	// total=6，只留第 4、5、6 行；游标 2 意味着第 3 行已经没了。
	if _, ok := r.EntriesSince(2); ok {
		t.Fatal("EntriesSince(2) 声称完整：第 3 行已被覆盖，调用方会静默跳掉它（issue #113）")
	}
	// 游标正好停在保留窗口之前一格时仍然可以接上（差的只是被丢掉的那行本身）。
	if got, ok := r.EntriesSince(3); !ok || len(got) != 3 {
		t.Errorf("EntriesSince(3) = %d 行 / ok=%v，期望 3 行 / true", len(got), ok)
	}
}

// TestLogRingEntriesSinceWalksForward 逐拍推进游标：每拍只拿新增的行，累计起来
// 与整份窗口的最后 N 行一致。这是面板「追加而不是替换」那条路径的正确性。
func TestLogRingEntriesSinceWalksForward(t *testing.T) {
	r := NewLogRing(10)
	cursor := int64(0)
	var seen []string
	for i := 1; i <= 10; i++ {
		r.Add(fmt.Sprintf("line %d", i))
		if i%3 != 0 {
			continue // 模拟「几行一批、不是每行都轮询」
		}
		batch, ok := r.EntriesSince(cursor)
		if !ok {
			t.Fatalf("第 %d 行后游标 %d 接不上了", i, cursor)
		}
		for _, e := range batch {
			seen = append(seen, e.Text)
		}
		cursor = r.Total()
	}
	// 收尾：循环里最后一次查询之后又加了一行（i=10 不是 3 的倍数），
	// 这里补拉一次，模拟「轮询节拍与写入节拍不同相」的常态。
	if tail, ok := r.EntriesSince(cursor); ok {
		for _, e := range tail {
			seen = append(seen, e.Text)
		}
	}
	full := r.Entries(10)
	if len(seen) != len(full) {
		t.Fatalf("增量累计 %d 行，整份窗口 %d 行", len(seen), len(full))
	}
	for i := range full {
		if seen[i] != full[i].Text {
			t.Fatalf("第 %d 行不一致：增量 %q / 整份 %q", i, seen[i], full[i].Text)
		}
	}
}
