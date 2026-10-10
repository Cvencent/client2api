package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWaitForPanelAcceptsUnavailableHealthWhenShellLoads(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"unavailable","service":"client2api","clients":[]}`))
	})
	mux.HandleFunc("/panel/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><html><body>panel</body></html>`))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	var gatewayLog bytes.Buffer
	if err := waitForPanel(ctx, server.URL, &gatewayLog); err != nil {
		t.Fatalf("waitForPanel rejected a bootable gateway with no servable account: %v", err)
	}
}
