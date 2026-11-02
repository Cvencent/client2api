package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 用量库的写盘量（issue #112）。
//
// 30 秒一拍把整份文档重写一遍本身是设计（bucket 存在内存里、整份原子替换保证
// 读者看不到半截），但两件事让代价与变化量脱钩得更厉害：缩进纯放大 61.5%，
// 而 map 迭代序让「数据没变」的两次 flush 也产出一份逐字节不同的文档。
// 这里钉住这两条，并在最后用真实 flush 量一遍两个性质。

// TestUsageFileIsCompactAndStableOrdered 钉住 #112 的两处：紧凑序列化 + 稳定顺序。
func TestUsageFileIsCompactAndStableOrdered(t *testing.T) {
	raw, err := os.ReadFile("usage_store.go")
	if err != nil {
		t.Fatalf("读不到 usage_store.go：%v", err)
	}
	src := string(raw)

	// ① 不许再用 MarshalIndent 写这份文档：它是机器读的，缩进占 61.5% 的字节。
	// 只查 usageFile / recentFile 那两个调用点，别误伤别处真要给人看的文件。
	if strings.Contains(src, `json.MarshalIndent(f, "", "  ")`) {
		t.Fatal("usage.json 又用 MarshalIndent 序列化：缩进多占 61.5% 的写盘量（issue #112）")
	}
	if strings.Contains(src, `json.MarshalIndent(rf, "", "  ")`) {
		t.Error("recent.json 仍是缩进序列化：与 usage.json 同一路径上的纯放大（issue #112）")
	}
	if !strings.Contains(src, "json.Marshal(f)") {
		t.Error("usage.json 没有用紧凑序列化（issue #112）")
	}

	// ② buckets 从 map 迭代出来，序列化前必须排序，否则内容不等的两次 flush
	// 产出的字节也全不一样（实测两份相隔 65 秒的文件只有 13.6% 字节相同）。
	if !strings.Contains(src, "sort.Slice(f.Buckets") {
		t.Fatal("buckets 没有按身份键排序：数据没变也会写出一份逐字节不同的文档（issue #112）")
	}
	if !strings.Contains(src, "f.Buckets[i].key() < f.Buckets[j].key()") {
		t.Error("排序用的不是身份键（scope|client|realm|account|model）：顺序仍不稳定（issue #112）")
	}
	// 排序必须发生在 Marshal 之前，否则只是白排一遍。
	sortAt := strings.Index(src, "sort.Slice(f.Buckets")
	marshalAt := strings.Index(src, "json.Marshal(f)")
	if sortAt < 0 || marshalAt < 0 || sortAt > marshalAt {
		t.Error("排序写在序列化之后：那份文档的顺序照样是随机的（issue #112）")
	}
}

// TestUsageFlushIsCompactAndRepeatable 是上面那条的运行时可判形态：真跑两次
// flush，量字节数与「同一份数据两次落盘是否逐字节相同」。
func TestUsageFlushIsCompactAndRepeatable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	s := NewUsageStore(0)
	if err := s.SetPath(path); err != nil {
		t.Fatalf("SetPath：%v", err)
	}
	// 不 Start()：本测试只关心落盘字节，后台 ticker 会引入并发与时钟依赖。
	defer s.Stop()

	// 造一批跨客户端 / 模型 / 小时的 bucket，让 map 迭代序有足够的重排空间。
	base := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	for h := 0; h < 6; h++ {
		for c := 0; c < 4; c++ {
			for m := 0; m < 3; m++ {
				s.Record(UsageRecord{
					At:     base.Add(time.Duration(h) * time.Hour),
					Client: string(rune('a' + c)),
					Model:  "m" + string(rune('0'+m)),
				})
			}
		}
	}

	if err := s.Save(); err != nil {
		t.Fatalf("第一次 Save：%v", err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读第一份：%v", err)
	}
	// 数据没变，再 flush 一次（force，因为 Save 之后 dirty 已清）。
	if err := s.Save(); err != nil {
		t.Fatalf("第二次 Save：%v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读第二份：%v", err)
	}

	// 稳定顺序：除了 saved 时间戳，两份内容必须逐字节相同。
	strip := func(b []byte) string {
		var f usageFile
		if err := json.Unmarshal(b, &f); err != nil {
			t.Fatalf("解析落盘文档：%v", err)
		}
		f.Saved = time.Time{}
		out, err := json.Marshal(f)
		if err != nil {
			t.Fatalf("重新序列化：%v", err)
		}
		return string(out)
	}
	if a, b := strip(first), strip(second); a != b {
		t.Fatal("数据没变的两次 flush 产出的文档不同：顺序不稳定，diff / 去重全都吃不到（issue #112）")
	}

	// 紧凑：不许有缩进换行（`\n  ` 是 MarshalIndent 的痕迹）。
	if strings.Contains(string(first), "\n  ") {
		t.Error("落盘文档里仍有缩进：应该用紧凑序列化（issue #112）")
	}
	if len(first) == 0 || first[len(first)-1] != '\n' {
		t.Error("落盘文档末尾没有换行")
	}

	// 量一下收益：同一份文档缩进版比紧凑版大多少。
	var roundtrip usageFile
	if err := json.Unmarshal(first, &roundtrip); err != nil {
		t.Fatalf("解析：%v", err)
	}
	compact, _ := json.Marshal(roundtrip)
	indented, _ := json.MarshalIndent(roundtrip, "", "  ")
	if len(indented) <= len(compact) {
		t.Fatal("缩进版不比紧凑版大：样本太小，这条断言失去意义")
	}
	t.Logf("这份样本：紧凑 %d B / 缩进 %d B（缩进多占 %.1f%%）",
		len(compact), len(indented), float64(len(indented)-len(compact))/float64(len(compact))*100)
}
