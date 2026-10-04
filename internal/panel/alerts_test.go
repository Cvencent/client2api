package panel

import (
	"net/http"
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
