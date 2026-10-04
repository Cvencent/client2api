package panel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// fakeQuotaClient implements all three quota providers on top of the account
// manager fake, and records enough to tell "the route asked the module" apart
// from "the route made the numbers up".
type fakeQuotaClient struct {
	*fakeAccountClient

	mu       sync.Mutex
	balances map[string]core.Balance
	balErr   map[string]error
	balID    []string
	balSoon  []time.Duration

	packages map[string]core.PackageReport
	pkgErr   map[string]error

	vouchers    map[string][]core.Voucher
	voucherErr  map[string]error
	pkgDelay    time.Duration
	inFlight    int
	maxInFlight int
}

func quotaClient(name string, accounts ...core.AccountRecord) *fakeQuotaClient {
	return &fakeQuotaClient{
		fakeAccountClient: &fakeAccountClient{
			fakeClient: &fakeClient{name: name},
			accounts:   accounts,
		},
		balances:   map[string]core.Balance{},
		balErr:     map[string]error{},
		packages:   map[string]core.PackageReport{},
		pkgErr:     map[string]error{},
		vouchers:   map[string][]core.Voucher{},
		voucherErr: map[string]error{},
	}
}

func (f *fakeQuotaClient) AccountBalance(ctx context.Context, id string, soon time.Duration) (core.Balance, error) {
	f.mu.Lock()
	f.balID = append(f.balID, id)
	f.balSoon = append(f.balSoon, soon)
	err := f.balErr[id]
	bal := f.balances[id]
	f.mu.Unlock()
	if err != nil {
		return core.Balance{}, err
	}
	return bal, nil
}

func (f *fakeQuotaClient) AccountPackages(ctx context.Context, id string) (core.PackageReport, error) {
	f.mu.Lock()
	f.inFlight++
	if f.inFlight > f.maxInFlight {
		f.maxInFlight = f.inFlight
	}
	delay := f.pkgDelay
	f.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}

	f.mu.Lock()
	f.inFlight--
	err := f.pkgErr[id]
	rep := f.packages[id]
	f.mu.Unlock()
	if err != nil {
		return core.PackageReport{}, err
	}
	return rep, nil
}

func (f *fakeQuotaClient) AccountVouchers(ctx context.Context, id string) ([]core.Voucher, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.voucherErr[id]; err != nil {
		return nil, err
	}
	return f.vouchers[id], nil
}

func (f *fakeQuotaClient) seenSoon() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]time.Duration, len(f.balSoon))
	copy(out, f.balSoon)
	return out
}

func (f *fakeQuotaClient) peakConcurrency() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxInFlight
}

// fakeOnlyPackages implements PackageProvider but not AccountManager: a module
// that can report a breakdown but cannot enumerate its own accounts.
type fakeOnlyPackages struct {
	*fakeClient
}

func (f *fakeOnlyPackages) AccountPackages(context.Context, string) (core.PackageReport, error) {
	return core.PackageReport{}, nil
}

func TestAccountBalanceRelaysTheNumbers(t *testing.T) {
	c := quotaClient("wb", core.AccountRecord{ID: "uid-1"})
	c.balances["uid-1"] = core.Balance{
		Credits:           1200,
		Total:             5000,
		Expiring:          300,
		EarliestAt:        time.Date(2026, 9, 15, 18, 30, 0, 0, time.UTC),
		EarliestRemaining: 300,
	}
	h := New(Options{
		Registry:     registryOf(c),
		Started:      time.Now(),
		ExpiringSoon: 42 * time.Hour,
	})

	rec := post(t, h, "/panel/api/clients/wb/accounts/uid-1/balance")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := decodeMap(t, rec)
	if body["credits"] != float64(1200) || body["credits_total"] != float64(5000) {
		t.Errorf("credits = %v/%v, want 1200/5000", body["credits"], body["credits_total"])
	}
	if body["expiring"] != float64(300) {
		t.Errorf("expiring = %v, want 300", body["expiring"])
	}
	if got := body["earliest_at"]; got != "2026-09-15T18:30:00Z" {
		t.Errorf("earliest_at = %v, want RFC3339", got)
	}
	if _, has := body["accounts"]; !has {
		t.Errorf("the account list did not come back: %v", body)
	}
	// The operator's configured window has to reach the module: a module told
	// "0" would answer with no expiring bucket at all.
	if got := c.seenSoon(); len(got) != 1 || got[0] != 42*time.Hour {
		t.Fatalf("soon = %v, want exactly one call with 42h", got)
	}
}

func TestAccountBalanceReportsAVendorRefusalAsBadGateway(t *testing.T) {
	c := quotaClient("wb", core.AccountRecord{ID: "uid-1"})
	c.balErr["uid-1"] = errors.New("user resource: access_token=abcdef123456 rejected")
	h := New(Options{Registry: registryOf(c), Started: time.Now()})

	rec := post(t, h, "/panel/api/clients/wb/accounts/uid-1/balance")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", rec.Code, rec.Body.String())
	}
	msg := msgString(decodeMap(t, rec)["error"])
	if !contains(msg, "access_token=<redacted>") {
		t.Errorf("error = %q, want the credential redacted in place", msg)
	}
}

func TestAccountBalanceReportsAGoneAccountAsNotFound(t *testing.T) {
	c := quotaClient("wb", core.AccountRecord{ID: "other"})
	c.balErr["uid-9"] = errors.New("no such account")
	h := New(Options{Registry: registryOf(c), Started: time.Now()})

	rec := post(t, h, "/panel/api/clients/wb/accounts/uid-9/balance")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestAccountBalanceWithoutTheCapability(t *testing.T) {
	plain := &fakeAccountClient{fakeClient: &fakeClient{name: "kimi"}}
	h := New(Options{Registry: registryOf(plain), Started: time.Now()})

	rec := post(t, h, "/panel/api/clients/kimi/accounts/a1/balance")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: %s", rec.Code, rec.Body.String())
	}
}

func TestAccountBalanceRejectsTheWrongMethod(t *testing.T) {
	c := quotaClient("wb", core.AccountRecord{ID: "uid-1"})
	h := New(Options{Registry: registryOf(c), Started: time.Now()})

	rec := get(t, h, "/panel/api/clients/wb/accounts/uid-1/balance")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405: %s", rec.Code, rec.Body.String())
	}
	if got := c.seenSoon(); len(got) != 0 {
		t.Fatalf("a rejected method still asked the vendor: %v", got)
	}
}

func TestPackagesAggregatesAndSorts(t *testing.T) {
	c := quotaClient("wb",
		core.AccountRecord{ID: "small"},
		core.AccountRecord{ID: "big"},
		core.AccountRecord{ID: "broken"},
	)
	c.packages["small"] = core.PackageReport{Remain: 10, Size: 100, Packages: []core.CreditPackage{{Name: "signup", Remain: 10, Size: 100}}}
	c.packages["big"] = core.PackageReport{Remain: 30, Size: 100}
	c.pkgErr["broken"] = errors.New("billing meter: access_token=abcdef123456 denied")
	h := New(Options{Registry: registryOf(c), Started: time.Now()})

	rec := get(t, h, "/panel/api/clients/wb/packages")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rows := rowsOf(t, decodeMap(t, rec)["accounts"])
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	// Biggest balance first, the failed row last at zero.
	if rows[0]["id"] != "big" || rows[1]["id"] != "small" || rows[2]["id"] != "broken" {
		t.Fatalf("order = %v/%v/%v, want big/small/broken", rows[0]["id"], rows[1]["id"], rows[2]["id"])
	}
	if rows[0]["remain"] != float64(30) {
		t.Errorf("big.remain = %v, want 30", rows[0]["remain"])
	}
	// An account that failed is a row-level fact, not a failed request.
	if msg := msgString(rows[2]["error"]); !contains(msg, "access_token=<redacted>") {
		t.Errorf("broken.error = %q, want a redacted message", msg)
	}
	if _, has := rows[0]["error"]; has {
		t.Errorf("a healthy row carried an error: %v", rows[0])
	}
	// An empty breakdown must serialise as [] rather than null, or the browser
	// has to special-case it.
	if pk, ok := rows[0]["packages"].([]any); !ok || pk == nil || len(pk) != 0 {
		t.Errorf("big.packages = %#v, want an empty array", rows[0]["packages"])
	}
	// The real package object survives the trip.
	pk, _ := rows[1]["packages"].([]any)
	if len(pk) != 1 {
		t.Fatalf("small.packages = %#v, want one entry", rows[1]["packages"])
	}
	if item, _ := pk[0].(map[string]any); item["name"] != "signup" || item["remain"] != float64(10) {
		t.Errorf("small package = %#v, want the module's values", pk[0])
	}
}

func TestPackagesBoundsTheFanOut(t *testing.T) {
	// The reference caps this at 3 concurrent vendor calls; a fleet refresh must
	// not turn into a burst.  Six accounts is enough to see a bound of 3.
	var recs []core.AccountRecord
	for i := 0; i < 6; i++ {
		recs = append(recs, core.AccountRecord{ID: fmt.Sprintf("a%d", i)})
	}
	c := quotaClient("wb", recs...)
	c.pkgDelay = 20 * time.Millisecond
	h := New(Options{Registry: registryOf(c), Started: time.Now()})

	rec := get(t, h, "/panel/api/clients/wb/packages")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if peak := c.peakConcurrency(); peak > packageFanout {
		t.Fatalf("peak concurrency = %d, want at most %d", peak, packageFanout)
	} else if peak < 2 {
		t.Fatalf("peak concurrency = %d, want the accounts queried in parallel", peak)
	}
}

func TestPackagesWithoutTheCapability(t *testing.T) {
	plain := &fakeAccountClient{fakeClient: &fakeClient{name: "kimi"}}
	h := New(Options{Registry: registryOf(plain), Started: time.Now()})

	rec := get(t, h, "/panel/api/clients/kimi/packages")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: %s", rec.Code, rec.Body.String())
	}

	// A provider that cannot enumerate accounts has nothing to iterate; both
	// halves are required, and the answer must name the missing half.
	only := &fakeOnlyPackages{fakeClient: &fakeClient{name: "zcode"}}
	h2 := New(Options{Registry: registryOf(only), Started: time.Now()})
	rec2 := get(t, h2, "/panel/api/clients/zcode/packages")
	if rec2.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: %s", rec2.Code, rec2.Body.String())
	}
	if msg := msgString(decodeMap(t, rec2)["error"]); !contains(msg, "cannot list accounts") {
		t.Errorf("error = %q, want it to name the missing half", msg)
	}
}

func TestPackagesRejectsTheWrongMethod(t *testing.T) {
	c := quotaClient("wb", core.AccountRecord{ID: "a1"})
	h := New(Options{Registry: registryOf(c), Started: time.Now()})

	rec := post(t, h, "/panel/api/clients/wb/packages")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405: %s", rec.Code, rec.Body.String())
	}
	if peak := c.peakConcurrency(); peak != 0 {
		t.Fatalf("a rejected method still queried the vendor: peak %d", peak)
	}
}

func TestSchoolVouchersListsCodes(t *testing.T) {
	c := quotaClient("wb", core.AccountRecord{ID: "a1"}, core.AccountRecord{ID: "a2"})
	c.vouchers["a1"] = []core.Voucher{{GrantID: 7, PrizeName: "咖啡券", Code: "secret_123456"}}
	h := New(Options{Registry: registryOf(c), Started: time.Now()})

	rec := get(t, h, "/panel/api/clients/wb/school/vouchers")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := decodeMap(t, rec)
	if body["count"] != float64(1) {
		t.Errorf("count = %v, want 1", body["count"])
	}
	rows := rowsOf(t, body["accounts"])
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want one per account", len(rows))
	}
	vs, _ := rows[0]["vouchers"].([]any)
	if len(vs) != 1 {
		t.Fatalf("a1 vouchers = %#v, want one", rows[0]["vouchers"])
	}
	v, _ := vs[0].(map[string]any)
	// The code is the point of the view, so it must arrive verbatim: redaction
	// here would hand the operator "secret=<redacted>" instead of a redeemable
	// code.
	if v["code"] != "secret_123456" {
		t.Errorf("code = %v, want the code as the module reported it", v["code"])
	}
	// An account with nothing to show still gets a row with an empty array.
	if list, ok := rows[1]["vouchers"].([]any); !ok || list == nil || len(list) != 0 {
		t.Errorf("a2 vouchers = %#v, want an empty array", rows[1]["vouchers"])
	}
}

func TestSchoolVouchersReportsARowError(t *testing.T) {
	c := quotaClient("wb", core.AccountRecord{ID: "a1"}, core.AccountRecord{ID: "a2"})
	c.voucherErr["a1"] = errors.New("school portal: access_token=abcdef123456 rejected")
	c.vouchers["a2"] = []core.Voucher{{Code: "KFC-ABC"}}
	h := New(Options{Registry: registryOf(c), Started: time.Now()})

	rec := get(t, h, "/panel/api/clients/wb/school/vouchers")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even with one failure: %s", rec.Code, rec.Body.String())
	}
	rows := rowsOf(t, decodeMap(t, rec)["accounts"])
	if msg := msgString(rows[0]["error"]); !contains(msg, "access_token=<redacted>") {
		t.Errorf("a1.error = %q, want a redacted message", msg)
	}
	vs, _ := rows[1]["vouchers"].([]any)
	if len(vs) != 1 {
		t.Errorf("the account that answered lost its vouchers: %#v", rows[1])
	}
}

func TestSchoolVouchersWithoutTheCapability(t *testing.T) {
	plain := &fakeAccountClient{fakeClient: &fakeClient{name: "trae"}}
	h := New(Options{Registry: registryOf(plain), Started: time.Now()})

	rec := get(t, h, "/panel/api/clients/trae/school/vouchers")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: %s", rec.Code, rec.Body.String())
	}
}

func TestCapabilitiesReportQuota(t *testing.T) {
	ctx := context.Background()
	caps := core.CapabilitiesOf(ctx, quotaClient("wb"))
	if !caps.Balance || !caps.Packages || !caps.Vouchers {
		t.Errorf("a full quota client reported %+v, want all three flags", caps)
	}
	plain := core.CapabilitiesOf(ctx, &fakeAccountClient{fakeClient: &fakeClient{name: "kimi"}})
	if plain.Balance || plain.Packages || plain.Vouchers {
		t.Errorf("a module with no quota surface reported %+v", plain)
	}
	if !plain.Manage {
		t.Error("the fixture stopped being an account manager; the check above proves nothing")
	}
}

// ---------------------------------------------------------------------------
// the fleet-wide balance column
// ---------------------------------------------------------------------------

func TestBalancesReturnsOneRowPerAccountInListOrder(t *testing.T) {
	// The accounts page renders this beside the account list, so a reordering
	// used is the vendor's own consumption figure, not inferred from credits.
	// here would make the 余额 column belong to the wrong row.
	c := quotaClient("wb",
		core.AccountRecord{ID: "small", Label: "小号"},
		core.AccountRecord{ID: "big"},
		core.AccountRecord{ID: "broken"},
	)
	c.balances["small"] = core.Balance{Credits: 10, Total: 100, Unit: "积分"}
	c.balances["big"] = core.Balance{
		Credits:           30,
		Total:             100,
		Expiring:          5,
		EarliestAt:        time.Date(2026, 9, 15, 18, 30, 0, 0, time.UTC),
		EarliestRemaining: 5,
		Unit:              "积分",
	}
	c.balErr["broken"] = errors.New("user resource: access_token=abcdef123456 rejected")
	h := New(Options{Registry: registryOf(c), Started: time.Now(), ExpiringSoon: 42 * time.Hour})

	rec := get(t, h, "/panel/api/clients/wb/balances")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := decodeMap(t, rec)
	rows := rowsOf(t, body["accounts"])
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want one per account", len(rows))
	}
	if rows[0]["id"] != "small" || rows[1]["id"] != "big" || rows[2]["id"] != "broken" {
		t.Fatalf("order = %v/%v/%v, want the account list order", rows[0]["id"], rows[1]["id"], rows[2]["id"])
	}
	if rows[0]["label"] != "小号" {
		t.Errorf("label = %v, want the record's own label", rows[0]["label"])
	}
	if rows[0]["credits"] != float64(10) || rows[0]["credits_total"] != float64(100) {
		t.Errorf("small = %v/%v, want 10/100", rows[0]["credits"], rows[0]["credits_total"])
	}
	if rows[1]["earliest_at"] != "2026-09-15T18:30:00Z" || rows[1]["earliest_remaining"] != float64(5) {
		t.Errorf("big expiry = %v/%v, want the module's tranche", rows[1]["earliest_at"], rows[1]["earliest_remaining"])
	}
	// One account failing is a row-level fact, never a failed request, and the
	// credential in the vendor's own words must be redacted in place.
	if msg := msgString(rows[2]["error"]); !contains(msg, "access_token=<redacted>") {
		t.Errorf("broken.error = %q, want a redacted message", msg)
	}
	if _, has := rows[0]["error"]; has {
		t.Errorf("a healthy row carried an error: %v", rows[0])
	}
	// The unit is a property of the module, not of a row, so it is hoisted:
	// the column header has to be able to say 积分 once rather than per row.
	if body["unit"] != "积分" {
		t.Errorf("unit = %v, want the module's own label", body["unit"])
	}
	// The operator's window has to reach the module for every account.
	soon := c.seenSoon()
	if len(soon) != 3 {
		t.Fatalf("soon = %v, want one call per account", soon)
	}
	for _, d := range soon {
		if d != 42*time.Hour {
			t.Fatalf("soon = %v, want 42h for every account", soon)
		}
	}
}

func TestBalancesOmitsTheUnitWhenTheModuleNamesNone(t *testing.T) {
	// zcode reports tokens and tabbit reports a percentage; a module that says
	// nothing must get a bare number rather than a label the panel guessed.
	c := quotaClient("wb", core.AccountRecord{ID: "a1"})
	c.balances["a1"] = core.Balance{Credits: 7, Total: 10}
	h := New(Options{Registry: registryOf(c), Started: time.Now()})

	body := decodeMap(t, get(t, h, "/panel/api/clients/wb/balances"))
	if _, has := body["unit"]; has {
		t.Errorf("unit = %v, want the key absent when the module named none", body["unit"])
	}
}

func TestBalancesReportsTheUnitEvenWhenTheFirstRowFailed(t *testing.T) {
	c := quotaClient("wb", core.AccountRecord{ID: "broken"}, core.AccountRecord{ID: "ok"})
	c.balErr["broken"] = errors.New("boom")
	c.balances["ok"] = core.Balance{Credits: 1, Total: 2, Unit: "tokens"}
	h := New(Options{Registry: registryOf(c), Started: time.Now()})

	body := decodeMap(t, get(t, h, "/panel/api/clients/wb/balances"))
	if body["unit"] != "tokens" {
		t.Errorf("unit = %v, want the label from the row that answered", body["unit"])
	}
}

func TestBalancesWithoutTheCapability(t *testing.T) {
	plain := &fakeAccountClient{fakeClient: &fakeClient{name: "kimi"}}
	h := New(Options{Registry: registryOf(plain), Started: time.Now()})

	rec := get(t, h, "/panel/api/clients/kimi/balances")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: %s", rec.Code, rec.Body.String())
	}

	// A provider that cannot enumerate accounts has nothing to iterate; the
	// answer must name the missing half.
	only := &fakeOnlyBalances{fakeClient: &fakeClient{name: "zcode"}}
	h2 := New(Options{Registry: registryOf(only), Started: time.Now()})
	rec2 := get(t, h2, "/panel/api/clients/zcode/balances")
	if rec2.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: %s", rec2.Code, rec2.Body.String())
	}
	if msg := msgString(decodeMap(t, rec2)["error"]); !contains(msg, "cannot list accounts") {
		t.Errorf("error = %q, want it to name the missing half", msg)
	}
}

func TestBalancesRejectsTheWrongMethod(t *testing.T) {
	// GET here, POST on the per-account route: a browser or proxy must not be
	// able to replay the explicit "ask the vendor now" button by accident.
	c := quotaClient("wb", core.AccountRecord{ID: "a1"})
	h := New(Options{Registry: registryOf(c), Started: time.Now()})

	rec := post(t, h, "/panel/api/clients/wb/balances")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405: %s", rec.Code, rec.Body.String())
	}
	if got := c.seenSoon(); len(got) != 0 {
		t.Fatalf("a rejected method still asked the vendor: %v", got)
	}
}

// fakeOnlyBalances implements BalanceProvider but not AccountManager.
type fakeOnlyBalances struct {
	*fakeClient
}

func (f *fakeOnlyBalances) AccountBalance(context.Context, string, time.Duration) (core.Balance, error) {
	return core.Balance{}, nil
}

// contains and rowsOf keep the assertions readable without pulling in more
// helpers from the other test files.
func contains(s, sub string) bool { return strings.Contains(s, sub) }

func rowsOf(t *testing.T, v any) []map[string]any {
	t.Helper()
	raw, _ := v.([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("row is not an object: %#v", item)
		}
		out = append(out, m)
	}
	return out
}
