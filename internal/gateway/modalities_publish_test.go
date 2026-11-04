package gateway

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"client2api/internal/core"
)

// 钉住 /v1/models 的模态能力声明（issue #188）。
//
// 全部 32 个模型里曾只有 2 个带能力标记，且键名方言不一（vision vs
// supports_images）——下游中转被迫手写能力表，写错就把带图请求误拦
// （magpie 预检 `this model accepts text only` 的定责链里，网关侧可修的
// 间接隐患就是这一层）。现在顶层发 OpenAI/magpie 同款的 modalities，
// 三种方言在发布层折算，方言原键保留。

func modalitiesList(t *testing.T, srv *http.Server) map[string]modelEntry {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/models = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data []modelEntry `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("parse: %v", err)
	}
	byID := make(map[string]modelEntry, len(out.Data))
	for _, e := range out.Data {
		byID[e.ID] = e
	}
	return byID
}

func TestModelsPublishStandardModalities(t *testing.T) {
	c := &testClient{name: "t", catalogue: []core.Model{
		{ID: "vision-flag", OwnedBy: "t", Extra: map[string]any{"supports_images": true}},
		{ID: "vision-dialect", OwnedBy: "t", Extra: map[string]any{"vision": true}},
		{ID: "modalities-dialect", OwnedBy: "t", Extra: map[string]any{"input_modalities": []string{"text", "image"}}},
		{ID: "text-only", OwnedBy: "t", Extra: map[string]any{"supports_images": false}},
		{ID: "unstated", OwnedBy: "t"},
	}}
	srv := newTestServer(t, c, NewStats(), nil)
	byID := modalitiesList(t, srv)

	// 三种方言统一折算成 image 输入；extra 原键保留兼容。
	for _, id := range []string{"t/vision-flag", "t/vision-dialect", "t/modalities-dialect"} {
		e, ok := byID[id]
		if !ok {
			t.Fatalf("%s missing from the list", id)
		}
		if e.Modalities == nil {
			t.Fatalf("%s: no modalities published (issue #188)", id)
		}
		if len(e.Modalities.Input) != 2 || e.Modalities.Input[1] != "image" {
			t.Errorf("%s: modalities.input = %v, want [text image]", id, e.Modalities.Input)
		}
		if len(e.Modalities.Output) != 1 || e.Modalities.Output[0] != "text" {
			t.Errorf("%s: modalities.output = %v, want [text]", id, e.Modalities.Output)
		}
	}

	// 显式 text-only 是一个事实，不是「没说」：要发出来，把 input 收窄成 [text]。
	if e := byID["t/text-only"]; e.Modalities == nil || len(e.Modalities.Input) != 1 || e.Modalities.Input[0] != "text" {
		t.Errorf("text-only: modalities = %+v, want input [text]（显式声明不许静默丢失，issue #188）", e.Modalities)
	}

	// 没说的保持省略：unknown ≠ 不支持。
	if e := byID["t/unstated"]; e.Modalities != nil {
		t.Errorf("unstated: modalities = %+v, want omitted（unknown 不得读成不支持，issue #188）", e.Modalities)
	}
}

func TestAutoAndGroupModalitiesAreTheUnion(t *testing.T) {
	// 两个平台服务同一个 canonical id：一个声明 image、一个没说 —— Auto 的
	// 声明是并集，有 image 说话者即 image。组内同理。
	vision := &testClient{name: "va", catalogue: []core.Model{
		{ID: "m1", OwnedBy: "va", Extra: map[string]any{"supports_images": true}},
	}}
	silent := &testClient{name: "vb", catalogue: []core.Model{
		{ID: "m1", OwnedBy: "vb"},
	}}
	reg := core.NewRegistry()
	reg.Add(vision)
	reg.Add(silent)
	srv := NewServer(Options{Registry: reg, Version: "test", Logger: log.New(io.Discard, "", 0)})
	byID := modalitiesList(t, srv)

	if e := byID["Auto/m1"]; e.Modalities == nil || len(e.Modalities.Input) != 2 {
		t.Errorf("Auto/m1: modalities = %+v, want input [text image]（一个 image 说话者即并集，issue #188）", e.Modalities)
	}
}
