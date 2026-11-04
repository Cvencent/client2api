package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"client2api/internal/livecfg"
)

// ---------------------------------------------------------------------------
// package_detail_limit 的生效值随热重载走（issue #175）。
//
// 这个键过去在 panel.New 构造期读一次就固化：保存接口回「不用重启」（panel 不在
// coldConfigKeys），运行中的进程却永远用旧值，保存响应里的 panel 字段还回 0——
// 同一响应 config.panel=9 与 panel=0 互相打脸。修后生效值在 livecfg.Snapshot
// 里，reload 随 live.Store 一起换；configRead 与保存响应都从 live 快照解出。
// ---------------------------------------------------------------------------

func TestPackageDetailLimitFollowsTheLiveSnapshot(t *testing.T) {
	live := livecfg.New(livecfg.Snapshot{PackageDetailLimit: 5})
	p := bundlePanel(t.TempDir(), "")
	p.opts.Live = live

	// 构造期 opts 未设（bundlePanel 不传），configRead 应回退到 live 值。
	rec := httptest.NewRecorder()
	p.configRead(rec, httptest.NewRequest(http.MethodGet, "/panel/api/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("configRead = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Panel panelSettings `json:"panel"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Panel.PackageDetailLimit != 5 {
		t.Fatalf("初始生效值 = %d, want 5（live 快照是唯一真源）", body.Panel.PackageDetailLimit)
	}

	// 一次 reload（本测试直接换快照等价）后，configRead 报新值。
	live.Store(livecfg.Snapshot{PackageDetailLimit: 9})
	rec2 := httptest.NewRecorder()
	p.configRead(rec2, httptest.NewRequest(http.MethodGet, "/panel/api/config", nil))
	var body2 struct {
		Panel panelSettings `json:"panel"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &body2); err != nil {
		t.Fatal(err)
	}
	if body2.Panel.PackageDetailLimit != 9 {
		t.Fatalf("reload 后生效值 = %d, want 9：改了配置还得重启的假热键（issue #175）", body2.Panel.PackageDetailLimit)
	}
}

func TestConfigSaveResponseCarriesTheLivePanelValue(t *testing.T) {
	live := livecfg.New(livecfg.Snapshot{PackageDetailLimit: 9})
	cfgPath := filepath.Join(t.TempDir(), "client2api.json")
	os.WriteFile(cfgPath, []byte("{}"), 0o600)
	p := bundlePanel(t.TempDir(), cfgPath)
	p.opts.Live = live

	// 保存响应里的 panel 字段不再回 0：同一响应里 config.panel 与 panel
	// 必须说的是同一个数。
	body := `{"panel":{"package_detail_limit":9}}`
	req := httptest.NewRequest(http.MethodPost, "/panel/api/config", strings.NewReader(body))
	req.Host = "127.0.0.1"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.configWrite(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("configWrite = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Panel           panelSettings `json:"panel"`
		RestartRequired bool          `json:"restart_required"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Panel.PackageDetailLimit != 9 {
		t.Fatalf("保存响应 panel = %d, want 9：同响应两个答案互相打脸（issue #175）", resp.Panel.PackageDetailLimit)
	}
	if resp.RestartRequired {
		t.Fatal("panel 键保存后要求重启：它是热键，不该进重启名单（issue #175）")
	}
}
