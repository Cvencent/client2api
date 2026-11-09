package openaicompat

import (
	"context"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// #248：全停用的中转站平台级 detail 不得是空字符串——8 个 not-ready 平台里
// 唯一查无原因的那个。原因写在账号行 note 上，平台级 detail 必须汇总出来
// （与 lobsterai 的 pool.summary 口径对齐）。
// ---------------------------------------------------------------------------

func TestPlatformDetailNamesTheReasonWhenNotReady(t *testing.T) {
	s := &source{
		name:    "tokenrhythm",
		now:     func() time.Time { return time.Now() },
		pool:    newProviderPool(),
		cooling: map[string]time.Time{},
	}
	// 两个账号全部被操作员停用（#244 封号案形状：面板 enabled 开关）。
	s.pool.reload([]providerRecord{{ID: "key-a", Disabled: true}, {ID: "key-b", Disabled: true}})

	st := s.Status(context.Background())
	if st.Ready {
		t.Fatal("an all-disabled source reports ready=true (issue #248)")
	}
	if strings.TrimSpace(st.Detail) == "" {
		t.Fatalf("all-disabled source detail is empty: the not-ready reason must be named at platform level (issue #248)")
	}
	if !strings.Contains(st.Detail, "停用") {
		t.Errorf("detail does not name the operator-disable reason: %q (issue #248)", st.Detail)
	}
	// 部分停用（还有一个 ready）时 ready=true，detail 不走兜底分支——
	// 这是既有行为，钉住防止兜底吞掉 ready 平台的正常 detail。
	s.pool.reload([]providerRecord{{ID: "key-a", Disabled: true}, {ID: "key-b", Disabled: true}, {ID: "key-c", Disabled: false}})
	st2 := s.Status(context.Background())
	if !st2.Ready {
		t.Fatal("a source with one enabled account reports not-ready")
	}
}
