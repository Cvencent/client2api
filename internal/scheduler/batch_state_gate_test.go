package scheduler

import (
	"context"
	"testing"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// #252：checkin/growth 的账号名单补 invalid 闸——vendor 拒收的终态会话每轮
// 烧真实上游往返且答案永远相同（loomy 100002 夜间 23 次+17 次）。#168 边界：
// abnormal/low_credit 是自愈数据源，必须留在名单里。
// ---------------------------------------------------------------------------

// TestInvalidStateIsGatedFromBatches 钉住 accountRecordsOfForBatch 的 State 闸：
// invalid 不进名单（checkin 与 growth 两种模式都是）；abnormal 与 low_credit
// 留在名单里（自愈数据源）；Enabled=false 的行为不变。
func TestInvalidStateIsGatedFromBatches(t *testing.T) {
	c := &fakeClient{
		name: "gate",
		accounts: []core.AccountRecord{
			{ID: "invalid-acct", Enabled: true, State: "invalid"},
			{ID: "abnormal-acct", Enabled: true, State: "unknown"},
			{ID: "lowcredit-acct", Enabled: true, State: "unknown"},
			{ID: "healthy-acct", Enabled: true, State: "ready"},
			{ID: "disabled-acct", Enabled: false, State: "ready"},
		},
	}
	// 用 State 字段区分：abnormal/low_credit 在 AccountRecord 上没有专字段，
	// fixture 用 State 的真实取值模拟（loomy 把 invalid 之外的都报 ready/unknown
	// 是因为 AccountRecord.State 只有五个取值；loomy-26 的 invalid 号 State=invalid）。
	_ = c

	for _, checkin := range []bool{false, true} {
		recs, err := accountRecordsOfForBatch(context.Background(), c, checkin)
		if err != nil {
			t.Fatalf("checkin=%v: %v", checkin, err)
		}
		ids := map[string]bool{}
		for _, rec := range recs {
			ids[rec.ID] = true
		}
		if ids["invalid-acct"] {
			t.Errorf("checkin=%v: the vendor-rejected (invalid) account is in the batch list; every run burns a round trip that always answers 100002 (issue #252)", checkin)
		}
		if ids["disabled-acct"] && !checkin {
			// Enabled=false 且非 checkin 模式：既有行为是排除，保持。
			t.Errorf("checkin=%v: a disabled account entered the list (pre-existing behavior changed)", checkin)
		}
	}
}

// TestAbnormalAndLowCreditStayInBatches 是 #168 边界的反向钉：可自愈状态必须
// 留在名单里——闸拦多了自愈就没有数据源了。AccountRecord.State 的取值域里
// abnormal/low_credit 落在 unknown（模块自报的精细状态不在 gateway 记录上），
// 所以闸按字面 "invalid" 拦，天然不误伤。
func TestAbnormalAndLowCreditStayInBatches(t *testing.T) {
	c := &fakeClient{
		name: "selfheal",
		accounts: []core.AccountRecord{
			{ID: "selfheal-a", Enabled: true, State: "unknown"},
			{ID: "selfheal-b", Enabled: true, State: "cooling"},
			{ID: "selfheal-c", Enabled: true, State: "exhausted"},
			{ID: "selfheal-d", Enabled: true, State: "low_credit_fallback"},
		},
	}
	recs, err := accountRecordsOfForBatch(context.Background(), c, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(recs) != 4 {
		t.Fatalf("kept %d of 4 records, want 4: the gate must only drop literal \"invalid\" (issue #252 反向钉)", len(recs))
	}
}
