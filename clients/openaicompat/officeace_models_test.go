package openaicompat

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOfficeAceScanPublishesExactlyTheDesktopModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/models" {
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"data":[
			{"id":"deepseek-v4.1-flash","name":"DeepSeek-V4.1-Flash"},
			{"id":"deepseek-v4-flash-0731","name":"DeepSeek-V4-Flash-0731"},
			{"id":"glm-5"},
			{"id":"glm-5.1"},
			{"id":"glm-5.2"},
			{"id":"glm-5.2-zx"},
			{"id":"glm-5.3"},
			{"id":"glm-5.3-flash"},
			{"id":"glm-5.3-test"},
			{"id":"qwen3-32b"}
		]}`)
	}))
	defer srv.Close()

	o := officeAcePremiumSource(t, srv)
	officeAceAddAccountWithSession(t, o, srv, officeAceStoredSession{
		Region:        "cn-southwest-2",
		AccessKey:     "access",
		SecretKey:     "secret",
		SecurityToken: "security",
		ProjectID:     "project",
		ExpiresAt:     "2099-01-01T00:00:00Z",
	})
	models, err := o.source.RefreshModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(models))
	for _, model := range models {
		got = append(got, model.ID)
	}
	want := []string{
		"deepseek-v4-flash-0731",
		"deepseek-v4.1-flash",
		"glm-5.1",
		"glm-5.2",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("published models = %v, want exactly %v", got, want)
	}
	if o.source.wireModel != nil {
		t.Fatal("OfficeAce must not rewrite published model ids at the wire boundary")
	}
}
