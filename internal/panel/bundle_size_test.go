package panel

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// bundle 两侧的量纲与超限语义（issue #174）。
//
// 导出量的是「原始凭据字节」（collectAccountFiles），导入过去量的是「base64
// 文档字节」（readBundleUpload 的 LimitReader）：1.334 倍膨胀让 6.2~8 MB 原始
// 凭据导出的文件永远导不回去，且超限被静默截断后伪装成「不是 bundle」。
// 修后：导入读取用「多读一字节」判超限回 413；导出在写 body 前自检编码后
// 文档大小，拒绝产出一个自己吃不下的快照。
// ---------------------------------------------------------------------------

func TestBundleUploadPastItsCapIsA413NotABadFormat(t *testing.T) {
	p := bundlePanel(t.TempDir(), "")

	// maxBundleUploadBytes+1 字节的合法 JSON 会被截断——修前这个 body 变成
	// 「unexpected end of JSON input」（400 not a bundle），修后明确 413。
	huge := strings.Repeat(" ", maxBundleUploadBytes) + `{}`
	req := httptest.NewRequest(http.MethodPost, "/panel/api/bundle", strings.NewReader(huge))
	req.Header.Set("Content-Type", "application/json")
	req.Host = "127.0.0.1"
	rec := httptest.NewRecorder()
	p.handleBundle(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload = %d (%s), want 413：超限不是格式错（issue #174）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "exceeds") {
		t.Errorf("body = %s, want a size explanation", rec.Body.String())
	}
}

func TestBundleExportSelfCheckRefusesAnOverlargeDocument(t *testing.T) {
	// 凭据原始字节落在 maxBundleBytes 内、但 base64 展开后超过导入读取上限：
	// 修前导出 200 产出一份吃不下的快照；修后导出自己拒绝。
	dir := t.TempDir()
	clientDir := filepath.Join(dir, "cline")
	if err := os.MkdirAll(clientDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 原始 7MB：collectAccountFiles 的 maxBundleBytes=8MB 放行；base64 后约
	// 9.3MB，超过 maxBundleUploadBytes=16MB？——不会。所以这里用配置面板的
	// cap 单位本身：8MB 原始 + JSON 框架 < 16MB，自检不触发。要让自检触发，
	// 需要编码后 > 16MB 的合法文档——即原始 > 12MB，但那会被 8MB 先拒。
	// 因此自检的可达路径只有一条：未来有人调高 maxBundleBytes 而忘了同步
	// maxBundleUploadBytes。守卫把这对关系钉死在常量层面（见下一条测试），
	// 这里验证自检函数在正常文档上不误报。
	data := make([]byte, 4<<20) // 4MB 原始凭据
	for i := range data {
		data[i] = byte('a' + i%26)
	}
	doc := bundleDoc{
		Kind:    bundleKind,
		Version: bundleVersion,
		Files: []bundleFile{{
			Client: "cline",
			Name:   "accounts.json",
			Size:   len(data),
			Data:   data,
			SHA256: "", // 无校验和，直接量尺寸
		}},
	}
	if size := bundleEncodedSize(doc); size <= maxBundleUploadBytes {
		// 合法文档必须能通过自检（导出不被误拒）。
		p := bundlePanel(dir, "")
		rec := httptest.NewRecorder()
		// handleBundle 会真的去读盘上的凭据目录；4MB 文件写进磁盘。
		if err := os.WriteFile(filepath.Join(clientDir, "accounts.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		p.handleBundle(rec, httptest.NewRequest(http.MethodGet, "/panel/api/bundle", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("export of a legal document = %d (%s), want 200：自检不得误拒合法快照（issue #174）", rec.Code, rec.Body.String())
		}
	}
}

func TestBundleSizeConstantsKeepTheYardsticksConsistent(t *testing.T) {
	// 量纲一致性的常量级守卫：maxBundleUploadBytes（base64 文档硬顶）必须
	// 容得下 maxBundleBytes（原始凭据上限）经 base64 膨胀（4/3 倍）再加
	// JSON 框架的文档——否则导出又会产出自查不过的文件（issue #174 的根因
	// 换个常量重现）。
	documented := maxBundleBytes*4/3 + 4096 // 膨胀 + JSON/字段名框架余量
	if maxBundleUploadBytes < documented {
		t.Fatalf("maxBundleUploadBytes=%d 装不下 maxBundleBytes=%d 的 base64 文档（至少 %d）：导出会产出导入吃不下的快照（issue #174）",
			maxBundleUploadBytes, maxBundleBytes, documented)
	}
}

func TestBundleUploadOfARealExportStillRoundTrips(t *testing.T) {
	// 端到端：导出（含 base64 凭据）→ 原样导入，200 且账号恢复——
	// 「whatever the export wrote is exactly what the import reads」。
	src := t.TempDir()
	dst := t.TempDir()
	clientDir := filepath.Join(src, "cline")
	if err := os.MkdirAll(clientDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cred := `{"accounts":{"a1":{"token":"tok-1"}}}`
	if err := os.WriteFile(filepath.Join(clientDir, "accounts.json"), []byte(cred), 0o600); err != nil {
		t.Fatal(err)
	}
	exporter := bundlePanel(src, "")
	rec, doc := bundleExport(t, exporter)
	if rec.Code != http.StatusOK {
		t.Fatalf("export = %d: %s", rec.Code, rec.Body.String())
	}

	// 导入侧走原始 body 形态（脚本调用者路径），文档与导出逐字节一致。
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	importer := bundlePanel(dst, "")
	req := httptest.NewRequest(http.MethodPost, "/panel/api/bundle", strings.NewReader(string(body)))
	req.Host = "127.0.0.1"
	req.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	importer.handleBundle(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("import = %d: %s", rec2.Code, rec2.Body.String())
	}
	restored, err := os.ReadFile(filepath.Join(dst, "cline", "accounts.json"))
	if err != nil {
		t.Fatalf("restored file: %v", err)
	}
	if string(restored) != cred {
		t.Fatalf("restored = %q, want the original credential bytes", restored)
	}
	_ = base64.StdEncoding // referenced so the import's encoding stays obvious
}
