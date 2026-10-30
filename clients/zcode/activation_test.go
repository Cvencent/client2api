package zcode

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// activationReports returns, for every activation event this transport saw,
// whether that request carried a credential, plus the event names in order.
func activationReports(t *testing.T, ft *fakeTransport) (authed []bool, events []string) {
	t.Helper()
	ft.mu.Lock()
	defer ft.mu.Unlock()
	for i, req := range ft.requests {
		if req.URL.Path != eventReportPath {
			continue
		}
		authed = append(authed, req.Header.Get("Authorization") != "")
		var body map[string]string
		if i < len(ft.bodies) && json.Unmarshal([]byte(ft.bodies[i]), &body) == nil {
			events = append(events, body["event"])
		}
	}
	return authed, events
}

// TestReportActivationCarriesTheAccountCredential is the regression test for the
// defect that left a pooled account unclaimable while a second account on the
// same device was granted the daily plan.  The liveness signal used to be sent
// anonymously, so it named only the device: the vendor had no way to attribute
// activity to this account and answered its preview with an empty plan list,
// for every scheduled attempt of the day.
func TestReportActivationCarriesTheAccountCredential(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	ft := routeTransport(t, nil)
	c := env.client(t, ft)
	id := addJWTAccount(t, c)

	c.reportActivation(context.Background(), c.pool.find(id))

	authed, events := activationReports(t, ft)
	if len(events) != len(activationEvents) {
		t.Fatalf("activation events = %v, want both of %v", events, activationEvents)
	}
	for i, a := range authed {
		if !a {
			t.Errorf("report %d (%s) was anonymous; a liveness report must name the account", i, events[i])
		}
	}
}

// TestReportActivationFallsBackToAnonymous pins the other half of the contract:
// when the endpoint will not take this account's credential, the liveness
// signal must still be sent.  Reporting nothing would be worse than reporting
// anonymously, since the vendor keys the daily grant on activity.
func TestReportActivationFallsBackToAnonymous(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	ft := routeTransport(t, map[string]func(*http.Request) (*http.Response, error){
		eventReportPath: func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("Authorization") != "" {
				return jsonResponse(http.StatusOK, `{"code":1004,"msg":"no permission"}`), nil
			}
			return jsonResponse(http.StatusOK, eventReportFixture), nil
		},
	})
	c := env.client(t, ft)
	id := addJWTAccount(t, c)

	c.reportActivation(context.Background(), c.pool.find(id))

	authed, events := activationReports(t, ft)
	if len(events) == 0 {
		t.Fatal("no activation report was sent at all")
	}
	sawAnonymous := false
	for i, a := range authed {
		if !a && strings.TrimSpace(events[i]) != "" {
			sawAnonymous = true
		}
	}
	if !sawAnonymous {
		t.Errorf("reports = %v (authed=%v), want an anonymous fallback after the refusal", events, authed)
	}
}
