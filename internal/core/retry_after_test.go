package core

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

// 钉住共享 Retry-After 解析（issue #210）：epoch 时刻、已过期的窗口、相对
// 秒数、HTTP 日期、毫秒五种形态各自正确；cap 由调用方保留。
func TestRetryAfterReset(t *testing.T) {
	now := time.Now()
	cap2h := 2 * time.Hour
	cases := []struct {
		name   string
		header map[string][]string
		want   time.Duration // window: [want-tol, want]
		tol    time.Duration
	}{
		{"epoch future", map[string][]string{"X-Ratelimit-Reset": {strconv.FormatInt(now.Add(90*time.Second).Unix(), 10)}}, 90 * time.Second, 5 * time.Second},
		{"epoch past means no wait", map[string][]string{"X-Ratelimit-Reset": {strconv.FormatInt(now.Add(-30*time.Second).Unix(), 10)}}, 0, 0},
		{"relative seconds", map[string][]string{"X-Ratelimit-Reset": {"90"}}, 90 * time.Second, time.Second},
		{"retry-after", map[string][]string{"Retry-After": {"90"}}, 90 * time.Second, time.Second},
		{"milliseconds", map[string][]string{"Retry-After-Ms": {"90000"}}, 90 * time.Second, time.Second},
		{"http date", map[string][]string{"Retry-After": {now.UTC().Add(90 * time.Second).Format(http.TimeFormat)}}, 90 * time.Second, 5 * time.Second},
		{"capped", map[string][]string{"Retry-After": {"7200"}}, cap2h, time.Second},
	}
	for _, tc := range cases {
		got := RetryAfterReset(http.Header(tc.header), cap2h)
		lo := tc.want - tc.tol
		if got < lo || got > tc.want {
			t.Errorf("%s = %v, want in [%v, %v]", tc.name, got, lo, tc.want)
		}
	}
	// 空头：0。
	if got := RetryAfterReset(nil, cap2h); got != 0 {
		t.Errorf("nil header = %v, want 0", got)
	}
	if got := RetryAfterReset(http.Header{}, cap2h); got != 0 {
		t.Errorf("empty header = %v, want 0", got)
	}
}
