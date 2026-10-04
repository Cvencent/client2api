package prompt

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// systemRoles 提取 messages 中所有 role 值，用于断言改写后的角色序列。
func systemRoles(t *testing.T, body []byte) []string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, body)
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		t.Fatalf("messages not []any: %v", obj["messages"])
	}
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			t.Fatalf("msg not map: %v", m)
		}
		r, _ := mm["role"].(string)
		out = append(out, r)
	}
	return out
}

func TestRewriteReplacesSystemAndDeveloper(t *testing.T) {
	in := []byte(`{
		"model":"glm-5.2",
		"messages":[
			{"role":"system","content":"被替换的旧提示词"},
			{"role":"developer","content":"开发者指令"},
			{"role":"user","content":"你好"}
		],
		"metadata":{"conversation_id":"c1"}
	}`)
	out := Rewrite(in, "我是自有提示词")
	roles := systemRoles(t, out)
	wantRoles := []string{"system", "user"}
	if len(roles) != len(wantRoles) {
		t.Fatalf("roles=%v want %v", roles, wantRoles)
	}
	for i, r := range roles {
		if r != wantRoles[i] {
			t.Fatalf("roles[%d]=%q want %q (all=%v)", i, r, wantRoles[i], roles)
		}
	}
	// 头部 system 内容恰为自有提示词，旧 system/developer 内容零残留。
	var obj map[string]any
	json.Unmarshal(out, &obj)
	msgs := obj["messages"].([]any)
	first := msgs[0].(map[string]any)
	if first["content"] != "我是自有提示词" {
		t.Errorf("first system content=%v", first["content"])
	}
	if strings.Contains(string(out), "被替换的旧提示词") || strings.Contains(string(out), "开发者指令") {
		t.Errorf("old system/developer content leaked: %s", out)
	}
}

func TestRewriteKeepsUserAssistantToolUntouched(t *testing.T) {
	in := []byte(`{
		"messages":[
			{"role":"user","content":"u-content"},
			{"role":"assistant","content":"a-content"},
			{"role":"tool","tool_call_id":"t1","content":"tool-result"}
		]
	}`)
	out := Rewrite(in, "SYS")
	var obj map[string]any
	json.Unmarshal(out, &obj)
	msgs := obj["messages"].([]any)
	// 预期：system(新) + user + assistant + tool，顺序保留。
	if len(msgs) != 4 {
		t.Fatalf("len=%d", len(msgs))
	}
	roles := systemRoles(t, out)
	want := []string{"system", "user", "assistant", "tool"}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("roles=%v want %v", roles, want)
		}
	}
	// user/assistant/tool 字段逐字不动。
	userMsg := msgs[1].(map[string]any)
	if userMsg["content"] != "u-content" {
		t.Errorf("user content changed: %v", userMsg["content"])
	}
	toolMsg := msgs[3].(map[string]any)
	if toolMsg["tool_call_id"] != "t1" || toolMsg["content"] != "tool-result" {
		t.Errorf("tool changed: %v", toolMsg)
	}
}

func TestRewriteKeepsMetadataAndOtherFields(t *testing.T) {
	in := []byte(`{
		"model":"glm-5.2",
		"stream":true,
		"metadata":{"conversation_id":"c1","user_id":"u9"},
		"messages":[{"role":"user","content":"hi"}]
	}`)
	out := Rewrite(in, "SYS")
	var obj map[string]any
	json.Unmarshal(out, &obj)
	if obj["model"] != "glm-5.2" {
		t.Errorf("model changed: %v", obj["model"])
	}
	if obj["stream"] != true {
		t.Errorf("stream changed: %v", obj["stream"])
	}
	meta, ok := obj["metadata"].(map[string]any)
	if !ok || meta["conversation_id"] != "c1" || meta["user_id"] != "u9" {
		t.Errorf("metadata changed: %v", obj["metadata"])
	}
}

func TestRewriteInjectsSystemWhenAbsent(t *testing.T) {
	in := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	out := Rewrite(in, "SYS")
	roles := systemRoles(t, out)
	want := []string{"system", "user"}
	if len(roles) != len(want) {
		t.Fatalf("roles=%v want %v", roles, want)
	}
	for i, r := range roles {
		if r != want[i] {
			t.Fatalf("roles=%v want %v", roles, want)
		}
	}
}

func TestRewriteMultimodalContentUntouched(t *testing.T) {
	// user content 为多模态数组（text + image_url），Rewrite 只动 messages 层级，
	// 不应改动 content 内部结构。
	in := []byte(`{
		"messages":[
			{"role":"system","content":"old"},
			{"role":"user","content":[
				{"type":"text","text":"看图"},
				{"type":"image_url","image_url":{"url":"data:..."}}
			]}
		]
	}`)
	out := Rewrite(in, "SYS")
	var obj map[string]any
	json.Unmarshal(out, &obj)
	msgs := obj["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("len=%d", len(msgs))
	}
	userMsg := msgs[1].(map[string]any)
	arr, ok := userMsg["content"].([]any)
	if !ok || len(arr) != 2 {
		t.Fatalf("multimodal content changed: %v", userMsg["content"])
	}
	textPart := arr[0].(map[string]any)
	if textPart["type"] != "text" || textPart["text"] != "看图" {
		t.Errorf("text part changed: %v", textPart)
	}
}

func TestRewriteInvalidJSONReturnedAsIs(t *testing.T) {
	in := []byte(`{not valid json`)
	out := Rewrite(in, "SYS")
	if string(out) != string(in) {
		t.Errorf("invalid json should return as-is: got %s", out)
	}
}

func TestRewriteEmptyBodyReturnedAsIs(t *testing.T) {
	out := Rewrite([]byte{}, "SYS")
	if len(out) != 0 {
		t.Errorf("empty body should return as-is: got %s", out)
	}
}

func TestRewriteEmptyPromptReturnedAsIs(t *testing.T) {
	in := []byte(`{"messages":[{"role":"system","content":"old"}]}`)
	out := Rewrite(in, "")
	// systemPrompt 空 → 不改写，原样返回。
	if string(out) != string(in) {
		t.Errorf("empty prompt should return as-is: got %s", out)
	}
}

func TestLoadDefaultWhenFileEmpty(t *testing.T) {
	got, err := Load("custom", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != defaultPrompt {
		t.Errorf("Load() returned non-default prompt (len=%d vs %d)", len(got), len(defaultPrompt))
	}
	if len(got) == 0 {
		t.Error("default prompt is empty")
	}
}

func TestLoadFileOverride(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "my.md")
	want := "这是我的自定义人格入口。"
	os.WriteFile(fp, []byte(want), 0o600)
	got, err := Load("custom", fp)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("Load()=%q want %q", got, want)
	}
}

func TestLoadFileMissingFailsFast(t *testing.T) {
	if _, err := Load("custom", "/nonexistent/promp.md"); err == nil {
		t.Fatal("missing file should return error (fail fast)")
	}
}

// ---------------------------------------------------------------------------
// 本仓新增测试（移植后补充）：
//   - Rewrite 删除「全部」system/developer（含中途出现者）并在头部插入自有 system；
//   - Append 在开头连续 system/developer 块之后插入，既有消息逐字不动；
//   - Load 对 mode 的取词行为（passthrough/custom/append 均只按 file 取词）；
//   - Degraded 常量与参照逐字一致；
//   - //go:embed 的 defaultprompt.md 与参照文件逐字节一致（sha256 锁定）。
// 全部断言行为（角色序列、内容逐字、字节数），非仅断言「不报错」。
// ---------------------------------------------------------------------------

// msgRolesOf 提取 role 序列，允许消息元素不是 map（渲染为 "<non-map>"），
// 用于覆盖 Append 在遇到非 map 消息时停止扫描的边界。
func msgRolesOf(t *testing.T, body []byte) []string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, body)
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		t.Fatalf("messages not []any: %v", obj["messages"])
	}
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			out = append(out, "<non-map>")
			continue
		}
		r, _ := mm["role"].(string)
		out = append(out, r)
	}
	return out
}

// msgContentsOf 按顺序提取每条消息的 content（非 map 渲染为 "<non-map>"）。
func msgContentsOf(t *testing.T, body []byte) []string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, body)
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		t.Fatalf("messages not []any: %v", obj["messages"])
	}
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			out = append(out, "<non-map>")
			continue
		}
		c, _ := mm["content"].(string)
		out = append(out, c)
	}
	return out
}

// messagesJSON 把 messages 整体规范化序列化：同一批 Go 值时字符串必相同，
// 因此可用来断言「消息逐字不动」（重排/改字段/丢字段都会改变该串）。
func messagesJSON(t *testing.T, body []byte) string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, body)
	}
	raw, err := json.Marshal(obj["messages"])
	if err != nil {
		t.Fatalf("marshal messages: %v", err)
	}
	return string(raw)
}

// messagesJSONWithout 同上，但先摘掉第 drop 条消息（用于「插入点之外逐字不动」）。
func messagesJSONWithout(t *testing.T, body []byte, drop int) string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, body)
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		t.Fatalf("messages not []any: %v", obj["messages"])
	}
	if drop < 0 || drop >= len(msgs) {
		t.Fatalf("drop index %d out of range (len=%d)", drop, len(msgs))
	}
	kept := make([]any, 0, len(msgs)-1)
	kept = append(kept, msgs[:drop]...)
	kept = append(kept, msgs[drop+1:]...)
	raw, err := json.Marshal(kept)
	if err != nil {
		t.Fatalf("marshal messages: %v", err)
	}
	return string(raw)
}

// equalStrings 顺序敏感的字符串切片相等断言辅助。
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestRewriteRemovesEverySystemDeveloperMessage 中途出现的 system/developer 也必须
// 全部删除，且最终只有头部一条 system（内容恰为自有提示词）。
func TestRewriteRemovesEverySystemDeveloperMessage(t *testing.T) {
	in := []byte(`{"model":"glm-5.2","messages":[
		{"role":"system","content":"LEAK-s1"},
		{"role":"user","content":"u1"},
		{"role":"developer","content":"LEAK-d1"},
		{"role":"assistant","content":"a1"},
		{"role":"system","content":"LEAK-s2"},
		{"role":"developer","content":"LEAK-d2"},
		{"role":"user","content":"u2"}
	]}`)
	out := Rewrite(in, "GW-SYS")
	wantRoles := []string{"system", "user", "assistant", "user"}
	if got := msgRolesOf(t, out); !equalStrings(got, wantRoles) {
		t.Fatalf("roles=%v want %v", got, wantRoles)
	}
	contents := msgContentsOf(t, out)
	if contents[0] != "GW-SYS" {
		t.Errorf("头部 system 内容=%q want GW-SYS", contents[0])
	}
	for _, leak := range []string{
		`"content":"LEAK-s1"`,
		`"content":"LEAK-d1"`,
		`"content":"LEAK-s2"`,
		`"content":"LEAK-d2"`,
	} {
		if strings.Contains(string(out), leak) {
			t.Errorf("system/developer 内容残留（%s）：%s", leak, out)
		}
	}
	if !strings.Contains(string(out), `"model":"glm-5.2"`) {
		t.Errorf("model 字段丢失：%s", out)
	}
}

// TestRewritePreservesNonSystemMessagesVerbatim 非 system/developer 消息规范化序列化
// 必须与改写前完全一致（结构、嵌套多模态、tool_calls、未知字段都不动）。
func TestRewritePreservesNonSystemMessagesVerbatim(t *testing.T) {
	in := []byte(`{"messages":[
		{"role":"system","content":"old"},
		{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:x"}}],"name":"u"},
		{"role":"assistant","content":"a","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"r","extra_field":{"k":[1,2,3]}}
	]}`)
	want := messagesJSONWithout(t, in, 0) // 摘掉旧 system 后的消息序列
	out := Rewrite(in, "GW-SYS")
	if got := msgRolesOf(t, out); !equalStrings(got, []string{"system", "user", "assistant", "tool"}) {
		t.Fatalf("roles=%v want [system user assistant tool]", got)
	}
	if got := messagesJSONWithout(t, out, 0); got != want {
		t.Errorf("非 system 消息被改动：\n got=%s\nwant=%s", got, want)
	}
}

// TestAppendInsertsAfterLeadingSystemDeveloperBlock Append 的插入点在开头连续
// system/developer 块之后；块内与块外消息全部逐字不动。
func TestAppendInsertsAfterLeadingSystemDeveloperBlock(t *testing.T) {
	in := []byte(`{"messages":[
		{"role":"system","content":"client-sys"},
		{"role":"developer","content":"client-dev"},
		{"role":"user","content":"u1"},
		{"role":"assistant","content":"a1"},
		{"role":"system","content":"mid-sys"},
		{"role":"user","content":"u2"}
	]}`)
	out := Append(in, "GW-SYS")
	wantRoles := []string{"system", "developer", "system", "user", "assistant", "system", "user"}
	if got := msgRolesOf(t, out); !equalStrings(got, wantRoles) {
		t.Fatalf("roles=%v want %v", got, wantRoles)
	}
	wantContents := []string{"client-sys", "client-dev", "GW-SYS", "u1", "a1", "mid-sys", "u2"}
	if got := msgContentsOf(t, out); !equalStrings(got, wantContents) {
		t.Fatalf("contents=%v want %v", got, wantContents)
	}
	// 插入点 = 2：摘掉 index 2 后，消息序列与输入逐字一致。
	if got, want := messagesJSONWithout(t, out, 2), messagesJSON(t, in); got != want {
		t.Errorf("既有消息被改动：\n got=%s\nwant=%s", got, want)
	}
	// 网关消息角色必须是 system（上游 role 白名单不含 developer）。
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	gw, ok := obj["messages"].([]any)[2].(map[string]any)
	if !ok || gw["role"] != "system" {
		t.Errorf("网关消息 role=%v want system", gw["role"])
	}
	if gw["content"] != "GW-SYS" {
		t.Errorf("网关消息 content=%v want GW-SYS", gw["content"])
	}
	// 没有凭空新增 developer。
	if got, want := strings.Count(string(out), `"role":"developer"`), strings.Count(string(in), `"role":"developer"`); got != want {
		t.Errorf("developer 条数=%d want %d", got, want)
	}
}

// TestAppendWithNoLeadingBlockInsertsAtVeryHead 第一条就不是 system/developer
// （含中途 system）→ 插入点 0，中途 system 原位不动。
func TestAppendWithNoLeadingBlockInsertsAtVeryHead(t *testing.T) {
	in := []byte(`{"messages":[{"role":"user","content":"u1"},{"role":"system","content":"mid"},{"role":"assistant","content":"a1"}]}`)
	out := Append(in, "GW-SYS")
	wantRoles := []string{"system", "user", "system", "assistant"}
	if got := msgRolesOf(t, out); !equalStrings(got, wantRoles) {
		t.Fatalf("roles=%v want %v", got, wantRoles)
	}
	wantContents := []string{"GW-SYS", "u1", "mid", "a1"}
	if got := msgContentsOf(t, out); !equalStrings(got, wantContents) {
		t.Fatalf("contents=%v want %v", got, wantContents)
	}
	if got, want := messagesJSONWithout(t, out, 0), messagesJSON(t, in); got != want {
		t.Errorf("既有消息被改动：\n got=%s\nwant=%s", got, want)
	}
}

// TestAppendStopsAtNonMapMessage 非 map 消息即停止扫描，插入点回到最前，
// 非 map 元素本身原样保留。
func TestAppendStopsAtNonMapMessage(t *testing.T) {
	in := []byte(`{"messages":[1,{"role":"system","content":"s"},{"role":"user","content":"u"}]}`)
	out := Append(in, "GW-SYS")
	wantRoles := []string{"system", "<non-map>", "system", "user"}
	if got := msgRolesOf(t, out); !equalStrings(got, wantRoles) {
		t.Fatalf("roles=%v want %v", got, wantRoles)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if n, ok := obj["messages"].([]any)[1].(float64); !ok || n != 1 {
		t.Errorf("非 map 元素被改动：%#v", obj["messages"].([]any)[1])
	}
}

// TestAppendStopsAtRoleMissingMessage 无 role 字段的 map 消息同样终止扫描。
func TestAppendStopsAtRoleMissingMessage(t *testing.T) {
	in := []byte(`{"messages":[{"content":"no-role"},{"role":"system","content":"s"}]}`)
	out := Append(in, "GW-SYS")
	wantRoles := []string{"system", "", "system"}
	if got := msgRolesOf(t, out); !equalStrings(got, wantRoles) {
		t.Fatalf("roles=%v want %v", got, wantRoles)
	}
}

// TestAppendGuardsReturnInputAsIs 空 body / nil body / 空提示词 / 坏 JSON →
// 原样返回（绝不失败，不阻塞转发）。
func TestAppendGuardsReturnInputAsIs(t *testing.T) {
	valid := []byte(`{"messages":[{"role":"user","content":"u"}]}`)
	cases := []struct {
		name string
		in   []byte
		sp   string
	}{
		{"空 body", []byte{}, "GW"},
		{"nil body", nil, "GW"},
		{"空提示词", valid, ""},
		{"坏 JSON", []byte(`{not json`), "GW"},
	}
	for _, tc := range cases {
		got := Append(tc.in, tc.sp)
		if string(got) != string(tc.in) {
			t.Errorf("%s：got %q want %q", tc.name, got, tc.in)
		}
	}
}

// TestAppendNoMessagesFieldInjectsSingleSystem 无 messages 字段 / 类型不符 →
// messages=[网关 system]，其余字段原样保留。
func TestAppendNoMessagesFieldInjectsSingleSystem(t *testing.T) {
	for _, in := range [][]byte{
		[]byte(`{"model":"glm-5.2","stream":true}`),
		[]byte(`{"model":"glm-5.2","messages":"not-an-array"}`),
	} {
		out := Append(in, "GW-SYS")
		if got := msgRolesOf(t, out); !equalStrings(got, []string{"system"}) {
			t.Fatalf("in=%s roles=%v want [system]", in, got)
		}
		if c := msgContentsOf(t, out); c[0] != "GW-SYS" {
			t.Errorf("content=%q want GW-SYS", c[0])
		}
		if !strings.Contains(string(out), `"model":"glm-5.2"`) {
			t.Errorf("model 丢失：%s", out)
		}
	}
}

// TestRewriteNoMessagesFieldInjectsSingleSystem Rewrite 同路径：无 messages →
// 头部单条 system，其余字段保留。
func TestRewriteNoMessagesFieldInjectsSingleSystem(t *testing.T) {
	in := []byte(`{"model":"glm-5.2","stream":true}`)
	out := Rewrite(in, "GW-SYS")
	if got := msgRolesOf(t, out); !equalStrings(got, []string{"system"}) {
		t.Fatalf("roles=%v want [system]", got)
	}
	if c := msgContentsOf(t, out); c[0] != "GW-SYS" {
		t.Errorf("content=%q want GW-SYS", c[0])
	}
	if !strings.Contains(string(out), `"model":"glm-5.2"`) {
		t.Errorf("model 丢失：%s", out)
	}
}

// TestRewriteNilBodyReturnedAsIs nil body 不 panic 且原样返回 nil。
func TestRewriteNilBodyReturnedAsIs(t *testing.T) {
	if out := Rewrite(nil, "SYS"); out != nil {
		t.Errorf("nil body → got %q want nil", out)
	}
}

// TestLoadModesAgreeOnFileResolution Load 只按 file 取词，mode 是透传参数：
// passthrough/custom/append 三种 mode 的取词行为完全相同（参照语义）。
func TestLoadModesAgreeOnFileResolution(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "persona.md")
	want := "自定义人格：只做被要求的事。"
	if err := os.WriteFile(fp, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.md")
	for _, mode := range []string{"passthrough", "custom", "append"} {
		// file 空 → 内置 defaultprompt.md（embed 读出，非空）。
		got, err := Load(mode, "")
		if err != nil {
			t.Fatalf("Load(%q,\"\") err=%v", mode, err)
		}
		if got != defaultPrompt || len(got) == 0 {
			t.Errorf("Load(%q,\"\") 非内置默认：len=%d", mode, len(got))
		}
		// file 非空 → 文件内容逐字。
		got, err = Load(mode, fp)
		if err != nil {
			t.Fatalf("Load(%q,file) err=%v", mode, err)
		}
		if got != want {
			t.Errorf("Load(%q,file)=%q want %q", mode, got, want)
		}
		// 文件缺失 → fail fast（error 含路径，且可 errors.Is 到 fs.ErrNotExist）。
		_, err = Load(mode, missing)
		if err == nil {
			t.Fatalf("Load(%q,missing) 应报错（fail fast）", mode)
		}
		if !strings.Contains(err.Error(), "prompt file") || !strings.Contains(err.Error(), missing) {
			t.Errorf("Load(%q,missing) err=%v 未含文件路径", mode, err)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Load(%q,missing) err=%v 未包装 fs.ErrNotExist", mode, err)
		}
	}
}

// TestLoadIgnoresUnknownMode Load 不校验 mode（校验在调用方 config.normalizePrompt），
// 未知/空/大小写变体一律按 file 取词，不 panic、不报错。
func TestLoadIgnoresUnknownMode(t *testing.T) {
	for _, mode := range []string{"", "unknown", "CUSTOM", "Passthrough", " append "} {
		got, err := Load(mode, "")
		if err != nil {
			t.Fatalf("Load(%q,\"\") err=%v", mode, err)
		}
		if got != defaultPrompt {
			t.Errorf("Load(%q,\"\") 非内置默认", mode)
		}
	}
}

// TestLoadFileReturnedByteForByte 文件内容按字节原样返回（含 CRLF 与中文，
// 不做 trim/换行规范化）。
func TestLoadFileReturnedByteForByte(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "crlf.md")
	raw := []byte("第一行\r\n第二行\r\n")
	if err := os.WriteFile(fp, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load("custom", fp)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(raw) || len(got) != len(raw) {
		t.Errorf("Load 未逐字节返回：len=%d want %d", len(got), len(raw))
	}
}

// TestDegradedConstantMatchesReference Degraded 逐字等于参照常量，且保持
// 「极简中性、纯 ASCII、无首尾空白」——它用于误报降级重试。
func TestDegradedConstantMatchesReference(t *testing.T) {
	const want = "You are a helpful assistant. Respond in the user's language, follow the user's instructions, and be direct and concise."
	if Degraded != want {
		t.Errorf("Degraded=%q want %q", Degraded, want)
	}
	if len(Degraded) != 119 {
		t.Errorf("len(Degraded)=%d want 119", len(Degraded))
	}
	if strings.TrimSpace(Degraded) != Degraded {
		t.Error("Degraded 有首尾空白")
	}
	for i, r := range Degraded {
		if r > 127 {
			t.Errorf("Degraded[%d]=%q 非 ASCII（中性提示词不应含非 ASCII）", i, r)
		}
	}
}

// TestEmbeddedDefaultPromptMatchesReferenceBytes defaultprompt.md 被 //go:embed
// 读到，且规范化换行后与参照文件逐字节一致（sha256 锁定）+ 行数/字节数一致。
func TestEmbeddedDefaultPromptMatchesReferenceBytes(t *testing.T) {
	if defaultPrompt == "" {
		t.Fatal("embed 的 defaultPrompt 为空")
	}
	const wantSHA = "140150d658e967866566333f57b332101b8aedd207fbfdb2c1b96b77376c22a6"
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte(defaultPrompt))); got != wantSHA {
		t.Errorf("defaultPrompt sha256=%s want %s（与参照 defaultprompt.md 不一致）", got, wantSHA)
	}
	if len(defaultPrompt) != 2134 {
		t.Errorf("len(defaultPrompt)=%d want 2134", len(defaultPrompt))
	}
	if n := strings.Count(defaultPrompt, "\n"); n != 38 {
		t.Errorf("defaultPrompt 换行数=%d want 38", n)
	}
	if !utf8.ValidString(defaultPrompt) {
		t.Error("defaultPrompt 不是合法 UTF-8")
	}
	if strings.ContainsRune(defaultPrompt, '\uFFFD') {
		t.Error("defaultPrompt 含替换字符 U+FFFD（编码已损坏）")
	}
	if strings.ContainsRune(defaultPrompt, '\r') {
		t.Error("defaultPrompt 仍含 CR，换行规范化失效")
	}
	nonASCII := 0
	for _, r := range defaultPrompt {
		if r > 127 {
			nonASCII++
		}
	}
	if nonASCII == 0 {
		t.Error("defaultPrompt 无任何非 ASCII 字符（中文提示词疑似丢失）")
	}
	// embed 的内容必须就是包目录下的那个文件。
	onDisk, err := os.ReadFile("defaultprompt.md")
	if err != nil {
		t.Fatalf("read defaultprompt.md: %v", err)
	}
	normalizedOnDisk := strings.ReplaceAll(string(onDisk), "\r\n", "\n")
	if normalizedOnDisk != defaultPrompt {
		t.Errorf("embed 内容与磁盘文件不一致：disk=%d bytes embed=%d bytes", len(normalizedOnDisk), len(defaultPrompt))
	}
	if !strings.HasPrefix(defaultPrompt, "# ") {
		t.Errorf("首行不是一级标题：%q", strings.SplitN(defaultPrompt, "\n", 2)[0])
	}
}
