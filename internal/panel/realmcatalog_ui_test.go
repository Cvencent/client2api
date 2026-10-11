package panel

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestModelColumnStripsOnlyDeclaredRealmPrefixes 钉住模型列的 realm 前缀口径。
// 面板原来把第一个冒号前的内容一律当成 realm 剥掉，于是 OpenRouter 的
// "nvidia/nemotron:free" 和 "openai/gpt-5:batch" 都只剩 "free" / "batch"，
// 一堆不同的模型挤在同一个名字下；反过来，OpenCode / ZCode 这种目录是裸 ID
// 的平台又因为找不到 "zai:" 一类前缀被过滤成空。
//
// catalogForRealm 是纯函数，所以这里用 node 把它真跑一遍：只有模块
// capabilities.realms 里明确声明的 code 才算前缀；没有匹配时只回退到不带任何
// 已声明前缀的裸 ID，WorkBuddy 的 global 账号因此不会借到 cn: 的模型。
func TestModelColumnStripsOnlyDeclaredRealmPrefixes(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; the realm-prefix behavior needs it to run the inline function")
	}
	block := poolStatsFuncBody(t, poolStatsUISource(t), "catalogForRealm") + "\n}"
	blockJSON, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("encoding the function for node: %v", err)
	}

	prog := `const fs = require("fs");
const out = process.argv[2];
try {
  const block = ` + string(blockJSON) + `;
  const catalogForRealm = new Function(block + "\nreturn catalogForRealm;")();
  const cases = [
    {
      name: "no declared realms keeps opaque ids whole",
      models: ["nvidia/nemotron:free", "openai/gpt-5:batch"],
      realm: "", realms: [], custom: false,
      want: ["nvidia/nemotron:free", "openai/gpt-5:batch"],
    },
    {
      name: "declared realm keeps only its own prefix and strips it",
      models: ["cn:glm-5.3", "global:gpt-5", "bare-model"],
      realm: "cn", realms: [{ code: "cn" }, { code: "global" }], custom: false,
      want: ["glm-5.3"],
    },
    {
      name: "bare catalogue falls back for an account whose realm has no prefix",
      models: ["glm-5.3", "glm-4.6"],
      realm: "zai", realms: [{ code: "bigmodel" }, { code: "zai" }], custom: false,
      want: ["glm-5.3", "glm-4.6"],
    },
    {
      name: "another realm's prefix is never borrowed",
      models: ["cn:glm-5.3"],
      realm: "global", realms: [{ code: "cn" }, { code: "global" }], custom: false,
      want: [],
    },
    {
      name: "an account with no realm sees only unqualified ids",
      models: ["cn:glm-5.3", "bare-model", "vendor/model:free"],
      realm: "", realms: [{ code: "cn" }, { code: "global" }], custom: false,
      want: ["bare-model", "vendor/model:free"],
    },
    {
      name: "custom source ids pass through untouched",
      models: ["free", "batch"],
      realm: "", realms: [{ code: "cn" }], custom: true,
      want: ["free", "batch"],
    },
  ];
  const got = cases.map(c => ({ name: c.name, want: c.want, got: catalogForRealm(c.models, c.realm, c.realms, c.custom) }));
  fs.writeFileSync(out, JSON.stringify({ ok: true, got }));
} catch (e) {
  fs.writeFileSync(out, JSON.stringify({ ok: false, error: String((e && e.message) || e) }));
}
`
	dir := t.TempDir()
	script := filepath.Join(dir, "realm-models.js")
	result := filepath.Join(dir, "realm-models.json")
	if err := os.WriteFile(script, []byte(prog), 0o600); err != nil {
		t.Fatalf("writing the node harness: %v", err)
	}
	if err := exec.Command(node, script, result).Run(); err != nil {
		t.Fatalf("node could not run the inline function: %v", err)
	}
	raw, err := os.ReadFile(result)
	if err != nil {
		t.Fatalf("node wrote no result: %v", err)
	}
	var got struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Got   []struct {
			Name string   `json:"name"`
			Want []string `json:"want"`
			Got  []string `json:"got"`
		} `json:"got"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decoding the node result: %v\n%s", err, raw)
	}
	if !got.OK {
		t.Fatalf("the inline function threw: %s", got.Error)
	}
	if len(got.Got) != 6 {
		t.Fatalf("node returned %d cases, want 6", len(got.Got))
	}
	for _, c := range got.Got {
		if strings.Join(c.Got, ",") != strings.Join(c.Want, ",") {
			t.Errorf("%s: catalog = %v, want %v", c.Name, c.Got, c.Want)
		}
	}
}
