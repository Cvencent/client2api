# 模型上下文与最大输出可编辑实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为所有平台模型提供官方上下文/最大输出预设，并在模型页支持查看来源、手动覆盖、恢复默认和统一保存。

**Architecture:** 扩展 `internal/modelmeta`：静态表提供官方预设，OverrideStore 提供操作者覆盖，Extra 解析兼容不同模块的键名。面板读写覆盖并返回逐字段来源；网关在未显式传 `max_tokens` 时按“手动覆盖 -> 模块 -> 静态预设”取值。

**Tech Stack:** Go 标准库、`net/http`、内嵌 HTML/JavaScript、`go test`。

---

### Task 1: 覆盖存储

**Files:**
- Create: `internal/modelmeta/overrides.go`
- Test: `internal/modelmeta/overrides_test.go`

- [ ] **Step 1: 写失败测试**

```go
func TestOverrideStorePersistsAndDeletes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model_context.json")
	s, err := OpenOverrideStore(path)
	if err != nil { t.Fatal(err) }
	if err := s.Set("zcode", "glm-5.3-flash", 1048576, 131072); err != nil { t.Fatal(err) }
	got, ok := s.Get("zcode", "glm-5.3-flash")
	if !ok || got.ContextLength != 1048576 || got.MaxOutputTokens != 131072 { t.Fatalf("got %+v ok=%v", got, ok) }
	reopened, err := OpenOverrideStore(path)
	if err != nil { t.Fatal(err) }
	if got, ok := reopened.Get("zcode", "glm-5.3-flash"); !ok || got.ContextLength != 1048576 { t.Fatalf("reopened %+v ok=%v", got, ok) }
	if err := reopened.Set("zcode", "glm-5.3-flash", 0, 131072); err != nil { t.Fatal(err) }
	if got, _ := reopened.Get("zcode", "glm-5.3-flash"); got.ContextLength != 0 || got.MaxOutputTokens != 131072 { t.Fatalf("partial delete %+v", got) }
	if err := reopened.Delete("zcode", "glm-5.3-flash"); err != nil { t.Fatal(err) }
	if _, ok := reopened.Get("zcode", "glm-5.3-flash"); ok { t.Fatal("delete did not remove entry") }
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/modelmeta -run TestOverrideStorePersistsAndDeletes -v`

Expected: FAIL，提示 `OpenOverrideStore` 未定义。

- [ ] **Step 3: 实现最小代码**

实现 `OverrideStore`、`OpenOverrideStore`、`Get`、`Set`、`Delete`、`Snapshot`。JSON 结构为 `{"clients":{"zcode":{"glm-5.3-flash":{"context_length":1048576,"max_output_tokens":131072}}}}`。写入使用 `os.CreateTemp`、`chmod 0600`、`Sync`、`Rename`。损坏 JSON 按空 store 处理并调用告警函数。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/modelmeta -run TestOverrideStorePersistsAndDeletes -v`

Expected: PASS。

---

### Task 2: 解析优先级和 Extra 兼容

**Files:**
- Modify: `internal/modelmeta/meta.go`
- Modify: `internal/modelmeta/extra.go`
- Modify: `internal/modelmeta/provider.go`
- Test: `internal/modelmeta/meta_test.go`
- Test: `internal/modelmeta/extra_test.go`
- Test: `internal/modelmeta/provider_test.go`

- [ ] **Step 1: 写失败测试**

```go
func TestOverrideWinsOverVendorAndStatic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model_context.json")
	s, _ := OpenOverrideStore(path)
	_ = s.Set("zcode", "glm-5.3-flash", 222222, 33333)
	p := New(Options{Client: "zcode", Overrides: s})
	got := p.Merge("glm-5.3-flash", Meta{ContextLength: 111111, MaxOutputTokens: 22222, Source: SourceVendor})
	if got.ContextLength != 222222 || got.MaxOutputTokens != 33333 { t.Fatalf("got %+v", got) }
	if got.SourceOf(FieldContextLength) != SourceManual || got.SourceOf(FieldMaxOutputTokens) != SourceManual { t.Fatalf("sources %+v", got.FieldSources) }
}
```

再补一个 `FromExtra` 自动键名测试，确认 `context_window`、`max_input_tokens`、`max_completion_tokens` 也能解析。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/modelmeta -run 'TestOverrideWinsOverVendorAndStatic|TestFromExtraAutoKeys' -v`

Expected: FAIL，提示 `SourceManual`、`Options.Overrides` 或自动键名未实现。

- [ ] **Step 3: 实现最小代码**

新增 `SourceManual = "manual"` 且 `sourceRank` 排第一。`Options` 增加 `Overrides *OverrideStore`。`Provider.resolve` 在静态表之前合并手动覆盖。`FromExtra` 在显式键未命中时按常见别名探测。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/modelmeta/...`

Expected: PASS。

---

### Task 3: 面板 API 与模型报告

**Files:**
- Modify: `internal/panel/models.go`
- Modify: `internal/panel/panel.go`
- Test: `internal/panel/modelcontext_test.go`

- [ ] **Step 1: 写失败测试**

```go
func TestModelContextOverridesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, _ := modelmeta.OpenOverrideStore(filepath.Join(dir, "model_context.json"))
	h := New(Options{DataDir: dir, ModelOverrides: store})
	req := httptest.NewRequest(http.MethodPost, "/panel/api/model_context", strings.NewReader(`{"clients":{"zcode":{"glm-5.3-flash":{"context_length":1048576,"max_output_tokens":131072}}}}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK { t.Fatalf("post %d %s", rr.Code, rr.Body.String()) }
	req = httptest.NewRequest(http.MethodGet, "/panel/api/model_context", nil)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "1048576") { t.Fatalf("get %d %s", rr.Code, rr.Body.String()) }
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/panel -run TestModelContextOverridesRoundTrip -v`

Expected: FAIL，路由不存在并返回 404。

- [ ] **Step 3: 实现最小代码**

面板 `Options` 增加 `ModelOverrides *modelmeta.OverrideStore`。注册 GET/POST `/panel/api/model_context`。`modelInfo` 增加 `context_length`、`max_output_tokens`、`context_source`、`max_output_source`、`context_editable`、`max_output_editable`。`modelsFor` 使用 `Provider.Merge` 合并 Extra、静态预设和覆盖。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/panel -run 'TestModelContextOverridesRoundTrip|TestModels' -v`

Expected: PASS。

---

### Task 4: 网关真实生效

**Files:**
- Modify: `internal/gateway/server.go`
- Modify: `cmd/client2api/main.go`
- Test: `internal/gateway/modellimits_test.go`

- [ ] **Step 1: 写失败测试**

```go
func TestModelOverrideMaxTokensWins(t *testing.T) {
	dir := t.TempDir()
	store, _ := modelmeta.OpenOverrideStore(filepath.Join(dir, "model_context.json"))
	_ = store.Set("limits", "m", 0, 777)
	up := &recordingClient{}
	srv := NewServer(Options{Registry: registryWith(up), ModelOverrides: store, APIKey: ""})
	// POST /v1/chat/completions，body 不携带 max_tokens，断言 up.lastReq.MaxTokens == 777
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/gateway -run TestModelOverrideMaxTokensWins -v`

Expected: FAIL，提示 `ModelOverrides` 字段不存在或仍使用模块值。

- [ ] **Step 3: 实现最小代码**

`gateway.Options` 增加 `ModelOverrides *modelmeta.OverrideStore`。在现有 `req.MaxTokens == nil` 分支中先查覆盖，再查 `ModelLimitsProvider`，最后查 `modelmeta.StaticMeta`。`cmd/client2api/main.go` 只创建一个 store，同时传给面板和网关。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/gateway -run 'TestModelOverrideMaxTokensWins|TestModelLimits' -v`

Expected: PASS。

---

### Task 5: 官方预设与模型页

**Files:**
- Modify: `internal/modelmeta/table.json`
- Modify: `internal/panel/index.html`
- Test: `internal/modelmeta/table_test.go`
- Test: `internal/panel/modelcontext_ui_test.go`

- [ ] **Step 1: 写失败测试**

表测试断言已知模型 `zcode/glm-5.3-flash` 为 `1048576/131072`，并断言 `kimi`、`tabbit`、`trae`、`opencode` 等平台的已确认模型有预设。UI 测试断言模型页包含 `/panel/api/model_context`、保存按钮、恢复默认按钮和来源标签。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/modelmeta ./internal/panel -run 'TestStaticTable|TestModelContextUI' -v`

Expected: FAIL，缺少模型条目或 UI 元素。

- [ ] **Step 3: 实现最小代码**

按公开官方文档补齐可确认的模型；没有官方依据的模型不猜。模型页表格改为输入框、来源徽标、恢复默认按钮，增加统一保存。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/modelmeta/... ./internal/panel/...`

Expected: PASS。

---

### Task 6: 全量验证与打包

- [ ] **Step 1: 格式化**

Run: `gofmt -w internal/modelmeta/*.go internal/panel/*.go internal/gateway/*.go cmd/client2api/main.go`

- [ ] **Step 2: 全量测试**

Run: `go build ./...; go test ./...`

Expected: 全部通过。

- [ ] **Step 3: 提升版本并打空包**

修改 `cmd/client2api/main.go` 和 `installer/setup/main.go` 到新版本，运行：

```powershell
pwsh -File installer\build.ps1 -NoData
```

Expected: 生成 `dist\client2api-setup-<version>.exe`，不包含账号数据。
