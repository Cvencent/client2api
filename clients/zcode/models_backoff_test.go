package zcode

import (
	"errors"
	"testing"
	"time"
)

// #246：永久 404（plan 无目录端点）连续失败 3 次后，重试间隔从 TTL 拉长到
// 30 分钟——三天 288 次零收益上游请求不再发生。
func TestPermanentRefusalBacksOff(t *testing.T) {
	mc := newModelCache()
	now := time.Now()
	for i := 0; i < modelsConsecutiveFailures; i++ {
		mc.noteFailure(now, true)
	}
	// TTL 内当然退避；TTL 外但 30 分钟退避窗内也必须退避——这是本修复的增量。
	if !mc.triedRecently(now.Add(modelsCacheTTL+time.Minute), modelsCacheTTL) {
		t.Fatal("inside the failure backoff window the retry must be held off (issue #246)")
	}
	// 窗口经过：放行一次新尝试。
	if mc.triedRecently(now.Add(modelsFailureBackoff+time.Minute), modelsCacheTTL) {
		t.Fatal("the backoff window lapsed but the retry is still held (issue #246)")
	}
	// 成功重置。
	mc.resetFailures()
	if mc.triedRecently(now.Add(modelsCacheTTL+time.Minute), modelsCacheTTL) {
		t.Fatal("after a success the normal TTL must apply again (issue #246)")
	}
	// 瞬时失败不计入连击。
	mc2 := newModelCache()
	mc2.noteFailure(now, false)
	mc2.noteFailure(now.Add(time.Minute), false)
	mc2.noteFailure(now.Add(2*time.Minute), false)
	if mc2.triedRecently(now.Add(modelsCacheTTL+time.Minute), modelsCacheTTL) {
		t.Error("transient failures must not stretch the interval (issue #246)")
	}
	// isPermanentModelRefusal 的拼写。
	if !isPermanentModelRefusal(errors.New("404 Not Found")) || !isPermanentModelRefusal(errors.New("endpoint not found")) {
		t.Error("a 404/not-found error was not recognised as permanent (issue #246)")
	}
	if isPermanentModelRefusal(errors.New("context deadline exceeded")) {
		t.Error("a timeout was treated as permanent (issue #246)")
	}
}
