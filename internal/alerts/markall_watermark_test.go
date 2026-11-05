package alerts

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 钉住 MarkAllRead 的水位早退（issue #208）：未读已为 0 且水位未变时，
// 再次调用不得重写整份文件——曾连打 3 次、每次都完整重写 63KB
// （alerts[]/read_at 全同，只有 saved 时间戳变）。
func TestMarkAllReadIsIdempotentAtTheWatermark(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alerts.json")
	s := NewStore(DefaultLimit, path)
	s.Add(Alert{At: time.Now(), Kind: "fault", Client: "p1", Detail: "告警一"})
	s.MarkAllRead()

	st1, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after first mark: %v", err)
	}
	for i := 0; i < 3; i++ {
		s.MarkAllRead()
	}
	st2, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after repeat marks: %v", err)
	}
	// 早退后文件不再被触碰：mtime 不变即证明零写入。
	if !st1.ModTime().Equal(st2.ModTime()) {
		t.Fatal("MarkAllRead 在水位已到最新时仍重写了整份文件（issue #208）")
	}
	if s.Unread() != 0 {
		t.Fatalf("unread = %d, want 0", s.Unread())
	}

	// 新告警到来后水位必须照常推进：早退不能挡住真正的标记。
	s.Add(Alert{At: time.Now(), Kind: "fault", Client: "p1", Detail: "告警二"})
	s.MarkAllRead()
	if s.Unread() != 0 {
		t.Fatalf("unread after a new alert + mark = %d, want 0", s.Unread())
	}
}
