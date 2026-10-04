package panel

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 券码二维码（开学季券码弹窗）的回归测试。
//
// 参考面板把券码本体编码成 QR（到店出示扫描），编码器是 app.js 里一段自带的精简
// 实现 —— 不能引外链 QR 服务，因为面板 CSP 只允许 self。移植时保留了同样的规格
// 子集（byte 模式 / ECC L / 版本 1-5 / 固定掩码 0），并已用 python 的 qrcode 库对
// 6 组输入（v1-v5，含中文 UTF-8）逐像素交叉验证，diff 全为 0。
//
// 这里的两个测试守两件不同的事：
//  1. 接线（静态）：编码器在、券卡上有 data-qr 按钮、点击真的会切换 .vc-qr。
//  2. 算法（动态）：把内联编码器交给 node 跑一遍，逐像素比对那次交叉验证留下的
//     黄金矩阵；没有 node 就跳过。静态断言看不出 GF(256) 常数写错、终止符漏加、
//     掩码没异或这类错，黄金矩阵能 —— 而二维码画错时页面不报任何错，只是扫不出来。
// ---------------------------------------------------------------------------

// voucherQRBlock 截出 shell 里内联的 QR 编码器：从它的段注释到下一个顶层函数。
func voucherQRBlock(t *testing.T, src string) string {
	t.Helper()
	start := strings.Index(src, "/* ── 精简 QR 编码器")
	if start < 0 {
		t.Fatal("index.html has no QR encoder block, but the voucher card renders a data-qr button")
	}
	end := strings.Index(src[start:], "function vcExpired(v) {")
	if end < 0 {
		t.Fatal("the QR encoder block is not terminated by vcExpired")
	}
	return src[start : start+end]
}

// TestVoucherQREncoderIsWiredIntoTheVoucherCard pins the wiring: without any one
// of these pieces the button is either absent, dead, or renders an unstyled blob.
func TestVoucherQREncoderIsWiredIntoTheVoucherCard(t *testing.T) {
	src := poolStatsUISource(t)
	block := voucherQRBlock(t, src)

	for _, fn := range []string{"qrGenPoly", "rsRem", "qrDataCodewords", "qrMatrix", "qrSVG"} {
		if !strings.Contains(block, "function "+fn+"(") {
			t.Errorf("the QR encoder block has no %s", fn)
		}
	}
	// gmul 必须改名：shell 里已经有一个同名助手，重名会让整段脚本在解析期就死掉
	// （而那只会在浏览器里表现为整个面板白屏）。
	if strings.Contains(block, "const gmul =") || strings.Contains(block, "function gmul(") {
		t.Error("the encoder declares a bare gmul; it collides with the shell's existing helper")
	}
	if !strings.Contains(block, "qrGmul(") {
		t.Error("the encoder does not call qrGmul")
	}

	card := poolStatsFuncBody(t, src, "vcCard")
	if !strings.Contains(card, "data-qr=") {
		t.Fatal("vcCard renders no data-qr button; the QR feature is unreachable")
	}
	// 没有 code 的券不能出现二维码按钮（qrMatrix("") 会编码出一个空码）。
	if !strings.Contains(card, "(v.code ?") {
		t.Error("vcCard does not guard the QR button on a non-empty code")
	}

	load := poolStatsFuncBody(t, src, "loadSchoolVouchers")
	if !strings.Contains(load, `querySelectorAll("button[data-qr]")`) {
		t.Error("loadSchoolVouchers never binds the data-qr buttons")
	}
	for _, want := range []string{`querySelector(".vc-qr")`, `className = "vc-qr"`, "qrSVG(qrMatrix(", "148"} {
		if !strings.Contains(load, want) {
			t.Errorf("the QR toggle is missing %q", want)
		}
	}
	// 样式必须还在，否则展开的二维码是没边框、没居中、没留白的一堆黑块。
	if !strings.Contains(src, ".vc .vc-qr {") {
		t.Error("the .vc-qr stylesheet rule is gone")
	}
}

// voucherQRGolden holds the matrices an independent implementation produced:
// python `qrcode` with byte mode, ECC L, mask_pattern=0 and a pinned version.
// The first vector is v1 (21x21, no alignment pattern); the second is v2
// (25x25: alignment pattern plus a multi-byte UTF-8 length count).
var voucherQRGolden = []struct {
	text string
	rows []string
}{
	{"A1B2-C3D4-E5F6", []string{
		"111111100010101111111",
		"100000100000101000001",
		"101110101010001011101",
		"101110100000101011101",
		"101110100100101011101",
		"100000100111001000001",
		"111111101010101111111",
		"000000001011100000000",
		"111011111011111000100",
		"110011010010100101001",
		"110100101100101111101",
		"110111000011110010000",
		"000110110111110010101",
		"000000001011000011010",
		"111111101011101010011",
		"100000101000010011011",
		"101110101011100011010",
		"101110100110101101010",
		"101110101000111110001",
		"100000101000000001010",
		"111111101101101100011",
	}},
	{"券码中文测试-abc123", []string{
		"1111111001001000101111111",
		"1000001000111110101000001",
		"1011101011101001001011101",
		"1011101001110000001011101",
		"1011101000100111101011101",
		"1000001001000110001000001",
		"1111111010101010101111111",
		"0000000011101111000000000",
		"1110111110110101111000100",
		"0101010100110100111011101",
		"0110111001000011011110100",
		"1001100100010110011010110",
		"1000011110001101100000000",
		"0000110100011101000110010",
		"1000111000011111011011000",
		"0100100000101101111111100",
		"1010101001111100111110011",
		"0000000010010100100011011",
		"1111111010100111101010110",
		"1000001010100111100011100",
		"1011101010100111111110100",
		"1011101001000111111011000",
		"1011101011101011101001101",
		"1000001011100111100100110",
		"1111111011000111010110011",
	}},
}

// TestVoucherQRMatrixMatchesTheCrossCheckedGoldenVectors runs the inline encoder
// through node and compares it pixel for pixel with the cross-checked matrices.
// It skips when node is absent, because the panel itself does not need node.
func TestVoucherQRMatrixMatchesTheCrossCheckedGoldenVectors(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; the QR golden vectors need it to run the inline encoder")
	}
	src := poolStatsUISource(t)
	voucherQRBlock(t, src) // fail early (and clearly) if the markers moved

	texts := make([]string, 0, len(voucherQRGolden))
	for _, g := range voucherQRGolden {
		texts = append(texts, g.text)
	}
	shellJSON, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("encoding the shell for node: %v", err)
	}
	textsJSON, err := json.Marshal(texts)
	if err != nil {
		t.Fatalf("encoding the QR inputs: %v", err)
	}

	// node 把结果写进文件而不是 stdout：这样整个测试不依赖任何管道，
	// 也能把失败原因（比如语法错）原样带回来。
	prog := `const fs = require("fs");
const out = process.argv[2];
try {
  const src = ` + string(shellJSON) + `;
  const a = src.indexOf("/* ── 精简 QR 编码器");
  const b = src.indexOf("function vcExpired(v) {");
  if (a < 0 || b <= a) throw new Error("the QR encoder block is not where the test expects it");
  const { qrMatrix } = new Function(src.slice(a, b) + "\nreturn { qrMatrix };")();
  const matrices = ` + string(textsJSON) + `.map(t => qrMatrix(t).map(r => r.map(v => (v ? 1 : 0)).join("")));
  fs.writeFileSync(out, JSON.stringify({ ok: true, matrices }));
} catch (e) {
  fs.writeFileSync(out, JSON.stringify({ ok: false, error: String((e && e.message) || e) }));
}
`
	dir := t.TempDir()
	script := filepath.Join(dir, "qrcheck.js")
	result := filepath.Join(dir, "qrcheck.json")
	if err := os.WriteFile(script, []byte(prog), 0o600); err != nil {
		t.Fatalf("writing the node harness: %v", err)
	}
	if err := exec.Command(node, script, result).Run(); err != nil {
		t.Fatalf("node could not run the inline encoder: %v", err)
	}
	raw, err := os.ReadFile(result)
	if err != nil {
		t.Fatalf("node wrote no result: %v", err)
	}
	var got struct {
		OK       bool       `json:"ok"`
		Error    string     `json:"error"`
		Matrices [][]string `json:"matrices"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decoding the node result: %v\n%s", err, raw)
	}
	if !got.OK {
		t.Fatalf("the inline QR encoder failed: %s", got.Error)
	}
	if len(got.Matrices) != len(voucherQRGolden) {
		t.Fatalf("node returned %d matrices, want %d", len(got.Matrices), len(voucherQRGolden))
	}

	for i, g := range voucherQRGolden {
		m := got.Matrices[i]
		if len(m) != len(g.rows) {
			t.Errorf("%q: matrix has %d rows, want %d", g.text, len(m), len(g.rows))
			continue
		}
		for r := range g.rows {
			if m[r] != g.rows[r] {
				t.Fatalf("%q: row %d is %q, want %q\n"+
					"the encoder drifted from the python-qrcode cross-check",
					g.text, r, m[r], g.rows[r])
			}
		}
	}
}
