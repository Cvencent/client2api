package panel

import (
	"errors"
	"net/http"
	"sort"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// The wire shapes the shell reads by name.
//
// Nothing else in this package pins the key NAMES: the tests beside this file
// assert values through the same structs the handlers write, so renaming a json
// tag (or dropping one) keeps them green while the browser silently renders
// "—" everywhere.  These tests assert the key set of a real response instead,
// which is the only thing that fails when the shape moves.
// ---------------------------------------------------------------------------

func assertKeySet(t *testing.T, where string, m map[string]any, want ...string) {
	t.Helper()
	got := keysOf(m)
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("%s keys = %v, want %v", where, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s keys = %v, want %v", where, got, want)
		}
	}
}

func TestBalanceWireShape(t *testing.T) {
	full := quotaClient("wb", core.AccountRecord{ID: "a1", Label: "一号"})
	full.balances["a1"] = core.Balance{
		Credits:           1200,
		Used:              17.5,
		Total:             5000,
		Expiring:          300,
		EarliestAt:        time.Date(2026, 9, 15, 18, 30, 0, 0, time.UTC),
		EarliestRemaining: 120,
	}
	p := taskPanel(t, full)
	_, out := doTask(t, p, http.MethodPost, "/panel/api/clients/wb/accounts/a1/balance", "")

	// The reference answers ok/credits/credits_total; expiring/earliest_* are
	// our additive buckets.  All three are read by the shell's toast.
	assertKeySet(t, "balance", out,
		"ok", "credits", "credits_total", "used", "expiring", "earliest_at", "earliest_remaining", "accounts")
	numOf(t, out, "credits")
	numOf(t, out, "credits_total")
	numOf(t, out, "used")
	numOf(t, out, "expiring")
	numOf(t, out, "earliest_remaining")
	// earliest_at is an RFC3339 STRING, and it is the only reason the shell
	// looks at earliest_remaining at all.
	if _, ok := out["earliest_at"].(string); !ok {
		t.Fatalf("earliest_at = %#v, want an RFC3339 string", out["earliest_at"])
	}

	// The minimal answer: a module that reports neither bucket must not send
	// keys the shell would render as an empty "expiring" line.
	bare := quotaClient("kimi", core.AccountRecord{ID: "a1"})
	bare.balances["a1"] = core.Balance{Credits: 1, Total: 2}
	_, min := doTask(t, taskPanel(t, bare), http.MethodPost, "/panel/api/clients/kimi/accounts/a1/balance", "")
	assertKeySet(t, "minimal balance", min, "ok", "credits", "credits_total", "accounts")
}

func TestBalanceWireShapeReportsUnlimited(t *testing.T) {
	c := quotaClient("trae", core.AccountRecord{ID: "a1"})
	c.balances["a1"] = core.Balance{Unlimited: true, Used: 123.25, Unit: "积分"}
	_, out := doTask(t, taskPanel(t, c), http.MethodPost, "/panel/api/clients/trae/accounts/a1/balance", "")
	if out["unlimited"] != true {
		t.Fatalf("unlimited = %#v, want true", out["unlimited"])
	}
}

func TestBalanceWireShapeReportsUnverified(t *testing.T) {
	c := quotaClient("workbuddy", core.AccountRecord{ID: "a1"})
	c.balances["a1"] = core.Balance{Credits: 0, Total: 500, Unverified: true}
	p := taskPanel(t, c)
	_, out := doTask(t, p, http.MethodPost, "/panel/api/clients/workbuddy/accounts/a1/balance", "")
	if out["unverified"] != true {
		t.Fatalf("per-account unverified = %#v, want true", out["unverified"])
	}

	p.initBalanceCache()
	seedBalanceCache(p, c.Name(), "a1", c.balances["a1"])
	_, list := doTask(t, p, http.MethodGet, "/panel/api/clients/workbuddy/balances", "")
	row := rowsOf(t, list["accounts"])[0]
	if row["unverified"] != true {
		t.Fatalf("balance-row unverified = %#v, want true", row["unverified"])
	}
}

func TestPackagesWireShape(t *testing.T) {
	c := quotaClient("wb", core.AccountRecord{ID: "a1", Label: "一号", State: "ready"})
	c.packages["a1"] = core.PackageReport{
		Remain: 300,
		Size:   500,
		Packages: []core.CreditPackage{{
			Name:           "签到赠送",
			Remain:         300,
			Used:           200,
			Size:           500,
			EndTime:        "2026-09-30",
			ExpiresAt:      1790000000,
			CreatedAt:      "2026-08-01 00:00:00",
			PackageCode:    "pkg-1",
			SubProductCode: "sub-1",
			SubProductName: "会员月度包",
			Cycle:          true,
		}},
	}
	_, out := doTask(t, taskPanel(t, c), http.MethodGet, "/panel/api/clients/wb/packages", "")

	assertKeySet(t, "packages body", out, "accounts")
	rows := rowsOf(t, out["accounts"])
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	// error is omitempty: a row that answered must NOT carry it, or the shell
	// would paint a failure chip on a healthy account.
	assertKeySet(t, "package row", rows[0], "id", "label", "state", "remain", "size", "packages")

	// The mirror image: the account that failed is a row carrying error, and
	// the other accounts are untouched by it.
	broken := quotaClient("trae", core.AccountRecord{ID: "a1", Label: "一号", State: "ready"})
	broken.pkgErr["a1"] = errors.New("vendor refused")
	_, outErr := doTask(t, taskPanel(t, broken), http.MethodGet, "/panel/api/clients/trae/packages", "")
	assertKeySet(t, "package error row", rowsOf(t, outErr["accounts"])[0],
		"id", "label", "state", "remain", "size", "packages", "error")

	pks, ok := rows[0]["packages"].([]any)
	if !ok || len(pks) != 1 {
		t.Fatalf("packages = %#v, want one entry", rows[0]["packages"])
	}
	pkg, ok := pks[0].(map[string]any)
	if !ok {
		t.Fatalf("package = %#v, want an object", pks[0])
	}
	assertKeySet(t, "package", pkg,
		"name", "remain", "used", "size", "end_time", "expires_at", "created_at",
		"package_code", "sub_product_code", "sub_product_name", "cycle")

	// expires_at is a NUMBER: the shell runs it through Number().  A string
	// here would render "NaN" in the expiry column, so pin the type rather
	// than trusting the tag.
	numOf(t, pkg, "expires_at")
	if _, ok := pkg["end_time"].(string); !ok {
		t.Errorf("end_time = %#v, want a string", pkg["end_time"])
	}
	boolOf(t, pkg, "cycle")

	// A tranche that carries none of the optional detail must come back with
	// exactly the four fields the shell always reads -- not a null, not an
	// empty string per field.
	sparse := quotaClient("kimi", core.AccountRecord{ID: "a1"})
	sparse.packages["a1"] = core.PackageReport{Packages: []core.CreditPackage{{Name: "礼包", Remain: 1, Used: 2, Size: 3}}}
	_, out2 := doTask(t, taskPanel(t, sparse), http.MethodGet, "/panel/api/clients/kimi/packages", "")
	pk2 := rowsOf(t, out2["accounts"])[0]["packages"].([]any)[0].(map[string]any)
	assertKeySet(t, "sparse package", pk2, "name", "remain", "used", "size")
}

func TestVouchersWireShape(t *testing.T) {
	c := quotaClient("wb", core.AccountRecord{ID: "a1", Label: "一号"})
	c.vouchers["a1"] = []core.Voucher{{
		GrantID:   7,
		DrawUUID:  "draw-1",
		SKUCode:   "sku-1",
		PrizeName: "会员体验券",
		Code:      "secret_123456",
		ValidFrom: "2026-08-01",
		ValidTo:   "2026-09-01",
		GrantedAt: "2026-08-01 10:00:00",
	}}
	_, out := doTask(t, taskPanel(t, c), http.MethodGet, "/panel/api/clients/wb/school/vouchers", "")

	assertKeySet(t, "vouchers body", out, "accounts", "count")
	rows := rowsOf(t, out["accounts"])
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	assertKeySet(t, "voucher row", rows[0], "id", "label", "vouchers")

	broken := quotaClient("trae", core.AccountRecord{ID: "a1", Label: "一号"})
	broken.voucherErr["a1"] = errors.New("vendor refused")
	_, outErr := doTask(t, taskPanel(t, broken), http.MethodGet, "/panel/api/clients/trae/school/vouchers", "")
	assertKeySet(t, "voucher error row", rowsOf(t, outErr["accounts"])[0], "id", "label", "vouchers", "error")

	vs, ok := rows[0]["vouchers"].([]any)
	if !ok || len(vs) != 1 {
		t.Fatalf("vouchers = %#v, want one entry", rows[0]["vouchers"])
	}
	v, ok := vs[0].(map[string]any)
	if !ok {
		t.Fatalf("voucher = %#v, want an object", vs[0])
	}
	assertKeySet(t, "voucher", v,
		"grant_id", "draw_uuid", "sku_code", "prize_name", "code", "valid_from", "valid_to", "granted_at")
	numOf(t, v, "grant_id")
	// The code IS the payload of this view: it reaches the browser verbatim so
	// the copy button copies something redeemable.
	if v["code"] != "secret_123456" {
		t.Errorf("code = %#v, want the code verbatim", v["code"])
	}
}

func TestAutoVerbWireShape(t *testing.T) {
	c := verbClient("wb", []core.TaskInfo{{Code: "t1", Desc: "每日签到", Auto: true}},
		core.AccountRecord{ID: "a1", Label: "一号"})
	c.auto["t1"] = core.AutoTaskResult{
		TaskResult:     core.TaskResult{OK: true, Code: "t1", Credit: 7, Energy: 1, Message: "签到完成"},
		ProgressBefore: "2/5",
		ProgressAfter:  "5/5",
		Claimable:      true,
		Attempt:        true,
		Claimed:        true,
	}
	_, out := doTask(t, taskPanel(t, c), http.MethodPost, verbPath("wb", "a1", "auto"), `{"task_code":"t1"}`)

	// verify_supported and claimable are always present, because the shell
	// branches on them before it renders a single field.
	assertKeySet(t, "auto", out,
		"ok", "task_code", "verify_supported", "attempt", "claimable",
		"progress_before", "progress_after", "message", "claimed", "credit", "energy")
	if !boolOf(t, out, "verify_supported") {
		t.Error("verify_supported = false; the panel always reads the progress back")
	}
	numOf(t, out, "credit")
	numOf(t, out, "energy")

	// A run that skipped the chore still answers 200 with the message the
	// shell shows next to it, and no money fields.
	c.auto["t1"] = core.AutoTaskResult{TaskResult: core.TaskResult{OK: true, Code: "t1"}, Skipped: true}
	_, skipped := doTask(t, taskPanel(t, c), http.MethodPost, verbPath("wb", "a1", "auto"), `{"task_code":"t1"}`)
	assertKeySet(t, "skipped auto", skipped, "ok", "task_code", "verify_supported", "attempt", "claimable", "skipped", "message")
	if !boolOf(t, skipped, "skipped") {
		t.Error("skipped = false, want the skip flag the shell renders")
	}
}
