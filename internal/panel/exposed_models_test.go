package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"client2api/internal/core"
)

type exposedPanelTranscriptionClient struct {
	*fakeClient
}

func (c *exposedPanelTranscriptionClient) TranscriptionModels(context.Context) []core.Model {
	return []core.Model{{ID: "whisper-1"}}
}

func (c *exposedPanelTranscriptionClient) Transcribe(context.Context, *core.TranscriptionRequest) (core.TranscriptionResult, error) {
	return core.TranscriptionResult{}, nil
}

func TestExposedModelsEndpointReturnsTheUnfilteredFinalCatalogue(t *testing.T) {
	alpha := &fakeClient{name: "alpha", models: []core.Model{{ID: "m1"}}}
	beta := &fakeClient{name: "beta", models: []core.Model{{ID: "m2"}}}
	speech := &exposedPanelTranscriptionClient{fakeClient: &fakeClient{name: "speech"}}
	reg := registryOf(alpha, beta, speech)
	reg.AddAlias("alias", "alpha/m1")
	reg.SetModelGroups(map[string]core.ModelGroup{
		"shared": {Members: []core.ModelGroupMember{{Client: "alpha", Model: "m1"}, {Client: "beta", Model: "m2"}}},
	})
	reg.SetExposedModels([]string{"alpha/m1"}, true)

	h := New(Options{Registry: reg, Started: time.Now()})
	rec := get(t, h, "/panel/api/exposed_models")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got exposedModelsReport
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if !got.Configured || len(got.Selected) != 1 || got.Selected[0] != "alpha/m1" {
		t.Fatalf("selection = %#v, want configured alpha/m1", got)
	}
	ids := make(map[string]exposedModel, len(got.Models))
	for _, model := range got.Models {
		ids[model.ID] = model
	}
	for _, id := range []string{"alpha/m1", "beta/m2", "Auto/m1", "shared", "alias", "speech/whisper-1"} {
		if _, ok := ids[id]; !ok {
			t.Fatalf("unfiltered catalogue is missing %q: %#v", id, ids)
		}
	}
	if ids["shared"].Kind != "group" || len(ids["shared"].Members) != 2 {
		t.Fatalf("group row = %#v, want kind=group with both members", ids["shared"])
	}
	if ids["alias"].Kind != "alias" || ids["alias"].Target != "alpha/m1" {
		t.Fatalf("alias row = %#v, want target alpha/m1", ids["alias"])
	}
	if ids["speech/whisper-1"].Kind != "transcription" {
		t.Fatalf("transcription row = %#v, want kind=transcription", ids["speech/whisper-1"])
	}
}

func TestExposedModelsEndpointKeepsConfiguredIDsThatAreNoLongerInTheCatalogue(t *testing.T) {
	reg := registryOf(&fakeClient{name: "alpha", models: []core.Model{{ID: "m1"}}})
	reg.SetExposedModels([]string{"removed/model"}, true)
	h := New(Options{Registry: reg, Started: time.Now()})

	rec := get(t, h, "/panel/api/exposed_models")
	var got exposedModelsReport
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if len(got.Selected) != 1 || got.Selected[0] != "removed/model" {
		t.Fatalf("selected = %#v, want stale configured id preserved", got.Selected)
	}
	for _, model := range got.Models {
		if model.ID == "removed/model" {
			t.Fatalf("endpoint invented a stale catalogue row instead of letting the UI render it: %#v", model)
		}
	}
}
