package scheduler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Fakes.  No test may sleep on a real clock: every wait goes through
// fakeClock.Sleep, which advances a virtual "now" instead.
// ---------------------------------------------------------------------------

// recorder is the shared, mutex-guarded event log: batch codes and waits land
// in it in the order they happened, which is what the pacing tests assert on.
type recorder struct {
	mu   sync.Mutex
	ev   []string
	logs []string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	r.ev = append(r.ev, s)
	r.mu.Unlock()
}

func (r *recorder) log(s string) {
	r.mu.Lock()
	r.logs = append(r.logs, s)
	r.mu.Unlock()
}

func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ev...)
}

func (r *recorder) lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.logs...)
}

type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	sleeps  []time.Duration
	onSleep func() bool // false simulates a dead context
	rec     *recorder
	logs    *recorder
}

func newFakeClock(now time.Time, logs *recorder) *fakeClock {
	return &fakeClock{now: now, rec: logs, logs: logs}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) bool {
	c.mu.Lock()
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	hook := c.onSleep
	c.mu.Unlock()
	if c.rec != nil {
		c.rec.add("sleep")
	}
	if hook != nil && !hook() {
		return false
	}
	return ctx.Err() == nil
}

func (c *fakeClock) durations() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.sleeps...)
}

type fakeClient struct {
	name        string
	batches     []core.Batch
	accounts    []core.AccountRecord
	accountsErr error
	tasks       map[string][]core.TaskInfo
	tasksErr    error
	run         func(ctx context.Context, accountID, code string) (core.TaskResult, error)
	rec         *recorder
}

func (f *fakeClient) Name() string { return f.name }

func (f *fakeClient) Models(ctx context.Context) ([]core.Model, error) { return nil, nil }

func (f *fakeClient) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	return nil, core.ErrUnsupported
}

func (f *fakeClient) Status(ctx context.Context) core.Status {
	return core.Status{Name: f.name, Ready: true, UpdatedAt: time.Now()}
}

func (f *fakeClient) Batches() []core.Batch { return f.batches }

// fakeClient implements the whole optional core.AccountManager surface: the
// scheduler narrows with core.AsAccountManager, so implementing only part of
// it would silently look like "this client has no accounts at all".

func (f *fakeClient) AccountFields(ctx context.Context) []core.FieldSpec { return nil }

func (f *fakeClient) Accounts(ctx context.Context) ([]core.AccountRecord, error) {
	return f.accounts, f.accountsErr
}

func (f *fakeClient) AddAccount(ctx context.Context, spec core.AccountSpec) (core.AccountRecord, error) {
	return core.AccountRecord{}, core.ErrUnsupported
}

func (f *fakeClient) RemoveAccount(ctx context.Context, id string) error { return core.ErrUnsupported }

func (f *fakeClient) SetAccountEnabled(ctx context.Context, id string, enabled bool) error {
	return core.ErrUnsupported
}

func (f *fakeClient) TestAccount(ctx context.Context, id string) (core.TestResult, error) {
	return core.TestResult{}, core.ErrUnsupported
}

func (f *fakeClient) RefreshAccount(ctx context.Context, id string) ([]core.RefreshResult, error) {
	return nil, core.ErrUnsupported
}

func (f *fakeClient) Tasks(ctx context.Context, accountID string) ([]core.TaskInfo, error) {
	if f.tasksErr != nil {
		return nil, f.tasksErr
	}
	return f.tasks[accountID], nil
}

func (f *fakeClient) RunTask(ctx context.Context, accountID, code string) (core.TaskResult, error) {
	f.rec.add(accountID + "/" + code)
	if f.run != nil {
		return f.run(ctx, accountID, code)
	}
	return core.TaskResult{OK: true, Code: code, AccountID: accountID}, nil
}

// batchOf finds one of the fake's own batches, for building a report.
func (f *fakeClient) batchOf(name string) core.Batch {
	b, _ := core.BatchOf(f, name)
	return b
}

// plainClient has no capabilities at all: it must never be scheduled.
type plainClient struct{ name string }

func (p plainClient) Name() string { return p.name }

func (p plainClient) Models(ctx context.Context) ([]core.Model, error) { return nil, nil }

func (p plainClient) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	return nil, core.ErrUnsupported
}

func (p plainClient) Status(ctx context.Context) core.Status {
	return core.Status{Name: p.name}
}

// plannerOnly plans batches but cannot run a task, so it has nothing the
// scheduler can execute.
type plannerOnly struct{ name string }

func (p plannerOnly) Name() string { return p.name }

func (p plannerOnly) Models(ctx context.Context) ([]core.Model, error) { return nil, nil }

func (p plannerOnly) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	return nil, core.ErrUnsupported
}

func (p plannerOnly) Status(ctx context.Context) core.Status {
	return core.Status{Name: p.name}
}

func (p plannerOnly) Batches() []core.Batch {
	return []core.Batch{{Name: batchCheckin, Codes: []string{"orphan"}}}
}

// checkinOnly can check in for its accounts but has no BatchPlanner and no
// TaskProvider: the shared bridge must still expose one synthetic check-in
// batch and run it through CheckinProvider.
type checkinOnly struct {
	name     string
	accounts []core.AccountRecord
	rec      *recorder
}

func (c *checkinOnly) Name() string { return c.name }

func (c *checkinOnly) Models(ctx context.Context) ([]core.Model, error) { return nil, nil }

func (c *checkinOnly) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	return nil, core.ErrUnsupported
}

func (c *checkinOnly) Status(ctx context.Context) core.Status {
	return core.Status{Name: c.name, Ready: true, UpdatedAt: time.Now()}
}

func (c *checkinOnly) AccountFields(ctx context.Context) []core.FieldSpec { return nil }

func (c *checkinOnly) Accounts(ctx context.Context) ([]core.AccountRecord, error) {
	return c.accounts, nil
}

func (c *checkinOnly) AddAccount(ctx context.Context, spec core.AccountSpec) (core.AccountRecord, error) {
	return core.AccountRecord{}, core.ErrUnsupported
}

func (c *checkinOnly) RemoveAccount(ctx context.Context, id string) error {
	return core.ErrUnsupported
}

func (c *checkinOnly) SetAccountEnabled(ctx context.Context, id string, enabled bool) error {
	return core.ErrUnsupported
}

func (c *checkinOnly) TestAccount(ctx context.Context, id string) (core.TestResult, error) {
	return core.TestResult{}, core.ErrUnsupported
}

func (c *checkinOnly) RefreshAccount(ctx context.Context, id string) ([]core.RefreshResult, error) {
	return nil, core.ErrUnsupported
}

func (c *checkinOnly) CheckinActions(ctx context.Context) []core.CheckinAction {
	return []core.CheckinAction{{ID: "daily", Label: "每日签到"}}
}

func (c *checkinOnly) Checkin(ctx context.Context, id, action string) (core.CheckinResult, error) {
	if c.rec != nil {
		c.rec.add("checkin:" + id + "/" + action)
	}
	return core.CheckinResult{OK: true, AccountID: id, Action: action}, nil
}

type scopedCheckinOnly struct {
	*checkinOnly
	actions []core.CheckinAction
}

func (c *scopedCheckinOnly) CheckinActions(context.Context) []core.CheckinAction {
	return c.actions
}

type actionScopedClient struct {
	*fakeClient
	actions []core.CheckinAction
}

func (c *actionScopedClient) CheckinActions(context.Context) []core.CheckinAction {
	return c.actions
}

func (c *actionScopedClient) Checkin(_ context.Context, id, action string) (core.CheckinResult, error) {
	return core.CheckinResult{OK: true, AccountID: id, Action: action}, nil
}

func registryOf(cs ...core.Client) *core.Registry {
	reg := core.NewRegistry()
	for _, c := range cs {
		reg.Add(c)
	}
	return reg
}

func deps(reg *core.Registry, clk *fakeClock, logs *recorder) Deps {
	return Deps{
		Registry: reg,
		Logf:     func(format string, args ...any) { logs.log(format) },
		Now:      clk.Now,
		Sleep:    clk.Sleep,
	}
}

func oneBatch(name string, codes ...string) core.Batch {
	return core.Batch{Name: name, Codes: codes, Settle: time.Millisecond}
}

var cstMidnight = time.Date(2026, 3, 4, 0, 0, 0, 0, CST)

// ---------------------------------------------------------------------------
// NextFire
// ---------------------------------------------------------------------------

func TestNextFireBeforeTodayHour(t *testing.T) {
	now := time.Date(2026, 3, 4, 7, 30, 0, 0, CST)
	got := NextFire(now, []int{9}, CST)
	want := time.Date(2026, 3, 4, 9, 0, 0, 0, CST)
	if !got.Equal(want) {
		t.Fatalf("NextFire = %s, want %s", got, want)
	}
	if got.Location() != CST {
		t.Fatalf("NextFire location = %v, want CST", got.Location())
	}
}

func TestNextFireAfterTodayHour(t *testing.T) {
	now := time.Date(2026, 3, 4, 10, 30, 0, 0, CST)
	got := NextFire(now, []int{9}, CST)
	want := time.Date(2026, 3, 5, 9, 0, 0, 0, CST)
	if !got.Equal(want) {
		t.Fatalf("NextFire = %s, want %s", got, want)
	}
}

func TestNextFireMidnightBoundary(t *testing.T) {
	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{
			name: "before midnight",
			now:  time.Date(2026, 3, 4, 23, 30, 0, 0, CST),
			want: time.Date(2026, 3, 5, 0, 0, 0, 0, CST),
		},
		{
			name: "exactly at the fire instant is not after it",
			now:  cstMidnight,
			want: time.Date(2026, 3, 5, 0, 0, 0, 0, CST),
		},
		{
			name: "year rollover",
			now:  time.Date(2026, 12, 31, 23, 0, 0, 0, CST),
			want: time.Date(2027, 1, 1, 0, 0, 0, 0, CST),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NextFire(tc.now, []int{0}, CST)
			if !got.Equal(tc.want) {
				t.Fatalf("NextFire = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestNextFireIgnoresHostLocation(t *testing.T) {
	instant := time.Date(2026, 3, 4, 10, 30, 0, 0, CST)
	zones := []*time.Location{
		CST,
		time.UTC,
		time.FixedZone("UTC-5", -5*60*60),
		time.FixedZone("UTC+5:30", 5*60*60+30*60),
	}
	want := time.Date(2026, 3, 5, 9, 0, 0, 0, CST)
	for _, zone := range zones {
		got := NextFire(instant.In(zone), []int{9}, CST)
		if !got.Equal(want) {
			t.Fatalf("NextFire from %v = %s, want %s", zone, got, want)
		}
		if got.Location() != CST {
			t.Fatalf("NextFire from %v location = %v, want CST", zone, got.Location())
		}
	}
	// The same instant handed over already expressed in another zone must not
	// shift the answer either.
	alt := NextFire(time.Date(2026, 3, 4, 5, 30, 0, 0, time.UTC), []int{9}, CST)
	if !alt.Equal(want) {
		t.Fatalf("NextFire from UTC wall clock = %s, want %s", alt, want)
	}
}

func TestNextFireEmptyHoursNeverFires(t *testing.T) {
	if got := NextFire(cstMidnight, nil, CST); !got.IsZero() {
		t.Fatalf("NextFire(nil) = %s, want zero time", got)
	}
	if got := NextFire(cstMidnight, []int{}, CST); !got.IsZero() {
		t.Fatalf("NextFire(empty) = %s, want zero time", got)
	}
}

func TestNextFireNormalizesItsHours(t *testing.T) {
	// Unsorted, duplicated and out-of-range hours must behave exactly like
	// their normalized form, and must not mutate the caller's slice.
	now := time.Date(2026, 3, 4, 7, 0, 0, 0, CST)
	raw := []int{21, 9, 99, 9, -3}
	got := NextFire(now, raw, CST)
	want := time.Date(2026, 3, 4, 9, 0, 0, 0, CST)
	if !got.Equal(want) {
		t.Fatalf("NextFire = %s, want %s", got, want)
	}
	if !reflect.DeepEqual(raw, []int{21, 9, 99, 9, -3}) {
		t.Fatalf("NextFire mutated its input: %v", raw)
	}
}

// ---------------------------------------------------------------------------
// Validate
// ---------------------------------------------------------------------------

func TestValidateNormalizes(t *testing.T) {
	cfg := Config{
		Checkin: Group{Enabled: true, Hours: []int{23, 5, 5, 30, -1, 23}},
		Travel:  Group{Enabled: true, Hours: []int{7, 7, 21, 6}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate = nil, want an out-of-range error")
	}
	if !strings.Contains(err.Error(), batchCheckin) {
		t.Fatalf("error %q does not name the offending batch %q", err, batchCheckin)
	}
	if want := []int{5, 23}; !reflect.DeepEqual(cfg.Checkin.Hours, want) {
		t.Fatalf("checkin hours = %v, want %v", cfg.Checkin.Hours, want)
	}
	if want := []int{6, 7, 21}; !reflect.DeepEqual(cfg.Travel.Hours, want) {
		t.Fatalf("travel hours = %v, want %v", cfg.Travel.Hours, want)
	}
}

func TestValidateRejectsOutOfRange(t *testing.T) {
	cfg := Config{Activity: Group{Enabled: true, Hours: []int{24}}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate = nil, want an error for hour 24")
	}
	if !strings.Contains(err.Error(), batchActivity) {
		t.Fatalf("error %q does not name %q", err, batchActivity)
	}
}

func TestValidateRejectsEnabledWithNoHours(t *testing.T) {
	cfg := Config{Growth: Group{Enabled: true}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate = nil, want an error for an enabled batch with no hours")
	}
	if !strings.Contains(err.Error(), batchGrowth) {
		t.Fatalf("error %q does not name %q", err, batchGrowth)
	}

	// Disabled and empty is the normal "not configured" shape.
	off := Config{Growth: Group{Enabled: false}}
	if err := off.Validate(); err != nil {
		t.Fatalf("Validate(disabled, no hours) = %v, want nil", err)
	}
}

func TestValidateAcceptsZeroConfig(t *testing.T) {
	var cfg Config
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(zero) = %v, want nil", err)
	}
}

func TestDefaultConfigIsValid(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.Enabled {
		t.Fatal("DefaultConfig().Enabled = false, want true")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("DefaultConfig().Validate() = %v, want nil", err)
	}
	for _, e := range cfg.groups() {
		if !e.g.Enabled {
			t.Fatalf("default group %q is disabled", e.name)
		}
		if e.name == RecoveryTaskName {
			if e.g.Every <= 0 {
				t.Fatalf("default recovery interval = %v, want > 0", e.g.Every)
			}
			continue
		}
		if len(e.g.Hours) == 0 {
			t.Fatalf("default group %q has no hours", e.name)
		}
	}
	if cfg.BalanceRefresh.Every <= 0 {
		t.Fatalf("DefaultConfig().BalanceRefresh.Every = %v, want > 0", cfg.BalanceRefresh.Every)
	}
}

// ---------------------------------------------------------------------------
// Jitter and pacing
// ---------------------------------------------------------------------------

func TestJitterStaysWithinBounds(t *testing.T) {
	if got := jitter(0); got != 0 {
		t.Fatalf("jitter(0) = %v, want 0", got)
	}
	if got := jitter(-time.Second); got != -time.Second {
		t.Fatalf("jitter(negative) = %v, want it unchanged", got)
	}
	for _, d := range []time.Duration{45 * time.Second, 5 * time.Second, time.Millisecond, 3 * time.Hour} {
		lo := float64(d) * (1 - jitterFrac)
		hi := float64(d) * (1 + jitterFrac)
		for i := 0; i < 2000; i++ {
			got := float64(jitter(d))
			if got < lo-1 || got > hi+1 {
				t.Fatalf("jitter(%v) = %v on iteration %d, outside [%v, %v]", d, time.Duration(got), i, time.Duration(lo), time.Duration(hi))
			}
		}
	}
}

func TestAccountGapHonoredAndOrderingIsPerAccountThenPerCode(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	f := &fakeClient{
		name: "fake",
		batches: []core.Batch{{
			Name:       batchCheckin,
			Codes:      []string{"c1", "c2"},
			Claim:      []string{"k1"},
			AccountGap: 100 * time.Millisecond,
			Settle:     10 * time.Millisecond,
		}},
		accounts: []core.AccountRecord{{ID: "acc1", Enabled: true}, {ID: "acc2", Enabled: true}},
		rec:      logs,
	}
	r := New(deps(registryOf(f), clk, logs))
	r.Reconfigure(Config{Enabled: true, Checkin: Group{Enabled: true, Hours: []int{8}}})

	rep, ok := r.RunBatchNow(context.Background(), "fake", batchCheckin)
	if !ok {
		t.Fatal("RunBatchNow: ok = false")
	}
	want := []string{
		"acc1/c1", "acc1/c2", "acc1/k1",
		"sleep",
		"acc2/c1", "acc2/c2", "acc2/k1",
		"sleep", // settle, after the batch
	}
	if got := logs.all(); !reflect.DeepEqual(got, want) {
		t.Fatalf("event order:\n got %v\nwant %v", got, want)
	}
	sleeps := clk.durations()
	if len(sleeps) != 2 {
		t.Fatalf("sleeps = %v, want a gap and a settle", sleeps)
	}
	if sleeps[0] < 80*time.Millisecond || sleeps[0] > 120*time.Millisecond {
		t.Fatalf("account gap = %v, want 100ms +/-20%%", sleeps[0])
	}
	if sleeps[1] != 10*time.Millisecond {
		t.Fatalf("settle = %v, want 10ms", sleeps[1])
	}
	if rep.Accounts != 2 || rep.Ran != 6 || rep.Refused != 0 || rep.Failed != 0 {
		t.Fatalf("report = %+v, want accounts=2 ran=6 refused=0 failed=0", rep)
	}
	// Started is taken before the pacing wait, so the gap is the only virtual
	// time inside the run; the settle happens after Duration is measured.
	if rep.Duration != sleeps[0] {
		t.Fatalf("report.Duration = %v, want the account gap %v", rep.Duration, sleeps[0])
	}
}

func TestTaskGapPacesCodesWithinAnAccount(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	f := &fakeClient{
		name: "fake",
		batches: []core.Batch{{
			Name:       batchGrowth,
			Codes:      []string{"c1", "c2"},
			TaskGap:    100 * time.Millisecond,
			AccountGap: 500 * time.Millisecond,
			Settle:     10 * time.Millisecond,
		}},
		accounts: []core.AccountRecord{{ID: "acc1", Enabled: true}, {ID: "acc2", Enabled: true}},
		rec:      logs,
	}
	r := New(deps(registryOf(f), clk, logs))
	r.Reconfigure(Config{Enabled: true, Growth: Group{Enabled: true, Hours: []int{12}}})

	rep, ok := r.RunBatchNow(context.Background(), "fake", batchGrowth)
	if !ok {
		t.Fatal("RunBatchNow: ok = false")
	}
	want := []string{
		"acc1/c1", "sleep", "acc1/c2",
		"sleep",
		"acc2/c1", "sleep", "acc2/c2",
		"sleep", // settle, after the batch
	}
	if got := logs.all(); !reflect.DeepEqual(got, want) {
		t.Fatalf("event order:\n got %v\nwant %v", got, want)
	}
	sleeps := clk.durations()
	if len(sleeps) != 4 {
		t.Fatalf("sleeps = %v, want two task gaps, one account gap and one settle", sleeps)
	}
	if sleeps[0] < 80*time.Millisecond || sleeps[0] > 120*time.Millisecond {
		t.Fatalf("first task gap = %v, want 100ms +/-20%%", sleeps[0])
	}
	if sleeps[1] < 400*time.Millisecond || sleeps[1] > 600*time.Millisecond {
		t.Fatalf("account gap = %v, want 500ms +/-20%%", sleeps[1])
	}
	if sleeps[2] < 80*time.Millisecond || sleeps[2] > 120*time.Millisecond {
		t.Fatalf("second task gap = %v, want 100ms +/-20%%", sleeps[2])
	}
	if sleeps[3] != 10*time.Millisecond {
		t.Fatalf("settle = %v, want 10ms", sleeps[3])
	}
	if rep.Accounts != 2 || rep.Ran != 4 || rep.Failed != 0 {
		t.Fatalf("report = %+v, want accounts=2 ran=4 failed=0", rep)
	}
}

func TestDefaultAccountGapWhenBatchLeavesItZero(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	f := &fakeClient{
		name: "fake",
		// No AccountGap and no Settle: both must fall back to the defaults.
		batches:  []core.Batch{{Name: batchCheckin, Codes: []string{"c1"}}},
		accounts: []core.AccountRecord{{ID: "a", Enabled: true}, {ID: "b", Enabled: true}},
		rec:      logs,
	}
	r := New(deps(registryOf(f), clk, logs))
	r.Reconfigure(Config{Enabled: true, Checkin: Group{Enabled: true, Hours: []int{8}}})

	if _, ok := r.RunBatchNow(context.Background(), "fake", batchCheckin); !ok {
		t.Fatal("RunBatchNow: ok = false")
	}
	sleeps := clk.durations()
	if len(sleeps) != 2 {
		t.Fatalf("sleeps = %v, want a default gap then a default settle", sleeps)
	}
	lo := float64(DefaultAccountGap) * (1 - jitterFrac)
	hi := float64(DefaultAccountGap) * (1 + jitterFrac)
	if float64(sleeps[0]) < lo-1 || float64(sleeps[0]) > hi+1 {
		t.Fatalf("default account gap = %v, want DefaultAccountGap +/-20%%", sleeps[0])
	}
	if sleeps[1] != DefaultSettle {
		t.Fatalf("default settle = %v, want %v", sleeps[1], DefaultSettle)
	}
}

func TestDisabledAccountsAreSkipped(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	f := &fakeClient{
		name:    "fake",
		batches: []core.Batch{oneBatch(batchCheckin, "c1")},
		accounts: []core.AccountRecord{
			{ID: "on", Enabled: true},
			{ID: "off", Enabled: false},
		},
		rec: logs,
	}
	r := New(deps(registryOf(f), clk, logs))
	r.Reconfigure(Config{Enabled: true, Checkin: Group{Enabled: true, Hours: []int{8}}})

	rep, _ := r.RunBatchNow(context.Background(), "fake", batchCheckin)
	if got := logs.all(); !reflect.DeepEqual(got, []string{"on/c1", "sleep"}) {
		t.Fatalf("events = %v, want only the enabled account", got)
	}
	if rep.Accounts != 1 || rep.Ran != 1 {
		t.Fatalf("report = %+v, want accounts=1 ran=1", rep)
	}
}

// ---------------------------------------------------------------------------
// Failure semantics
// ---------------------------------------------------------------------------

func TestTaskBatchSkipsMismatchedActionChannel(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	f := &fakeClient{
		name:    "zcode",
		batches: []core.Batch{oneBatch("checkin", "claim")},
		accounts: []core.AccountRecord{{
			ID:      "api-key-row",
			Enabled: true,
			Fields:  map[string]any{"kind": "api-key"},
		}},
		rec: logs,
	}
	c := &actionScopedClient{fakeClient: f, actions: []core.CheckinAction{{ID: "claim", Channels: []string{"jwt"}}}}
	r := New(deps(registryOf(c), clk, logs))
	r.Reconfigure(Config{})

	rep, ok := r.RunBatchNow(context.Background(), "zcode", "checkin")
	if !ok {
		t.Fatal("RunBatchNow: ok = false")
	}
	if rep.Skipped != 1 || rep.Ran != 0 || rep.Refused != 0 || rep.Failed != 0 {
		t.Fatalf("report = %+v, want one channel-scoped skip and no calls", rep)
	}
	if got := logs.all(); len(got) != 1 || got[0] != "sleep" {
		t.Fatalf("events = %v, want only the post-batch settle", got)
	}
}

func TestRefusalReasonUsesErrorAndSkipIsNotRefusal(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	f := &fakeClient{
		name:     "fake",
		batches:  []core.Batch{oneBatch("checkin", "skip", "refuse")},
		accounts: []core.AccountRecord{{ID: "a1", Enabled: true}},
		rec:      logs,
		run: func(_ context.Context, _, code string) (core.TaskResult, error) {
			switch code {
			case "skip":
				return core.TaskResult{OK: false, Skipped: true, Message: "already done"}, nil
			default:
				return core.TaskResult{OK: false, Error: "quota exhausted"}, nil
			}
		},
	}
	r := New(deps(registryOf(f), clk, logs))
	r.Reconfigure(Config{})

	rep, _ := r.RunBatchNow(context.Background(), "fake", "checkin")
	if rep.Ran != 2 || rep.Skipped != 1 || rep.Refused != 1 || rep.Failed != 0 {
		t.Fatalf("report = %+v, want ran=2 skipped=1 refused=1 failed=0", rep)
	}
	if len(rep.Refusals) != 1 || rep.Refusals[0] != "quota exhausted" {
		t.Fatalf("refusals = %v, want the business error reason", rep.Refusals)
	}
}

func TestRefusalIsNotAnErrorAndDoesNotAbort(t *testing.T) {

	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	f := &fakeClient{
		name:     "fake",
		batches:  []core.Batch{oneBatch(batchCheckin, "c1", "c2", "c3")},
		accounts: []core.AccountRecord{{ID: "a1", Enabled: true}, {ID: "a2", Enabled: true}},
		rec:      logs,
		run: func(ctx context.Context, accountID, code string) (core.TaskResult, error) {
			if code == "c2" {
				return core.TaskResult{OK: false, Code: code, Message: "already claimed"}, nil
			}
			return core.TaskResult{OK: true, Code: code}, nil
		},
	}
	r := New(deps(registryOf(f), clk, logs))
	cfg := Config{Enabled: true, Checkin: Group{Enabled: true, Hours: []int{8}}}
	r.Reconfigure(cfg)

	rep, ok := r.RunBatchNow(context.Background(), "fake", batchCheckin)
	if !ok {
		t.Fatal("RunBatchNow: ok = false")
	}
	if rep.Ran != 6 || rep.Refused != 2 || rep.Failed != 0 {
		t.Fatalf("report = %+v, want ran=6 refused=2 failed=0", rep)
	}
	if len(rep.Errors) != 0 {
		t.Fatalf("report.Errors = %v, want none (a refusal is not an error)", rep.Errors)
	}
	want := []string{"a1/c1", "a1/c2", "a1/c3", "sleep", "a2/c1", "a2/c2", "a2/c3", "sleep"}
	if got := logs.all(); !reflect.DeepEqual(got, want) {
		t.Fatalf("refusal aborted the batch:\n got %v\nwant %v", got, want)
	}
}

func TestRunTaskErrorIsRecordedAndDoesNotAbort(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	boom := errors.New("transport: connection reset by peer")
	f := &fakeClient{
		name:     "fake",
		batches:  []core.Batch{oneBatch(batchCheckin, "c1", "c2")},
		accounts: []core.AccountRecord{{ID: "a1", Enabled: true}, {ID: "a2", Enabled: true}},
		rec:      logs,
		run: func(ctx context.Context, accountID, code string) (core.TaskResult, error) {
			if code == "c1" {
				return core.TaskResult{}, boom
			}
			return core.TaskResult{OK: true, Code: code}, nil
		},
	}
	r := New(deps(registryOf(f), clk, logs))
	r.Reconfigure(Config{Enabled: true, Checkin: Group{Enabled: true, Hours: []int{8}}})

	rep, _ := r.RunBatchNow(context.Background(), "fake", batchCheckin)
	if rep.Failed != 2 {
		t.Fatalf("report.Failed = %d, want 2", rep.Failed)
	}
	if !reflect.DeepEqual(rep.Errors, []string{boom.Error(), boom.Error()}) {
		t.Fatalf("report.Errors = %v, want the two transport errors", rep.Errors)
	}
	if rep.Ran != 2 {
		t.Fatalf("report.Ran = %d, want 2 (only the calls that returned)", rep.Ran)
	}
	want := []string{"a1/c1", "a1/c2", "sleep", "a2/c1", "a2/c2", "sleep"}
	if got := logs.all(); !reflect.DeepEqual(got, want) {
		t.Fatalf("a Go error aborted the batch:\n got %v\nwant %v", got, want)
	}
}

func TestAccountsErrorFallsBackToTheDefaultAccount(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	f := &fakeClient{
		name:        "fake",
		batches:     []core.Batch{oneBatch(batchCheckin, "c1")},
		accountsErr: errors.New("store is unreadable"),
		rec:         logs,
	}
	r := New(deps(registryOf(f), clk, logs))
	r.Reconfigure(Config{Enabled: true, Checkin: Group{Enabled: true, Hours: []int{8}}})

	rep, _ := r.RunBatchNow(context.Background(), "fake", batchCheckin)
	if rep.Failed != 1 || rep.Ran != 1 {
		t.Fatalf("report = %+v, want failed=1 ran=1", rep)
	}
	if want := []string{"store is unreadable"}; !reflect.DeepEqual(rep.Errors, want) {
		t.Fatalf("report.Errors = %v, want %v", rep.Errors, want)
	}
	if got := logs.all(); !reflect.DeepEqual(got, []string{"/c1", "sleep"}) {
		t.Fatalf("events = %v, want the batch to run once on the default account", got)
	}
}

// ---------------------------------------------------------------------------
// Gate
// ---------------------------------------------------------------------------

func TestGateSkips(t *testing.T) {
	cases := []struct {
		name  string
		tasks []core.TaskInfo
		want  []string
	}{
		{
			name:  "absent",
			tasks: nil,
			want:  []string{"sleep"},
		},
		{
			name:  "already complete",
			tasks: []core.TaskInfo{{Code: "gate", Target: 1, Current: 1}},
			want:  []string{"sleep"},
		},
		{
			name:  "already claimed",
			tasks: []core.TaskInfo{{Code: "gate", Target: 0, Current: 0, Claimed: true}},
			want:  []string{"sleep"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := &recorder{}
			clk := newFakeClock(cstMidnight, logs)
			f := &fakeClient{
				name: "fake",
				batches: []core.Batch{{
					Name:   batchTravel,
					Codes:  []string{"t1"},
					Gate:   "gate",
					Settle: time.Millisecond,
				}},
				accounts: []core.AccountRecord{{ID: "a1", Enabled: true}},
				tasks:    map[string][]core.TaskInfo{"a1": tc.tasks},
				rec:      logs,
			}
			r := New(deps(registryOf(f), clk, logs))
			r.Reconfigure(Config{Enabled: true, Travel: Group{Enabled: true, Hours: []int{9}}})

			rep, ok := r.RunBatchNow(context.Background(), "fake", batchTravel)
			if !ok {
				t.Fatal("RunBatchNow: ok = false")
			}
			if rep.Ran != 0 || rep.Refused != 0 || rep.Failed != 0 {
				t.Fatalf("report = %+v, want a clean skip", rep)
			}
			if rep.Accounts != 1 {
				t.Fatalf("report.Accounts = %d, want 1", rep.Accounts)
			}
			if len(rep.Errors) != 0 {
				t.Fatalf("report.Errors = %v, want none (a gated account is not a failure)", rep.Errors)
			}
			if got := logs.all(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("events = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGateOpenRunsTheBatch(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	f := &fakeClient{
		name: "fake",
		batches: []core.Batch{{
			Name:   batchTravel,
			Codes:  []string{"t1"},
			Gate:   "gate",
			Settle: time.Millisecond,
		}},
		accounts: []core.AccountRecord{{ID: "a1", Enabled: true}},
		tasks:    map[string][]core.TaskInfo{"a1": {{Code: "gate", Target: 3, Current: 1}}},
		rec:      logs,
	}
	r := New(deps(registryOf(f), clk, logs))
	r.Reconfigure(Config{Enabled: true, Travel: Group{Enabled: true, Hours: []int{9}}})

	rep, _ := r.RunBatchNow(context.Background(), "fake", batchTravel)
	if rep.Ran != 1 || rep.Refused != 0 || rep.Failed != 0 {
		t.Fatalf("report = %+v, want ran=1", rep)
	}
	if got := logs.all(); !reflect.DeepEqual(got, []string{"a1/t1", "sleep"}) {
		t.Fatalf("events = %v", got)
	}
}

func TestGateReadErrorIsRecordedAndSkipped(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	f := &fakeClient{
		name:     "fake",
		batches:  []core.Batch{{Name: batchTravel, Codes: []string{"t1"}, Gate: "gate", Settle: time.Millisecond}},
		accounts: []core.AccountRecord{{ID: "a1", Enabled: true}},
		tasksErr: errors.New("tasks endpoint down"),
		rec:      logs,
	}
	r := New(deps(registryOf(f), clk, logs))
	r.Reconfigure(Config{Enabled: true, Travel: Group{Enabled: true, Hours: []int{9}}})

	rep, _ := r.RunBatchNow(context.Background(), "fake", batchTravel)
	if rep.Failed != 1 || rep.Ran != 0 {
		t.Fatalf("report = %+v, want failed=1 ran=0", rep)
	}
	if want := []string{"tasks endpoint down"}; !reflect.DeepEqual(rep.Errors, want) {
		t.Fatalf("report.Errors = %v, want %v", rep.Errors, want)
	}
}

// ---------------------------------------------------------------------------
// Run
// ---------------------------------------------------------------------------

func TestRunFiresAtTheScheduledHourAndStopsWithTheContext(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(time.Date(2026, 3, 4, 8, 30, 0, 0, CST), logs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fakeClient{
		name:    "fake",
		batches: []core.Batch{oneBatch(batchCheckin, "c1")},
		rec:     logs,
	}
	r := New(deps(registryOf(f), clk, logs))
	r.Reconfigure(Config{Enabled: true, Checkin: Group{Enabled: true, Hours: []int{9}}})

	// The scheduler now dispatches batches to per-platform lanes.  With the
	// fake clock a central loop can otherwise race ahead to tomorrow before the
	// lane worker gets CPU; hold the post-dispatch wait until the batch has
	// actually started, then cancel it.
	var cancelled atomic.Bool
	clk.onSleep = func() bool {
		if len(clk.durations()) < 2 {
			return true
		}
		if !cancelled.CompareAndSwap(false, true) {
			return true
		}
		deadline := time.Now().Add(2 * time.Second)
		for {
			for _, event := range logs.all() {
				if event == "/c1" {
					cancel()
					return false
				}
			}
			if time.Now().After(deadline) {
				return false
			}
			time.Sleep(time.Millisecond)
		}
	}

	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context died")
	}

	got := logs.all()
	if len(got) < 3 || got[0] != "sleep" || got[len(got)-1] != "sleep" {
		t.Fatalf("events = %v, want a wait, the batch, and a final settle", got)
	}
	tasks := 0
	for _, event := range got {
		if event == "/c1" {
			tasks++
		}
	}
	if tasks != 1 {
		t.Fatalf("events = %v, want the scheduled batch exactly once", got)
	}
	if waits := clk.durations(); len(waits) == 0 || waits[0] != 30*time.Minute {
		t.Fatalf("first wait = %v, want exactly 30m", waits)
	}
	st := r.Status()
	last, ok := st.Last["fake/"+batchCheckin]
	if !ok {
		t.Fatalf("Status().Last = %v, want the checkin run", st.Last)
	}
	if last.Ran != 1 || last.Refused != 0 || last.Failed != 0 {
		t.Fatalf("last report = %+v", last)
	}
}

func TestRunIsIdleWhenNothingIsEnabled(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	f := &fakeClient{name: "fake", batches: []core.Batch{oneBatch(batchCheckin, "c1")}, rec: logs}
	r := New(deps(registryOf(f), clk, logs))
	// Master switch off (the zero config): Run must block, not spin.
	r.Reconfigure(Config{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return when the context died")
	}
	if waits := clk.durations(); len(waits) != 0 {
		t.Fatalf("waits = %v, want none", waits)
	}
	if got := logs.all(); len(got) != 0 {
		t.Fatalf("events = %v, want none", got)
	}
}

func TestRunIsIdleWhenNoClientPlansTheEnabledBatch(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	f := &fakeClient{name: "fake", batches: []core.Batch{oneBatch(batchCheckin, "c1")}, rec: logs}
	r := New(deps(registryOf(f), clk, logs))
	// Only batches nobody plans are enabled.
	r.Reconfigure(Config{Enabled: true, Growth: Group{Enabled: true, Hours: []int{12}}})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return when the context died")
	}
	if got := logs.all(); len(got) != 0 {
		t.Fatalf("events = %v, want none", got)
	}
}

func TestReconfigureWakesAnIdleRun(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(time.Date(2026, 3, 4, 8, 0, 0, 0, CST), logs)
	f := &fakeClient{
		name:    "fake",
		batches: []core.Batch{oneBatch(batchCheckin, "c1")},
		rec:     logs,
	}
	r := New(deps(registryOf(f), clk, logs))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()

	// Idle first: the runner is parked, and Reconfigure must reach it.
	r.Reconfigure(Config{Enabled: true, Checkin: Group{Enabled: true, Hours: []int{9}}})
	deadline := time.After(5 * time.Second)
	for {
		if waits := clk.durations(); len(waits) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Reconfigure did not wake the idle Run")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return when the context died")
	}
}

func TestBalanceRefreshTick(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	f := &fakeClient{name: "fake", batches: []core.Batch{oneBatch(batchCheckin, "c1")}, rec: logs}
	// The hook runs on the scheduler's goroutine while the polling loop below
	// reads the counter, so it has to be atomic: a plain int is a data race the
	// -race detector is right to report, and the CI race job would flake on it.
	var ticks atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := deps(registryOf(f), clk, logs)
	d.OnBalanceRefresh = func(ctx context.Context) {
		if ticks.Add(1) == 1 {
			cancel()
		}
	}
	r := New(d)

	cfg := Config{Enabled: true, BalanceRefresh: struct {
		Enabled bool
		Every   time.Duration
	}{Enabled: true, Every: time.Hour}}
	r.Reconfigure(cfg)

	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	deadline := time.After(5 * time.Second)
	for {
		if ticks.Load() > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the balance-refresh hook never fired")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return when the context died")
	}
	if got := ticks.Load(); got != 1 {
		t.Fatalf("ticks = %d, want 1", got)
	}
	if waits := clk.durations(); len(waits) == 0 || waits[0] != time.Hour {
		t.Fatalf("waits = %v, want a one-hour tick", waits)
	}
	if got := logs.all(); len(got) != 0 {
		for _, e := range got {
			if e != "sleep" {
				t.Fatalf("events = %v, want no batch to run (no group enabled)", got)
			}
		}
		// The only entries are the waits themselves.
	}
	st := r.Status()
	want := cstMidnight.Add(2 * time.Hour)
	if !st.Next[BalanceTickName].Equal(want) {
		t.Fatalf("Status().Next[%q] = %s, want %s", BalanceTickName, st.Next[BalanceTickName], want)
	}
}

// ---------------------------------------------------------------------------
// RunBatchNow / Status
// ---------------------------------------------------------------------------

func TestRunBatchNowIgnoresTheTimetable(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	f := &fakeClient{name: "fake", batches: []core.Batch{oneBatch(batchCheckin, "c1")}, rec: logs}
	r := New(deps(registryOf(f), clk, logs))
	// Everything in the timetable is off: a manual run must still work.
	r.Reconfigure(Config{})

	rep, ok := r.RunBatchNow(context.Background(), "fake", batchCheckin)
	if !ok {
		t.Fatal("RunBatchNow: ok = false, want true")
	}
	if rep.Ran != 1 {
		t.Fatalf("report = %+v, want ran=1", rep)
	}
	if got := logs.all(); !reflect.DeepEqual(got, []string{"/c1", "sleep"}) {
		t.Fatalf("events = %v", got)
	}
}

func TestRunBatchNowUnknownClientOrBatch(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	f := &fakeClient{name: "fake", batches: []core.Batch{oneBatch(batchCheckin, "c1")}, rec: logs}
	r := New(deps(registryOf(f), clk, logs))

	if rep, ok := r.RunBatchNow(context.Background(), "nope", batchCheckin); ok || rep.Client != "" {
		t.Fatalf("unknown client returned (%+v, %v), want (zero, false)", rep, ok)
	}
	if rep, ok := r.RunBatchNow(context.Background(), "fake", "nope"); ok || rep.Client != "" {
		t.Fatalf("unknown batch returned (%+v, %v), want (zero, false)", rep, ok)
	}
	if got := logs.all(); len(got) != 0 {
		t.Fatalf("events = %v, want none", got)
	}
}

func TestRunBatchNowRefusesAClientThatCannotRunTasks(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	r := New(deps(registryOf(plannerOnly{name: "planner"}), clk, logs))
	if _, ok := r.RunBatchNow(context.Background(), "planner", batchCheckin); ok {
		t.Fatal("RunBatchNow on a BatchPlanner without TaskProvider: ok = true, want false")
	}
}

func TestNilRegistryIsSafe(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	r := New(Deps{Now: clk.Now, Sleep: clk.Sleep, Logf: func(string, ...any) {}})
	r.Reconfigure(Config{Enabled: true, Checkin: Group{Enabled: true, Hours: []int{8}}})

	if _, ok := r.RunBatchNow(context.Background(), "fake", batchCheckin); ok {
		t.Fatal("RunBatchNow with a nil registry: ok = true, want false")
	}
	if st := r.Status(); len(st.Next) != 0 || len(st.Last) != 0 {
		t.Fatalf("Status = %+v, want empty", st)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return when the context died")
	}
}

func TestNilLogfIsSafe(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	f := &fakeClient{name: "fake", batches: []core.Batch{oneBatch(batchCheckin, "c1")}, rec: logs}
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("nil Logf panicked: %v", p)
		}
	}()
	r := New(Deps{Registry: registryOf(f), Now: clk.Now, Sleep: clk.Sleep})
	r.Reconfigure(Config{Enabled: true, Checkin: Group{Enabled: true, Hours: []int{8}}})
	if _, ok := r.RunBatchNow(context.Background(), "fake", batchCheckin); !ok {
		t.Fatal("RunBatchNow: ok = false")
	}
}

func TestStatusNextListsOnlyEnabledPlannedBatches(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(time.Date(2026, 3, 4, 7, 0, 0, 0, CST), logs)
	f := &fakeClient{
		name: "fake",
		batches: []core.Batch{
			oneBatch(batchCheckin, "c1"),
			oneBatch(batchTravel, "t1"),
			oneBatch("custom", "x1"), // no timetable group
		},
		rec: logs,
	}
	reg := registryOf(f, plainClient{name: "plain"}, plannerOnly{name: "planner"})
	r := New(deps(reg, clk, logs))
	cfg := DefaultConfig()
	cfg.BalanceRefresh.Enabled = false
	r.Reconfigure(cfg)

	st := r.Status()
	want := map[string]time.Time{
		batchCheckin: time.Date(2026, 3, 4, 8, 0, 0, 0, CST),
		batchTravel:  time.Date(2026, 3, 4, 9, 0, 0, 0, CST),
	}
	recoveryAt, hasRecovery := st.Next[RecoveryTaskName]
	if hasRecovery {
		delete(st.Next, RecoveryTaskName)
	}
	dailyAt, hasDaily := st.Next[DailyBalanceTaskName]
	if hasDaily {
		delete(st.Next, DailyBalanceTaskName)
	}
	if !reflect.DeepEqual(st.Next, want) {
		t.Fatalf("Status().Next = %v, want %v plus recovery", st.Next, want)
	}
	if !hasRecovery || recoveryAt.Sub(clk.Now()) < 3*time.Hour || recoveryAt.Sub(clk.Now()) > 5*time.Hour {
		t.Fatalf("recovery next = %v, want one 3h..5h window", recoveryAt)
	}
	if !hasDaily || dailyAt.Sub(clk.Now()) <= 0 || dailyAt.In(CST).Hour() != 0 || dailyAt.In(CST).Minute() >= 30 {
		t.Fatalf("daily balance next = %v, want one 00:00..00:30 CST window", dailyAt)
	}
	if !st.Enabled {
		t.Fatal("Status().Enabled = false, want true")
	}
}

func TestStatusNextHidesDisabledAndUnconfiguredBatch(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(time.Date(2026, 3, 4, 7, 0, 0, 0, CST), logs)
	f := &fakeClient{name: "fake", batches: []core.Batch{oneBatch(batchCheckin, "c1")}, rec: logs}
	r := New(deps(registryOf(f), clk, logs))
	r.Reconfigure(Config{Enabled: false, Checkin: Group{Enabled: true, Hours: []int{8}}})

	st := r.Status()
	if st.Enabled || len(st.Next) != 0 {
		t.Fatalf("Status = %+v, want disabled with no next fires", st)
	}

	r.Reconfigure(Config{Enabled: true, Checkin: Group{Enabled: false, Hours: []int{8}}})
	if st := r.Status(); len(st.Next) != 0 {
		t.Fatalf("Status().Next = %v, want empty for a disabled group", st.Next)
	}

	r.Reconfigure(Config{Enabled: true, Checkin: Group{Enabled: true, Hours: []int{8}}})
	if _, ok := r.Status().Next[batchCheckin]; !ok {
		t.Fatalf("Status().Next = %v, want checkin", r.Status().Next)
	}
}

func TestStatusLastSurvivesAndIsKeyedByClientAndBatch(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	a := &fakeClient{name: "a", batches: []core.Batch{oneBatch(batchCheckin, "c1")}, rec: logs}
	b := &fakeClient{name: "b", batches: []core.Batch{oneBatch(batchCheckin, "c1")}, rec: logs}
	r := New(deps(registryOf(a, b), clk, logs))
	r.Reconfigure(Config{Enabled: true, Checkin: Group{Enabled: true, Hours: []int{8}}})

	if _, ok := r.RunBatchNow(context.Background(), "a", batchCheckin); !ok {
		t.Fatal("RunBatchNow(a): ok = false")
	}
	st := r.Status()
	if len(st.Last) != 1 {
		t.Fatalf("Status().Last = %v, want one entry", st.Last)
	}
	if _, ok := st.Last["a/"+batchCheckin]; !ok {
		t.Fatalf("Status().Last = %v, want key a/checkin", st.Last)
	}
}

// ---------------------------------------------------------------------------
// Logging and misc
// ---------------------------------------------------------------------------

func TestLogsArePrefixed(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	f := &fakeClient{name: "fake", batches: []core.Batch{oneBatch(batchCheckin, "c1")}, rec: logs}
	r := New(deps(registryOf(f), clk, logs))
	r.Reconfigure(Config{Enabled: true, Checkin: Group{Enabled: true, Hours: []int{8}}})
	if _, ok := r.RunBatchNow(context.Background(), "fake", batchCheckin); !ok {
		t.Fatal("RunBatchNow: ok = false")
	}
	lines := logs.lines()
	if len(lines) == 0 {
		t.Fatal("no log lines recorded")
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "[scheduler]") {
			t.Fatalf("log line %q is not prefixed with [scheduler]", l)
		}
	}
}

func TestPlainClientIsNeverScheduled(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	r := New(deps(registryOf(plainClient{name: "plain"}, plannerOnly{name: "planner"}), clk, logs))
	r.Reconfigure(DefaultConfig())

	for _, f := range r.plan(clk.Now(), func() Config { c, _ := r.config(); return c }()) {
		if f.batch != RecoveryTaskName && f.batch != DailyBalanceTaskName {
			t.Fatalf("plan = %v, want no daily batch for capability-less clients", f)
		}
	}
	st := r.Status()
	if _, ok := st.Next[RecoveryTaskName]; !ok {
		t.Fatalf("Status().Next = %v, want recovery", st.Next)
	}
	if _, ok := st.Next[DailyBalanceTaskName]; !ok {
		t.Fatalf("Status().Next = %v, want daily_balance", st.Next)
	}
	for name := range st.Next {
		if name != RecoveryTaskName && name != DailyBalanceTaskName && name != BalanceTickName {
			t.Fatalf("Status().Next = %v, want only the synthetic tasks and the balance tick", st.Next)
		}
	}
}

// ---------------------------------------------------------------------------
// Manual balance refresh
// ---------------------------------------------------------------------------

// TestRunBalanceRefreshNowFiresTheHookAndKeepsTheTimetable pins both halves of
// a manual press: the hook really runs -- so the panel's 刷新余额 button is not
// a no-op that still answers 200 -- and the stored tick does not move, so
// pressing it twice cannot silently postpone the next automatic refresh.
func TestRunBalanceRefreshNowFiresTheHookAndKeepsTheTimetable(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	var calls int
	d := deps(registryOf(), clk, logs)
	d.OnBalanceRefresh = func(context.Context) { calls++ }
	r := New(d)
	r.Reconfigure(DefaultConfig())

	_, before := r.config()
	if before.IsZero() {
		t.Fatal("Reconfigure left no balance tick to preserve")
	}
	if !r.RunBalanceRefreshNow(context.Background()) {
		t.Fatal("RunBalanceRefreshNow = false, want true when a hook is wired")
	}
	if calls != 1 {
		t.Fatalf("hook calls = %d, want 1", calls)
	}
	if _, after := r.config(); !after.Equal(before) {
		t.Fatalf("balNext moved from %s to %s; a manual press must not reschedule", before, after)
	}
}

// TestRunBalanceRefreshNowWithoutAHookSaysSo covers the other half: a host that
// never wired Deps.OnBalanceRefresh gets false -- which the panel turns into
// 501 -- rather than a panic or a silently successful 200 for work that never
// happened.
func TestRunBalanceRefreshNowWithoutAHookSaysSo(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	r := New(deps(registryOf(), clk, logs))
	r.Reconfigure(DefaultConfig())

	if r.RunBalanceRefreshNow(context.Background()) {
		t.Fatal("RunBalanceRefreshNow = true with no hook wired")
	}
}

// ---------------------------------------------------------------------------
// Per-platform overrides
//
// 同一批 checkin，A 平台可以跑自己的 07:00，B 平台继续跑共享的 09:00。
// 「某个平台几点签到」正是任务中心要回答的问题，所以这三件事必须各自可
// 验证：查表（GroupFor/Override）、排期（NextByClient）、校验（Validate）。
// ---------------------------------------------------------------------------

func TestGroupForPrefersThePerClientOverride(t *testing.T) {
	cfg := Config{
		Enabled: true,
		Checkin: Group{Enabled: true, Hours: []int{9}},
		Clients: map[string]map[string]Group{
			// Deliberately odd casing: both names come from operator JSON.
			"WB": {"Checkin": {Enabled: true, Hours: []int{7}}},
		},
	}
	if g, ok := cfg.GroupFor("wb", "checkin"); !ok || len(g.Hours) != 1 || g.Hours[0] != 7 {
		t.Fatalf("GroupFor(wb) = %+v/%v, want the 07:00 override", g, ok)
	}
	if g, ok := cfg.GroupFor("other", "checkin"); !ok || len(g.Hours) != 1 || g.Hours[0] != 9 {
		t.Fatalf("GroupFor(other) = %+v/%v, want the shared 09:00", g, ok)
	}
	if _, ok := cfg.Override("wb", "checkin"); !ok {
		t.Error("Override(wb) did not report the entry the panel must mark as 自定义")
	}
	if _, ok := cfg.Override("other", "checkin"); ok {
		t.Error("Override(other) invented an entry")
	}
	// A known batch whose group is off has no timetable to run, and a batch
	// name outside the six is not schedulable at all.
	if g, ok := cfg.GroupFor("wb", batchTravel); !ok || g.Enabled {
		t.Errorf("GroupFor(wb, travel) = %+v/%v, want a known but disabled group", g, ok)
	}
	if _, ok := cfg.GroupFor("wb", "orphan"); ok {
		t.Fatal("GroupFor(wb, orphan) = ok, want an unknown batch to have no timetable")
	}
}

func TestPerClientOverridesPlanSeparately(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	a := &fakeClient{name: "a", batches: []core.Batch{oneBatch(batchCheckin, "c1")}, rec: logs}
	b := &fakeClient{name: "b", batches: []core.Batch{oneBatch(batchCheckin, "c1")}, rec: logs}
	r := New(deps(registryOf(a, b), clk, logs))
	r.Reconfigure(Config{
		Enabled: true,
		Checkin: Group{Enabled: true, Hours: []int{9}},
		Clients: map[string]map[string]Group{
			"a": {batchCheckin: {Enabled: true, Hours: []int{7}}},
		},
	})
	st := r.Status()
	wantA := time.Date(2026, 3, 4, 7, 0, 0, 0, CST)
	wantB := time.Date(2026, 3, 4, 9, 0, 0, 0, CST)
	if got := st.NextByClient["a/"+batchCheckin]; !got.Equal(wantA) {
		t.Errorf("NextByClient[a/checkin] = %s, want %s", got, wantA)
	}
	if got := st.NextByClient["b/"+batchCheckin]; !got.Equal(wantB) {
		t.Errorf("NextByClient[b/checkin] = %s, want %s", got, wantB)
	}
	// The batch-keyed map keeps its old meaning: the earliest fire, here a's.
	if got := st.Next[batchCheckin]; !got.Equal(wantA) {
		t.Errorf("Next[checkin] = %s, want the earliest %s", got, wantA)
	}
}

func TestValidateRejectsAnOutOfRangeOverrideHour(t *testing.T) {
	cfg := Config{
		Enabled: true,
		Clients: map[string]map[string]Group{
			"wb": {batchCheckin: {Enabled: true, Hours: []int{25, 7}}},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate accepted hour 25")
	}
	if !strings.Contains(err.Error(), "wb/checkin") {
		t.Errorf("error %q does not name the offending platform/batch", err)
	}
	// The surviving hour is still normalized in place, so a live timetable
	// never carries the bad value into the next plan.
	if got := cfg.Clients["wb"][batchCheckin].Hours; !reflect.DeepEqual(got, []int{7}) {
		t.Errorf("override hours after Validate = %v, want [7]", got)
	}
}

// ---------------------------------------------------------------------------
// Run history
// ---------------------------------------------------------------------------

func TestHistoryRecordsWhatStartedTheRun(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	f := &fakeClient{name: "fake", batches: []core.Batch{oneBatch(batchCheckin, "c1")}, rec: logs}
	r := New(deps(registryOf(f), clk, logs))
	r.Reconfigure(DefaultConfig())

	if _, ok := r.RunBatchNow(context.Background(), "fake", batchCheckin); !ok {
		t.Fatal("RunBatchNow: ok = false")
	}
	r.runBatch(context.Background(), "fake", batchCheckin)

	h := r.History()
	if len(h) != 2 {
		t.Fatalf("History = %+v, want the manual press and the scheduled run", h)
	}
	// Newest first: the scheduled run happened after the manual one.
	if h[0].Trigger != TriggerSchedule || h[1].Trigger != TriggerManual {
		t.Errorf("triggers = %q,%q, want schedule,manual (newest first)", h[0].Trigger, h[1].Trigger)
	}
}

func TestHistoryIsBounded(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	r := New(deps(registryOf(), clk, logs))
	for i := 0; i < historyMax+20; i++ {
		r.record(Report{Client: "c", Batch: batchCheckin, Started: cstMidnight.Add(time.Duration(i) * time.Minute)}, TriggerSchedule)
	}
	h := r.History()
	if len(h) != historyMax {
		t.Fatalf("History length = %d, want the bound %d", len(h), historyMax)
	}
	want := cstMidnight.Add(time.Duration(historyMax+19) * time.Minute)
	if !h[0].Started.Equal(want) {
		t.Errorf("newest entry = %s, want %s", h[0].Started, want)
	}
}

// TestHistorySurvivesARestart 钉住持久化：运行记录写进 HistoryPath，新的 Runner
// 起在同一路径上时读得回来——否则任务中心刷新一次就“看起来没人跑过”。
func TestHistorySurvivesARestart(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	f := &fakeClient{name: "fake", batches: []core.Batch{oneBatch(batchCheckin, "c1")}, rec: logs}
	path := filepath.Join(t.TempDir(), "schedule_runs.json")
	d := deps(registryOf(f), clk, logs)
	d.HistoryPath = path
	r := New(d)
	r.Reconfigure(DefaultConfig())
	if _, ok := r.RunBatchNow(context.Background(), "fake", batchCheckin); !ok {
		t.Fatal("RunBatchNow: ok = false")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the run journal was not written: %v", err)
	}

	// A second runner on the same path is a restart.
	r2 := New(d)
	h := r2.History()
	if len(h) != 1 || h[0].Client != "fake" || h[0].Trigger != TriggerManual {
		t.Fatalf("reloaded History = %+v, want the persisted manual run", h)
	}
}

// ---------------------------------------------------------------------------
// Check-in-only modules
// ---------------------------------------------------------------------------

func TestCheckinOnlyClientIsScheduled(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(time.Date(2026, 3, 4, 7, 0, 0, 0, CST), logs)
	c := &checkinOnly{name: "ci", accounts: []core.AccountRecord{{ID: "a"}}, rec: logs}
	r := New(deps(registryOf(c), clk, logs))
	cfg := DefaultConfig()
	cfg.BalanceRefresh.Enabled = false
	r.Reconfigure(cfg)

	st := r.Status()
	want := time.Date(2026, 3, 4, 8, 0, 0, 0, CST)
	if got := st.NextByClient["ci/"+core.CheckinBatchName]; !got.Equal(want) {
		t.Fatalf("NextByClient[ci/checkin] = %s, want %s", got, want)
	}
}

func TestSyntheticCheckinSkipsMismatchedActionChannel(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	base := &checkinOnly{
		name: "trae",
		accounts: []core.AccountRecord{{
			ID:      "api-key-row",
			Enabled: true,
			State:   "ready",
			Fields:  map[string]any{"kind": "api-key"},
		}},
		rec: logs,
	}
	c := &scopedCheckinOnly{checkinOnly: base, actions: []core.CheckinAction{{ID: "claim", Channels: []string{"jwt"}}}}
	r := New(deps(registryOf(c), clk, logs))
	r.Reconfigure(Config{})

	rep, ok := r.RunBatchNow(context.Background(), "trae", core.CheckinBatchName)
	if !ok {
		t.Fatal("RunBatchNow: ok = false")
	}
	if rep.Skipped != 1 || rep.Ran != 0 || rep.Refused != 0 || rep.Failed != 0 {
		t.Fatalf("report = %+v, want one channel-scoped skip and no calls", rep)
	}
	if got := logs.all(); len(got) != 1 || got[0] != "sleep" {
		t.Fatalf("events = %v, want only the post-batch settle", got)
	}
}

func TestSyntheticCheckinIncludesExhaustedButNotOperatorDisabled(t *testing.T) {

	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	c := &checkinOnly{
		name: "trae",
		accounts: []core.AccountRecord{
			{ID: "ready", Enabled: true, State: "ready"},
			{ID: "exhausted", Enabled: false, State: "exhausted"},
			{ID: "parked", Enabled: false, State: "exhausted", Fields: map[string]any{"disabled": true}},
			{ID: "invalid", Enabled: false, State: "invalid"},
		},
		rec: logs,
	}
	r := New(deps(registryOf(c), clk, logs))
	r.Reconfigure(Config{})

	rep, ok := r.RunBatchNow(context.Background(), "trae", core.CheckinBatchName)
	if !ok {
		t.Fatal("RunBatchNow: ok = false, want true")
	}
	if rep.Accounts != 2 || rep.Ran != 2 {
		t.Fatalf("report = %+v, want only ready+exhausted", rep)
	}
	want := []string{"checkin:ready/", "sleep", "checkin:exhausted/", "sleep"}
	if got := logs.all(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestRunBatchNowRunsTheSyntheticCheckin(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	c := &checkinOnly{
		name:     "ci",
		accounts: []core.AccountRecord{{ID: "a", Enabled: true}, {ID: "b", Enabled: true}, {ID: "off"}},
		rec:      logs,
	}
	r := New(deps(registryOf(c), clk, logs))
	r.Reconfigure(Config{})

	rep, ok := r.RunBatchNow(context.Background(), "ci", core.CheckinBatchName)
	if !ok {
		t.Fatal("RunBatchNow: ok = false, want true")
	}
	if rep.Accounts != 2 || rep.Ran != 2 || rep.Refused != 0 || rep.Failed != 0 {
		t.Fatalf("report = %+v, want 2 accounts, 2 runs, no refusals or failures", rep)
	}
	want := []string{"checkin:a/", "sleep", "checkin:b/", "sleep"}
	if got := logs.all(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestPendingOnlyBatchSkipsClaimedCodes(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	c := &fakeClient{
		name:     "fake",
		accounts: []core.AccountRecord{{ID: "a1", Enabled: true}},
		tasks: map[string][]core.TaskInfo{"a1": {
			{Code: "done", Claimed: true, Auto: true},
			{Code: "todo", Auto: true},
			{Code: "locked", Locked: true, Auto: true},
		}},
		batches: []core.Batch{{Name: "growth", Codes: []string{"done", "todo", "locked"}, PendingOnly: true, Settle: time.Millisecond}},
		rec:     logs,
	}
	r := New(deps(registryOf(c), clk, logs))
	r.Reconfigure(Config{Enabled: true, Growth: Group{Enabled: true, Hours: []int{12}}})

	rep := r.runBatch(context.Background(), "fake", "growth")
	if rep.Ran != 1 {
		t.Fatalf("ran = %d, want only the pending task", rep.Ran)
	}
	want := []string{"a1/todo", "sleep"}
	if got := logs.all(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestScheduledBatchHonoursAccountScope(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	c := &fakeClient{
		name:     "wb",
		accounts: []core.AccountRecord{{ID: "a1", Enabled: true}},
		tasks:    map[string][]core.TaskInfo{"a1": {{Code: "todo", Auto: true}}},
		batches:  []core.Batch{{Name: "growth", Codes: []string{"todo"}, Settle: time.Millisecond}},
		rec:      logs,
	}
	r := New(deps(registryOf(c), clk, logs))
	r.Reconfigure(Config{
		Enabled: true,
		Growth: Group{
			Enabled: true,
			Hours:   []int{12},
			Accounts: AccountScope{
				Mode:    AccountScopeInclude,
				Include: []string{"a2"},
			},
		},
	})

	rep := r.runBatch(context.Background(), "wb", "growth")
	if rep.Ran != 0 || rep.Accounts != 0 {
		t.Fatalf("report = %+v, want no account in the include scope", rep)
	}
}
func TestValidateAcceptsPeriodicRecoveryGroup(t *testing.T) {
	cfg := Config{Recovery: Group{Enabled: true, Every: 4 * time.Hour, Jitter: time.Hour}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(recovery every=4h jitter=1h) = %v, want nil", err)
	}
}

func TestValidateRecoveryStillRequiresAnIntervalWithHoursPresent(t *testing.T) {
	cfg := Config{Recovery: Group{Enabled: true, Hours: []int{9}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate(recovery with only hours) = nil, want a missing-interval error")
	}
}

func TestRecoveryGroupForUsesThePerClientOverride(t *testing.T) {
	cfg := Config{
		Enabled:  true,
		Recovery: Group{Enabled: true, Every: 4 * time.Hour, Jitter: time.Hour},
		Clients: map[string]map[string]Group{
			"loomy": {
				RecoveryTaskName: {Enabled: true, Every: 90 * time.Minute, Jitter: 15 * time.Minute},
			},
		},
	}
	g, ok := cfg.GroupFor("loomy", RecoveryTaskName)
	if !ok || g.Every != 90*time.Minute || g.Jitter != 15*time.Minute {
		t.Fatalf("GroupFor(loomy, recovery) = %+v/%v, want the 90m/15m override", g, ok)
	}
	shared, ok := cfg.GroupFor("other", RecoveryTaskName)
	if !ok || shared.Every != 4*time.Hour || shared.Jitter != time.Hour {
		t.Fatalf("GroupFor(other, recovery) = %+v/%v, want the shared 4h/1h", shared, ok)
	}
}

func TestRecoveryPlanAndStatusUseOneStoredWindow(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	f := &fakeClient{name: "fake", rec: logs}
	var calls atomic.Int32
	d := deps(registryOf(f), clk, logs)
	d.OnRecoveryProbe = func(_ context.Context, client string) {
		if client != "fake" {
			t.Errorf("recovery hook client = %q, want fake", client)
		}
		calls.Add(1)
	}
	r := New(d)
	r.Reconfigure(Config{Enabled: true, Recovery: Group{Enabled: true, Every: 4 * time.Hour, Jitter: time.Hour}})

	st := r.Status()
	at, ok := st.NextByClient["fake/"+RecoveryTaskName]
	if !ok {
		t.Fatalf("Status().NextByClient = %v, want fake/recovery", st.NextByClient)
	}
	wait := at.Sub(cstMidnight)
	if wait < 3*time.Hour || wait > 5*time.Hour {
		t.Fatalf("recovery next in %s, want 3h..5h", wait)
	}
	for _, f := range r.plan(clk.Now(), r.Config()) {
		if f.client == "fake" && f.batch == RecoveryTaskName && !f.at.Equal(at) {
			t.Fatalf("plan recovery at %s, Status reported %s", f.at, at)
		}
	}
	if !r.RunRecoveryNow(context.Background(), "fake") {
		t.Fatal("RunRecoveryNow = false, want true when the hook is wired")
	}
	if calls.Load() != 1 {
		t.Fatalf("recovery hook calls = %d, want 1", calls.Load())
	}
	if got := r.Status().NextByClient["fake/"+RecoveryTaskName]; !got.Equal(at) {
		t.Fatalf("manual recovery moved next from %s to %s", at, got)
	}
}

func TestRunFiresRecoveryProbe(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ticks atomic.Int32
	d := deps(registryOf(&fakeClient{name: "fake", rec: logs}), clk, logs)
	d.OnRecoveryProbe = func(context.Context, string) {
		if ticks.Add(1) == 1 {
			cancel()
		}
	}
	r := New(d)
	r.Reconfigure(Config{Enabled: true, Recovery: Group{Enabled: true, Every: 4 * time.Hour, Jitter: time.Hour}})
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the recovery hook")
	}
	if ticks.Load() != 1 {
		t.Fatalf("recovery ticks = %d, want 1", ticks.Load())
	}
	waits := clk.durations()
	if len(waits) != 1 || waits[0] < 3*time.Hour || waits[0] > 5*time.Hour {
		t.Fatalf("recovery waits = %v, want one 3h..5h wait", waits)
	}
}

func TestScheduledBatchesOnDifferentPlatformsRunConcurrently(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(time.Date(2026, 3, 4, 8, 30, 0, 0, CST), logs)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()

	startedA := make(chan struct{})
	startedB := make(chan struct{})
	a := &fakeClient{
		name:     "a",
		batches:  []core.Batch{oneBatch(batchCheckin, "a-task")},
		accounts: []core.AccountRecord{{ID: "a-account", Enabled: true}},
		rec:      logs,
		run: func(ctx context.Context, accountID, code string) (core.TaskResult, error) {
			close(startedA)
			select {
			case <-release:
				return core.TaskResult{OK: true, Code: code, AccountID: accountID}, nil
			case <-ctx.Done():
				return core.TaskResult{}, ctx.Err()
			}
		},
	}
	b := &fakeClient{
		name:     "b",
		batches:  []core.Batch{oneBatch(batchCheckin, "b-task")},
		accounts: []core.AccountRecord{{ID: "b-account", Enabled: true}},
		rec:      logs,
		run: func(ctx context.Context, accountID, code string) (core.TaskResult, error) {
			close(startedB)
			return core.TaskResult{OK: true, Code: code, AccountID: accountID}, nil
		},
	}
	r := New(deps(registryOf(a, b), clk, logs))
	r.Reconfigure(Config{Enabled: true, Checkin: Group{Enabled: true, Hours: []int{9}}})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	waitDone := func() {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Run did not stop after cancellation")
		}
	}

	select {
	case <-startedA:
	case <-time.After(2 * time.Second):
		cancel()
		waitDone()
		t.Fatal("the first platform's batch never started")
	}

	select {
	case <-startedB:
	case <-time.After(time.Second):
		unblock()
		cancel()
		waitDone()
		t.Fatal("a second platform was blocked behind the first platform's long batch")
	}

	unblock()
	cancel()
	waitDone()
}

func TestScheduledBatchesForOnePlatformStaySerial(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(time.Date(2026, 3, 4, 8, 30, 0, 0, CST), logs)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()

	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	var active, maxActive atomic.Int32
	a := &fakeClient{
		name: "a",
		batches: []core.Batch{
			oneBatch(batchCheckin, "first"),
			oneBatch(batchGrowth, "second"),
		},
		accounts: []core.AccountRecord{{ID: "a-account", Enabled: true}},
		rec:      logs,
		run: func(ctx context.Context, accountID, code string) (core.TaskResult, error) {
			n := active.Add(1)
			for {
				old := maxActive.Load()
				if n <= old || maxActive.CompareAndSwap(old, n) {
					break
				}
			}
			defer active.Add(-1)
			switch code {
			case "first":
				close(firstStarted)
				select {
				case <-release:
				case <-ctx.Done():
				}
			case "second":
				close(secondStarted)
			}
			return core.TaskResult{OK: true, Code: code, AccountID: accountID}, nil
		},
	}
	r := New(deps(registryOf(a), clk, logs))
	r.Reconfigure(Config{
		Enabled: true,
		Checkin: Group{Enabled: true, Hours: []int{9}},
		Growth:  Group{Enabled: true, Hours: []int{9}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	waitDone := func() {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Run did not stop after cancellation")
		}
	}

	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		cancel()
		waitDone()
		t.Fatal("the first batch never started")
	}
	select {
	case <-secondStarted:
		unblock()
		cancel()
		waitDone()
		t.Fatal("two batches for one platform overlapped")
	case <-time.After(100 * time.Millisecond):
	}
	if got := maxActive.Load(); got != 1 {
		unblock()
		cancel()
		waitDone()
		t.Fatalf("maximum overlapping tasks for one platform = %d, want 1", got)
	}

	unblock()
	select {
	case <-secondStarted:
	case <-time.After(2 * time.Second):
		cancel()
		waitDone()
		t.Fatal("the second batch did not start after the first finished")
	}
	cancel()
	waitDone()
}
