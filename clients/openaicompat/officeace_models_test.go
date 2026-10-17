package openaicompat

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOfficeAceScanFallsBackWhenSubscriptionTokenExpired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/models":
			fmt.Fprint(w, `{"data":[
				{"id":"glm-5.3"},
				{"id":"glm-5.3-flash"},
				{"id":"glm-5.3-test"}
			]}`)
		case "/v1/subscription":
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error_msg":"Bad request: the security token has expired","error_code":"APIG.0602"}`)
		case "/v3.0/OS-CREDENTIAL/securitytokens":
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error_msg":"Bad request: the security token has expired","error_code":"APIG.0602"}`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
		}
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
		t.Fatalf("refresh models with expired subscription token: %v", err)
	}
	if len(models) != 1 || models[0].ID != "glm-5.3-flash" {
		t.Fatalf("published models = %+v, want only glm-5.3-flash", models)
	}
}
