package panel

import (
	"bytes"
	"context"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// importbundle_test.go covers the panel's half of the credential-bundle upload:
// pulling the document out of the two accepted envelope shapes, refusing the
// cases it can refuse cheaply, and passing a module's per-row report through
// without leaking whatever the rows contained.

// bundleCall records one handed-over document so a test can assert the panel
// passed the bytes and the name through unchanged.
type bundleCall struct {
	name string
	data string
}

// fakeBundleClient adds BundleImporter to fakeClient.
type fakeBundleClient struct {
	*fakeClient
	report core.BundleImportReport
	err    error

	mu    sync.Mutex
	calls []bundleCall
}

func (f *fakeBundleClient) ImportBundle(ctx context.Context, name string, data []byte) (core.BundleImportReport, error) {
	f.mu.Lock()
	f.calls = append(f.calls, bundleCall{name: name, data: string(data)})
	f.mu.Unlock()
	if f.err != nil {
		return core.BundleImportReport{}, f.err
	}
	return f.report, nil
}

func (f *fakeBundleClient) seen() []bundleCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]bundleCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// fakeBundleAccountClient adds the account listing on top, which is what makes
// the panel append the fresh account rows to its reply.
type fakeBundleAccountClient struct {
	*fakeAccountClient
	report core.BundleImportReport
}

func (f *fakeBundleAccountClient) ImportBundle(ctx context.Context, name string, data []byte) (core.BundleImportReport, error) {
	return f.report, nil
}

// bundlePost issues a POST with an explicit body and content type, because the
// shared post helper sends neither.
func bundlePost(t *testing.T, h http.Handler, target, contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPanelImportBundleTakesARawBody(t *testing.T) {
	c := &fakeBundleClient{
		fakeClient: &fakeClient{name: "wb"},
		report:     core.BundleImportReport{Total: 3, Imported: 2, Skipped: 1, Errors: []string{"uid=x: no token"}},
	}
	h := New(Options{Registry: registryOf(c), Started: time.Now()})
	rec := bundlePost(t, h, "/panel/api/clients/wb/import/bundle", "application/json", []byte(`[{"uid":"x"}]`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	got := decodeMap(t, rec)
	if got["ok"] != true || got["total"] != float64(3) || got["imported"] != float64(2) || got["skipped"] != float64(1) {
		t.Fatalf("reply = %v", got)
	}
	errs, _ := got["errors"].([]any)
	if len(errs) != 1 || errs[0] != "uid=x: no token" {
		t.Fatalf("errors = %v, want the module's own reason", got["errors"])
	}
	calls := c.seen()
	if len(calls) != 1 {
		t.Fatalf("module saw %d documents, want 1", len(calls))
	}
	// A body upload has no filename, so the module is told what it is.
	if calls[0].name != "body" {
		t.Fatalf("name = %q, want \"body\"", calls[0].name)
	}
	if calls[0].data != `[{"uid":"x"}]` {
		t.Fatalf("data = %q, want the bytes unchanged", calls[0].data)
	}
}

func TestPanelImportBundleTakesAMultipartUpload(t *testing.T) {
	c := &fakeBundleClient{fakeClient: &fakeClient{name: "wb"}}
	h := New(Options{Registry: registryOf(c), Started: time.Now()})

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "dump.json")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write([]byte(`[{"uid":"y"}]`)); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	rec := bundlePost(t, h, "/panel/api/clients/wb/import/bundle", mw.FormDataContentType(), buf.Bytes())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	calls := c.seen()
	if len(calls) != 1 {
		t.Fatalf("module saw %d documents, want 1", len(calls))
	}
	// The filename travels with the document so the module can label it, and it
	// is reduced to a base name so a crafted path cannot reach the report.
	if calls[0].name != "dump.json" {
		t.Fatalf("name = %q, want \"dump.json\"", calls[0].name)
	}
	if calls[0].data != `[{"uid":"y"}]` {
		t.Fatalf("data = %q, want the part's bytes", calls[0].data)
	}
}

func TestPanelImportBundleRefusesAModuleThatCannotImport(t *testing.T) {
	h := New(Options{Registry: registryOf(&fakeClient{name: "plain"}), Started: time.Now()})
	rec := bundlePost(t, h, "/panel/api/clients/plain/import/bundle", "application/json", []byte(`[]`))
	// 501 and not 404: the route exists, this module simply cannot do it, and
	// the panel distinguishes "not implemented" from "broken".
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rec.Code)
	}
	if msg, _ := decodeMap(t, rec)["error"].(string); !strings.Contains(msg, "plain") {
		t.Fatalf("error = %v, want it to name the module", decodeMap(t, rec)["error"])
	}
}

func TestPanelImportBundleRefusesWhatItCanRefuseCheaply(t *testing.T) {
	c := &fakeBundleClient{fakeClient: &fakeClient{name: "wb"}}
	h := New(Options{Registry: registryOf(c), Started: time.Now()})
	cases := []struct {
		name       string
		method     string
		body       []byte
		wantStatus int
		wantErr    string
	}{
		{"empty document", http.MethodPost, nil, http.StatusBadRequest, "empty"},
		{"not a post", http.MethodGet, []byte(`[]`), http.StatusMethodNotAllowed, "POST"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var rec *httptest.ResponseRecorder
			if tc.method == http.MethodPost {
				rec = bundlePost(t, h, "/panel/api/clients/wb/import/bundle", "application/json", tc.body)
			} else {
				rec = get(t, h, "/panel/api/clients/wb/import/bundle")
			}
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if msg, _ := decodeMap(t, rec)["error"].(string); !strings.Contains(msg, tc.wantErr) {
				t.Fatalf("error = %q, want it to mention %q", msg, tc.wantErr)
			}
			// Nothing reaches the module on a request the panel already refused.
			if calls := c.seen(); len(calls) != 0 {
				t.Fatalf("module saw %d documents, want none", len(calls))
			}
		})
	}
}

func TestPanelImportBundleReportsAModuleFailureAsBadGateway(t *testing.T) {
	c := &fakeBundleClient{
		fakeClient: &fakeClient{name: "wb"},
		err:        errors.New("invalid json: unexpected end of input"),
	}
	h := New(Options{Registry: registryOf(c), Started: time.Now()})
	rec := bundlePost(t, h, "/panel/api/clients/wb/import/bundle", "application/json", []byte(`{`))
	// A document the module cannot read is the module's 502, not the panel's
	// 400: only the owner of the format can say whether the bytes are valid.
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %s)", rec.Code, rec.Body.String())
	}
	if msg, _ := decodeMap(t, rec)["error"].(string); !strings.Contains(msg, "invalid json") {
		t.Fatalf("error = %q, want the module's reason", msg)
	}
}

func TestPanelImportBundleRedactsWhatItEchoesBack(t *testing.T) {
	c := &fakeBundleClient{
		fakeClient: &fakeClient{name: "wb"},
		report: core.BundleImportReport{
			Total: 1, Skipped: 1,
			Errors: []string{`uid=1: save auth failed: token=abcdef123456`},
		},
	}
	h := New(Options{Registry: registryOf(c), Started: time.Now()})
	rec := bundlePost(t, h, "/panel/api/clients/wb/import/bundle", "application/json", []byte(`[]`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// A module is expected not to echo secrets, but the report lands in a
	// browser, so the panel redacts rather than trusting that promise.
	if strings.Contains(body, "abcdef123456") {
		t.Fatalf("the reply echoed a credential: %s", body)
	}
	if !strings.Contains(body, "uid=1") {
		t.Fatalf("redaction ate the reason: %s", body)
	}
}

func TestPanelImportBundleRelistsAccountsWhenItCan(t *testing.T) {
	c := &fakeBundleAccountClient{
		fakeAccountClient: &fakeAccountClient{
			fakeClient: &fakeClient{name: "wb"},
			accounts:   []core.AccountRecord{{ID: "uid-1", Label: "one"}},
		},
		report: core.BundleImportReport{Total: 1, Imported: 1},
	}
	h := New(Options{Registry: registryOf(c), Started: time.Now()})
	rec := bundlePost(t, h, "/panel/api/clients/wb/import/bundle", "application/json", []byte(`[]`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	got := decodeMap(t, rec)
	rows, _ := got["accounts"].([]any)
	if len(rows) != 1 {
		t.Fatalf("accounts = %v, want the fresh list", got["accounts"])
	}
	// The panel answers with the account rows it can act on, so the page can
	// redraw without a second round trip.
	row, _ := rows[0].(map[string]any)
	if row["id"] != "uid-1" {
		t.Fatalf("account row = %v", row)
	}
}
