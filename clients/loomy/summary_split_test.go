package loomy

import (
	"context"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// #251：全池不可选的 detail 话术按状态分布分档——low_credit 是等回血的经济态
// （夜间赠送成批复活是常态），abnormal 才需要修因/重登；"every account is
// abnormal" 把 48 个等回血的盖成要修因。反向钉：存在 low_credit 时不得出现
// "every account" 措辞。
// ---------------------------------------------------------------------------

func TestSummarySplitsLowCreditFromAbnormal(t *testing.T) {
	store := &store{}
	// 4 low_credit + 2 abnormal（issue 实测分布 48/7 的缩放 fixtures）。
	for i := 0; i < 4; i++ {
		store.all = append(store.all, &account{ID: defaultAccountID(account{AccessToken: "lc" + string(rune('a'+i))}), AccessToken: "lc", lowCredit: true})
	}
	for i := 0; i < 2; i++ {
		store.all = append(store.all, &account{ID: defaultAccountID(account{AccessToken: "ab" + string(rune('a'+i))}), AccessToken: "ab", abnormal: true})
	}
	c := &Client{store: store}
	st := c.Status(context.Background())
	if st.Ready {
		t.Fatal("an all-unusable pool reports ready=true")
	}
	if strings.Contains(st.Detail, "every account") {
		t.Errorf("low_credit present but detail says 'every account': %q (issue #251)", st.Detail)
	}
	if !strings.Contains(st.Detail, "low_credit") || !strings.Contains(st.Detail, "awaiting credit refill") {
		t.Errorf("detail does not name the low_credit bucket: %q (issue #251)", st.Detail)
	}
	if !strings.Contains(st.Detail, "abnormal") {
		t.Errorf("detail does not name the abnormal bucket: %q (issue #251)", st.Detail)
	}
}

func TestSummaryKeepsEveryAccountWordingWhenAllAbnormal(t *testing.T) {
	store := &store{}
	// 全部 abnormal、无 low_credit：原话术保留（issue 反向钉）。
	for i := 0; i < 3; i++ {
		store.all = append(store.all, &account{ID: defaultAccountID(account{AccessToken: "ab" + string(rune('a'+i))}), AccessToken: "ab", abnormal: true})
	}
	c := &Client{store: store}
	st := c.Status(context.Background())
	if !strings.Contains(st.Detail, "every account is marked abnormal") {
		t.Errorf("all-abnormal pool lost the original wording: %q (issue #251 反向钉)", st.Detail)
	}
	if strings.Contains(st.Detail, "low_credit") {
		t.Errorf("no low_credit accounts but detail names the bucket: %q", st.Detail)
	}
}
