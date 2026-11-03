package panel

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// MODULE-CONTRACT「The one rule」的两条 import 约束必须有人看着（issue #149）。
//
// docs/MODULE-CONTRACT.md:13-15 写明：
//
//   - Nothing under internal/ or cmd/ may import a clients/* package;
//   - No clients/* package may import another clients/* package
//     (clients/all is the ONLY file that knows the full client list).
//
// 在这两条有人看着之前，它们只是文档里的一句话：报告方实测向 internal/panel
// 注入一个 `import _ "client2api/clients/all"`、向 clients/loomy 注入一个
// `import _ "client2api/clients/raccoon"`，gofmt / vet / build / test 四道
// 全绿——同一份文档里能量化的约定（接口四方法、clients/all 名单、注册名）
// 都有机械兜底，唯独这两条最架构性的规则零覆盖。
//
// 判据是**扫描 Go 源文件的 import 块**而不是 `go list`：不依赖工具链的输出
// 格式，测试自身可读、失败时直接点名文件与 import 行。
// ---------------------------------------------------------------------------

// importRe matches a single import path inside a Go import block.  Only the
// `client2api/...` module's own packages matter: vendor and stdlib paths are
// out of scope.
var importRe = regexp.MustCompile(`"client2api/([^"]+)"`)

// goFiles walks root and returns every non-test .go file path.
func goFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "testdata" || info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// importLines returns the client2api paths imported by one file, with the
// line numbers they appear on, so a failure can name the exact import.
func importLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []string
	for i, line := range strings.Split(string(raw), "\n") {
		for _, m := range importRe.FindAllStringSubmatch(line, -1) {
			out = append(out, filepath.ToSlash(path)+":"+strconvIt(i+1)+" client2api/"+m[1])
		}
	}
	return out
}

func strconvIt(n int) string {
	return strings.TrimSpace(strings.Join(strings.Fields(string(rune(0)))[:0], "") + itoa(n))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// TestInternalAndCmdNeverImportClients pins rule 1: the panel, the gateway and
// every cmd/ entry point reach modules through core.Client interfaces and the
// clients/all registration, never through a module package directly.
func TestInternalAndCmdNeverImportClients(t *testing.T) {
	var violations []string
	for _, root := range []string{"../../internal", "../../cmd"} {
		for _, path := range goFiles(t, root) {
			for _, line := range importLines(t, path) {
				if !strings.Contains(line, "client2api/clients/") {
					continue
				}
				// clients/all is the one clients/* package internal/ and cmd/
				// MAY import: it is the registry itself, and the cmd entry
				// points' blank import of it is the documented wiring.  A
				// specific module's package would reach past the interface.
				// (Match AFTER the "lineNo " prefix: a file path can itself
				// contain client2api -- e.g. cmd/client2api/main.go.)
				imp := line[strings.LastIndex(line, " client2api/")+1:]
				if strings.TrimPrefix(imp, "client2api/clients/") == "all" {
					continue
				}
				violations = append(violations, line)
			}
		}
	}
	if len(violations) > 0 {
		t.Errorf("internal/ 或 cmd/ 里 import 了具体模块包（MODULE-CONTRACT 第一条，issue #149；"+
			"唯一合法的是 clients/all 注册表）：\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// TestClientsNeverImportEachOther pins rule 2: modules talk through
// core.Client interfaces, never through each other's packages.  clients/all
// is the documented exception -- it is the one file allowed to know the list.
func TestClientsNeverImportEachOther(t *testing.T) {
	var violations []string
	for _, path := range goFiles(t, "../../clients") {
		slashed := filepath.ToSlash(path)
		// A module's own package import is its own name -- always legal.
		parts := strings.Split(slashed, "/")
		self := parts[len(parts)-2] // clients/<name>/file.go
		// The one legal home for the full list: clients/all may (and must)
		// import every module -- that blank-import list IS the registry.
		if self == "all" {
			continue
		}
		for _, line := range importLines(t, path) {
			// Only edges INTO clients/* are violations: internal/core and
			// internal/fingerprint are the documented downstream direction.
			if !strings.Contains(line, "client2api/clients/") {
				continue
			}
			imp := line[strings.LastIndex(line, " client2api/")+1:]
			rest := strings.TrimPrefix(imp, "client2api/clients/")
			if rest == "all" {
				// Only clients/all may be imported by cmd/ entry points; a
				// module importing it would drag the whole registry in.
				violations = append(violations, line+"  (module → clients/all)")
				continue
			}
			other := strings.Split(rest, "/")[0]
			if other != self {
				violations = append(violations, line)
			}
		}
	}
	if len(violations) > 0 {
		t.Errorf("clients/* 之间出现了互相 import（MODULE-CONTRACT 第二条，issue #149）：\n  %s",
			strings.Join(violations, "\n  "))
	}
}
