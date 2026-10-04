package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"client2api/internal/alerts"
)

func TestAlertsEndpointReturnsNewestFirst(t *testing.T) {
	store := alerts.NewStore(10, "")
	store.Add(alerts.Alert{At: time.Unix(1, 0), Kind: "older", Client: "cline"})
	store.Add(alerts.Alert{At: time.Unix(2, 0), Kind: "newer", Client: "trae", Model: "trae/m", Count: 3})

	h := New(Options{Alerts: store, Started: time.Now()})
	rec := get(t, h, "/panel/api/alerts")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	got := decodeMap(t, rec)
	rows := usageRows(t, got, "alerts")
	if len(rows) != 2 {
		t.Fatalf("alerts has %d rows, want 2", len(rows))
	}
	first := usageRow(t, rows, 0)
	if first["kind"] != "newer" || first["client"] != "trae" || first["model"] != "trae/m" {
		t.Fatalf("alerts[0] = %v, want the newest trae alert", first)
	}
	if n := usageNum(t, first, "count"); n != 3 {
		t.Fatalf("alerts[0].count = %v, want 3", n)
	}
}

func TestAlertsReadEndpointClearsTheBadge(t *testing.T) {
	store := alerts.NewStore(10, "")
	store.Add(alerts.Alert{At: time.Unix(1, 0), Kind: "older", Client: "cline"})
	store.Add(alerts.Alert{At: time.Unix(2, 0), Kind: "newer", Client: "trae"})
	h := New(Options{Alerts: store, Started: time.Now()})

	got := decodeMap(t, get(t, h, "/panel/api/alerts"))
	if n := usageNum(t, got, "unread"); n != 2 {
		t.Fatalf("unread = %v, want 2", got["unread"])
	}
	req := httptest.NewRequest(http.MethodPost, "/panel/api/alerts", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := store.Unread(); got != 0 {
		t.Fatalf("unread after POST = %d, want 0", got)
	}
	if got := len(store.List()); got != 2 {
		t.Fatalf("history length = %d, want the two alerts kept", got)
	}
}
func TestAlertsPageHasClearButton(t *testing.T) {
	src := string(indexHTML)
	for _, want := range []string{
		`id="btnAlertsClear"`,
		`async function clearAlerts() {`,
		`api("/panel/api/alerts", { method: "DELETE" })`,
		`$("#btnAlertsClear").addEventListener("click", clearAlerts)`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("alerts clear UI is missing %s", want)
		}
	}
}

func TestAlertsDeleteClearsHistory(t *testing.T) {
	store := alerts.NewStore(10, "")
	store.Add(alerts.Alert{At: time.Unix(1, 0), Kind: "older", Client: "cline"})
	store.Add(alerts.Alert{At: time.Unix(2, 0), Kind: "newer", Client: "trae"})
	h := New(Options{Alerts: store, Started: time.Now()})

	req := httptest.NewRequest(http.MethodDelete, "/panel/api/alerts", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := len(store.List()); got != 0 {
		t.Fatalf("history length = %d after DELETE, want 0", got)
	}
	if got := store.Unread(); got != 0 {
		t.Fatalf("unread = %d after DELETE, want 0", got)
	}
}

func TestAlertsEndpointWithoutStoreReturnsEmptyArray(t *testing.T) {
	rec := get(t, New(Options{Started: time.Now()}), "/panel/api/alerts")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	got := decodeMap(t, rec)
	if rows := usageRows(t, got, "alerts"); len(rows) != 0 {
		t.Fatalf("alerts = %v, want an empty array", rows)
	}
}
