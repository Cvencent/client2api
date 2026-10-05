package workbuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// Task board ("growth centre") — the vendor's chore list plus the runners that
// complete those chores on the user's behalf.
//
// The wire formats here are ported from the reference implementation
// (client2api-lab/_upstream/workbuddy2api-panel, internal/upstream/{tasks.go,
// report.go, school.go, desktop.go, travel.go, blackcat.go} and
// internal/panel/autotask.go, MIT).  Three lessons from that port drive the
// shape of this file:
//
//  1. The task board is not one endpoint but two.  The default list is the web
//     view; the miniprogram list is a superset that also carries the school
//     season chores, and touching it needs the X-Client-Platform: miniprogram
//     header — without it the vendor answers "task not found".
//
//  2. Reporting an *event* is not the same as reporting a *chat*.  The same
//     POST /v2/report accepts a CLI fingerprint, a desktop fingerprint, a
//     browser fingerprint and a miniprogram fingerprint, and the vendor judges
//     each chore against the fingerprint it expects.  The fingerprints are
//     therefore spelled out per runner rather than shared.
//
//  3. Scoring is asynchronous.  The reference measured a 5-8 second lag
//     between an event and the board catching up, and measured that firing the
//     miniprogram chat events back to back gets the whole batch rolled back
//     (progress reverts, and the claim answers 400 "task not completed"),
//     while ~45s of spacing with jitter survives.  So: space the events, and
//     poll for the reward instead of trusting an immediate re-read.
//
// Everything here is stdlib-only and self-contained: it reuses this module's
// Auth/pool/header/HTTP helpers and reaches no other client package.

const (
	// Growth-centre routes.  The board and the "accept" (报名) call live on the
	// chat host; the reward claim is a browser route on the web host, and the
	// miniprogram variant of it is a chat-host route.
	tasksListPath   = "/v2/activity/growth/tasks"
	tasksAcceptPath = "/v2/activity/growth/tasks/accept"
	tasksClaimFmt   = "/activity/growth/tasks/%s/claim"

	// X-Client-Platform must say miniprogram on every miniprogram call,
	// including the list, or the vendor cannot find the task at all.
	mpPlatformHeader = "X-Client-Platform"
	mpPlatformValue  = "miniprogram"

	// The activity-report endpoint.  Every fingerprint posts an *array* here.
	reportPath       = "/v2/report"
	appearanceSetPth = "/v2/user-asset/appearance/set"

	// claimPollTries counts how many times the reward claim is re-read.
	claimPollTries = 4
)

// Production pacing.  These are the values the module ships with and the ones
// the vendor was actually measured against -- do not tighten them without
// re-measuring: the reference lost an entire batch of sequential chat events to
// 2s spacing.
//
// They are named constants rather than literals buried in the vars below so the
// anti-abuse guard can assert on what production runs without depending on the
// test binary's restore ordering.
const (
	// defaultMPChatEventGap spaces consecutive miniprogram chat events.
	defaultMPChatEventGap = 45 * time.Second
	// defaultMPChatJitter is the random extra delay on top of that gap.
	defaultMPChatJitter = 10 * time.Second
	// defaultReportGap matches the ~1.05s cadence the vendor's own script uses
	// between consecutive CLI activity reports.
	defaultReportGap = 1050 * time.Millisecond
	// defaultClaimPollGap covers the asynchronous scorer: an immediate re-read
	// still shows 0/1 for several seconds.
	defaultClaimPollGap = 3 * time.Second
	// defaultMPActionGap is the pause between accepting a miniprogram chore and
	// reading it back; the accept call can answer 200 before it has registered.
	defaultMPActionGap = 2 * time.Second
)

// The pacing the runners actually sleep on. These are vars rather than consts
// for exactly one reason: they are the module's wall-clock cost, and a test that
// walks a paced path would otherwise spend 45-50s per run proving something it
// could prove in milliseconds -- which made `go test ./clients/workbuddy` the
// slowest package in the tree by 3x. The test binary shrinks them once in
// TestMain and restores them on the way out; production always runs the defaults
// above, and TestWorkbuddyTaskAntiAbuseGapIsRespected pins those so nobody can
// quietly ship a tighter gap.
var (
	mpChatEventGap = defaultMPChatEventGap
	mpChatJitter   = defaultMPChatJitter
	reportGap      = defaultReportGap
	claimPollGap   = defaultClaimPollGap
	mpActionGap    = defaultMPActionGap
)

const (
	taskTimeout  = 30 * time.Second
	taskCacheTTL = 20 * time.Second

	// taskMaxEvents caps a single run so a malformed upstream target cannot turn
	// one click into a multi-hour loop.
	taskMaxEvents = 20

	// Desktop fingerprint constants.
	desktopVersion   = "5.5.6"
	desktopCommit    = "5f9692923c93033111c51ad7b003eb80204a9b75"
	desktopUserAgent = "WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1"
	taskModelID      = "fast-model"
	fpReportDelay    = 2000
	fpReleaseDate    = int64(1789036585355)

	// Miniprogram fingerprint constants.
	mpExtVersion       = "2.4.0"
	mpMachineID        = "0655736a-607f-4d9d-b430-58176ee9a090"
	mpSchoolActivityID = "school_open_day_2026"

	// defaultChatModelID is what the reference's plain chat activity report
	// claims to have used.
	defaultChatModelID   = "deepseek-v4-flash"
	defaultChatModelName = "DeepSeek V4 Flash"

	// appearanceThemeKey is the 和平精英 skin the reference applies for
	// Hp_Appearance.
	appearanceThemeKey = "theme-tkmwj7"

	// buddyAppID/buddyAppName are the assistant the Buddy_App chore binds.
	buddyAppID   = "cb_y5Dy46tPQGGWtueMxXbe"
	buddyAppName = "企鹅教师助手"

	// libraryDocURL is the资料库 document Library_read opens.
	libraryDocURL  = "https://www.workbuddy.cn/space/d/o0KWYeynteVv06UnAZqIFm"
	libraryDocElem = "library_doc_intro_click"
	libraryDocName = "WorkBuddy资料库介绍"

	// buddyGateMarker is what the vendor answers when first_buddy is attempted
	// before the adoption prerequisite has been credited.  It is expected, not a
	// failure, and must not be retried the same day.
	buddyGateMarker = "first_buddy task not completed yet"
)

// --- task board model -------------------------------------------------------

// growthTask is one row of the vendor's board.  Only the fields this module maps
// are modelled; unknown keys are ignored, so a vendor-side addition cannot break
// the listing.
type growthTask struct {
	TaskCode     string `json:"task_code"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	TaskDesc     string `json:"task_desc"`
	RewardCredit int64  `json:"reward_credit"`
	RewardEnergy int64  `json:"reward_energy"`
	TaskType     string `json:"task_type"`
	Tag          string `json:"tag"`
	Locked       bool   `json:"locked"`
	AcceptStatus string `json:"accept_status"`
	Status       string `json:"status"`
	Target       int64  `json:"target"`
	Current      int64  `json:"current"`

	// Progress is deliberately raw.  The reference reads it as
	// {"current":n,"target":m} and lets it override the flat fields, but the
	// vendor has also been seen sending a bare number, and a typed struct would
	// make that response fail to parse at all.
	Progress json.RawMessage `json:"progress"`
}

// applyProgress folds the optional progress field into Current/Target.
func (t *growthTask) applyProgress() {
	raw := bytes.TrimSpace(t.Progress)
	if len(raw) == 0 {
		return
	}
	switch raw[0] {
	case '{':
		var pr struct {
			Current int64 `json:"current"`
			Target  int64 `json:"target"`
		}
		if err := json.Unmarshal(raw, &pr); err == nil && (pr.Target > 0 || pr.Current > 0) {
			t.Current = pr.Current
			t.Target = pr.Target
		}
	case '"':
		var s string
		if json.Unmarshal(raw, &s) == nil {
			if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
				t.Current = n
			}
		}
	default:
		var n int64
		if json.Unmarshal(raw, &n) == nil {
			t.Current = n
		}
	}
}

// claimed reports whether the reward has already been taken.  That is an
// idempotent success, never an error.
func (t *growthTask) claimed() bool {
	return strings.EqualFold(strings.TrimSpace(t.AcceptStatus), "claimed")
}

// claimable reports whether the chore is finished but the reward is still
// sitting there.
func (t *growthTask) claimable() bool {
	return !t.claimed() && t.Target > 0 && t.Current >= t.Target
}

// taskRow is one board row together with the channel it came from, because the
// same code means different things on the two channels.
type taskRow struct {
	task growthTask
	mp   bool
}

// mpTaskCodes are the codes the reference routes through the miniprogram
// channel.  Every code in this set has a ported runner; the set is still kept
// separate from the runner table because the two answer different questions --
// this one says which channel the board row lives on, the table says what to
// report.
var mpTaskCodes = map[string]bool{
	"school_season":      true,
	"Sequential_Tasks_1": true,
	"Sequential_Tasks_2": true,
	"Sequential_Tasks_3": true,
	"Sequential_Tasks_4": true,
	"Sequential_Tasks_5": true,
	"Sequential_Tasks_6": true,
	"Sequential_Tasks_7": true,
}

func isMPTaskCode(code string) bool { return mpTaskCodes[strings.TrimSpace(code)] }

// taskManualNotes explains, honestly, why a chore on the board has no runner.
// An empty answer means "no runner ported, and we have nothing more specific to
// say than that".
var taskManualNotes = map[string]string{
	"skill_1": "Needs a real Skill tool call inside the client; the reference never cracked it, so there is no runner.",
	"Expert_lighthouse": "Needs a real connector authorisation the user must grant in the client; " +
		"deliberately not automated.",
	"Expert_Philanthropy": "Spends real money (a donation); deliberately not automated.",
}

const taskNoRunnerNote = "No runner is ported for this chore, so it has to be completed in the client."

// --- Tasks ------------------------------------------------------------------

// Tasks implements core.TaskProvider.  It merges the default board with the
// miniprogram board, de-duplicated by task_code, and marks a chore Auto only
// when this module really has a runner for it.
//
// A client with no usable account reports an empty board and no error: "nothing
// to show" is not a failure.  A named-but-unknown (or parked) account is a real
// error, because that one cannot be shown at all.
func (c *Client) Tasks(ctx context.Context, accountID string) ([]core.TaskInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c == nil {
		return []core.TaskInfo{}, nil
	}
	c.refreshAccounts(false)

	a, err := c.taskAccount(accountID)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return []core.TaskInfo{}, nil
	}

	rows, err := c.mergedTaskRows(ctx, a)
	if err != nil {
		return nil, err
	}
	out := make([]core.TaskInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, taskInfo(r))
	}
	return out, nil
}

// taskAccount resolves which account a task call should use.  An empty
// accountID means "whichever account you would use anyway"; (nil, nil) means
// nothing usable is configured.
func (c *Client) taskAccount(accountID string) (*Auth, error) {
	id := strings.TrimSpace(accountID)
	if id != "" {
		if a := c.findAuth(id); a != nil {
			return a, nil
		}
		if _, parked := c.findDisabled(id); parked {
			return nil, fmt.Errorf("the account is parked; enable it before reading its chore board")
		}
		return nil, fmt.Errorf("account %q not found", id)
	}
	if c.pool == nil || !c.pool.Ready() {
		return nil, nil
	}
	if a, ok := c.pool.Pick(nil); ok {
		return a, nil
	}
	return nil, nil
}

// taskInfo maps one board row onto the frozen core shape.
func taskInfo(r taskRow) core.TaskInfo {
	t := r.task
	info := core.TaskInfo{
		Code:      t.TaskCode,
		Title:     firstNonEmpty(strings.TrimSpace(t.Title), t.TaskCode),
		Desc:      firstNonEmpty(strings.TrimSpace(t.Description), strings.TrimSpace(t.TaskDesc)),
		Group:     taskGroup(r),
		Credit:    t.RewardCredit,
		Energy:    t.RewardEnergy,
		Current:   t.Current,
		Target:    t.Target,
		Claimable: t.claimable(),
		Claimed:   t.claimed(),
		Locked:    t.Locked,
	}
	if _, ok := taskRunnerFor(t.TaskCode); ok {
		info.Auto = true
		return info
	}
	info.Auto = false
	info.Note = firstNonEmpty(taskManualNotes[t.TaskCode], taskNoRunnerNote)
	return info
}

// taskGroup picks the section the panel files a chore under.  A miniprogram
// chore is grouped by its channel even when the vendor also tags it, because
// that is the channel an operator has to look at to understand it; an ordinary
// chore keeps whatever the vendor called it.
func taskGroup(r taskRow) string {
	if r.mp {
		return "miniprogram"
	}
	if g := strings.TrimSpace(r.task.Tag); g != "" {
		return g
	}
	if g := strings.TrimSpace(r.task.TaskType); g != "" {
		return g
	}
	return "growth"
}

// mergedTaskRows reads both channels and folds them together, tolerating an
// unreachable miniprogram board (the default board alone is still useful).
func (c *Client) mergedTaskRows(ctx context.Context, a *Auth) ([]taskRow, error) {
	def, err := c.taskRowsFor(ctx, a, false)
	if err != nil {
		return nil, err
	}
	mp, mErr := c.taskRowsFor(ctx, a, true)
	if mErr != nil {
		c.deps.Logf("workbuddy: miniprogram task board unavailable: %v", mErr)
	}
	return mergeTaskRows(def, mp), nil
}

// mergeTaskRows de-duplicates the two boards by task_code.  The miniprogram
// board wins for the codes it owns, because that is the channel the vendor
// scores them on; for everything else the (richer) web row wins.
func mergeTaskRows(def, mp []taskRow) []taskRow {
	byCode := make(map[string]taskRow, len(def)+len(mp))
	order := make([]string, 0, len(def)+len(mp))
	put := func(r taskRow, replace bool) {
		code := r.task.TaskCode
		if _, seen := byCode[code]; seen {
			if replace {
				byCode[code] = r
			}
			return
		}
		byCode[code] = r
		order = append(order, code)
	}
	for _, r := range def {
		put(r, false)
	}
	for _, r := range mp {
		put(r, isMPTaskCode(r.task.TaskCode))
	}
	out := make([]taskRow, 0, len(order))
	for _, code := range order {
		out = append(out, byCode[code])
	}
	return out
}

// --- board cache ------------------------------------------------------------
//
// The cache is keyed by the client instance as well as the account, so two
// Clients (or two tests) never see each other's board.  Expired entries are
// swept on every write, so the map cannot grow without bound.

type taskCacheKey struct {
	client *Client
	acct   string
	mp     bool
}

type taskCacheEntry struct {
	rows []taskRow
	at   time.Time
}

var taskListCache = struct {
	mu sync.Mutex
	m  map[taskCacheKey]taskCacheEntry
}{m: make(map[taskCacheKey]taskCacheEntry)}

// taskRowsFor reads one channel, serving a cached copy for taskCacheTTL.
func (c *Client) taskRowsFor(ctx context.Context, a *Auth, mp bool) ([]taskRow, error) {
	key := taskCacheKey{client: c, acct: a.ID(), mp: mp}
	now := time.Now()

	taskListCache.mu.Lock()
	if e, ok := taskListCache.m[key]; ok && now.Sub(e.at) < taskCacheTTL {
		taskListCache.mu.Unlock()
		return e.rows, nil
	}
	taskListCache.mu.Unlock()

	rows, err := c.listGrowthTasks(ctx, a, mp)
	if err != nil {
		return nil, err
	}

	taskListCache.mu.Lock()
	if taskListCache.m == nil {
		taskListCache.m = make(map[taskCacheKey]taskCacheEntry)
	}
	for k, e := range taskListCache.m {
		if now.Sub(e.at) >= taskCacheTTL {
			delete(taskListCache.m, k)
		}
	}
	taskListCache.m[key] = taskCacheEntry{rows: rows, at: now}
	taskListCache.mu.Unlock()
	return rows, nil
}

// invalidateTaskCache drops both channels for one account so a run observes
// fresh progress instead of the stale row it started from.
func (c *Client) invalidateTaskCache(a *Auth) {
	taskListCache.mu.Lock()
	defer taskListCache.mu.Unlock()
	for _, mp := range []bool{false, true} {
		delete(taskListCache.m, taskCacheKey{client: c, acct: a.ID(), mp: mp})
	}
}

// listGrowthTasks reads one board straight from the vendor.
func (c *Client) listGrowthTasks(ctx context.Context, a *Auth, mp bool) ([]taskRow, error) {
	env, err := c.growthJSON(ctx, a, http.MethodGet, tasksListPath, nil, mp)
	if err != nil {
		return nil, err
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("the task board was refused: code=%d %s",
			env.Code, truncate(strings.TrimSpace(env.Msg), 120))
	}
	return parseGrowthTasks(env.Data, mp)
}

// parseGrowthTasks reads data.tasks[] and folds in the optional progress field.
func parseGrowthTasks(data json.RawMessage, mp bool) ([]taskRow, error) {
	var payload struct {
		Tasks []growthTask `json:"tasks"`
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &payload); err != nil {
			return nil, fmt.Errorf("parse the task board: %w", err)
		}
	}
	out := make([]taskRow, 0, len(payload.Tasks))
	for _, t := range payload.Tasks {
		t.applyProgress()
		if strings.TrimSpace(t.TaskCode) == "" {
			continue
		}
		out = append(out, taskRow{task: t, mp: mp})
	}
	return out, nil
}

// fetchTask reads one row fresh from one channel, or (nil, nil) when the
// channel does not carry that code.
func (c *Client) fetchTask(ctx context.Context, a *Auth, code string, mp bool) (*growthTask, error) {
	rows, err := c.listGrowthTasks(ctx, a, mp)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].task.TaskCode == code {
			t := rows[i].task
			return &t, nil
		}
	}
	return nil, nil
}

// readTask reads one row from the channel that owns the code, falling back to
// the other channel so a chore the vendor moved between boards still runs.
func (c *Client) readTask(ctx context.Context, a *Auth, code string, mp bool) (*growthTask, error) {
	t, err := c.fetchTask(ctx, a, code, mp)
	switch {
	case err == nil && t != nil:
		return t, nil
	case err != nil && !mp:
		return nil, err
	case err != nil:
		c.deps.Logf("workbuddy: miniprogram read of %s failed, trying the web board: %v", code, err)
	}
	return c.fetchTask(ctx, a, code, !mp)
}

// waitClaimable polls the board until the asynchronous scorer has caught up.
// An immediate 0/1 re-read is NOT evidence of failure: the reference measured a
// 5-8 second lag.
func (c *Client) waitClaimable(ctx context.Context, a *Auth, code string, mp bool) *growthTask {
	for i := 0; i < claimPollTries; i++ {
		t, err := c.readTask(ctx, a, code, mp)
		if err != nil {
			c.deps.Logf("workbuddy: re-reading %s failed: %v", code, err)
			return nil
		}
		if t == nil || t.claimable() || t.claimed() {
			return t
		}
		if i == claimPollTries-1 {
			return t
		}
		if !sleepCtx(ctx, claimPollGap) {
			return t
		}
	}
	return nil
}

// --- HTTP helpers -----------------------------------------------------------

// taskJSON performs one JSON API call and returns the gateway envelope without
// judging its business code, mirroring postJSON in checkin.go but for any verb
// (the board is a GET).
func (c *Client) taskJSON(ctx context.Context, method, endpoint string, body []byte, decorate func(*http.Request)) (apiEnvelope, error) {
	var env apiEnvelope
	hc := c.up.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	cctx, cancel := context.WithTimeout(ctx, taskTimeout)
	defer cancel()

	var rdr io.Reader
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(cctx, method, endpoint, rdr)
	if err != nil {
		return env, err
	}
	if decorate != nil {
		decorate(req)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return env, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	if err != nil {
		return env, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return env, &Error{
			Kind:   Classify(resp.StatusCode, string(raw)),
			Status: resp.StatusCode,
			Msg:    truncate(string(raw), 200),
		}
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return env, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 200))
	}
	return env, nil
}

// growthJSON calls a growth-centre route on the chat host with the billing
// identity, optionally declaring the miniprogram client.
func (c *Client) growthJSON(ctx context.Context, a *Auth, method, path string, body []byte, mp bool) (apiEnvelope, error) {
	return c.taskJSON(ctx, method, c.up.chatBase(a)+path, body, func(req *http.Request) {
		c.up.BillingHeaders(req, a)
		if mp {
			req.Header.Set(mpPlatformHeader, mpPlatformValue)
		}
	})
}

// growthPost marshals payload and posts it to a growth route.
func (c *Client) growthPost(ctx context.Context, a *Auth, path string, payload any, mp bool) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	env, err := c.growthJSON(ctx, a, http.MethodPost, path, body, mp)
	if err != nil {
		return err
	}
	if env.Code != 0 {
		return fmt.Errorf("the upstream refused %s: code=%d %s",
			path, env.Code, truncate(strings.TrimSpace(env.Msg), 120))
	}
	return nil
}

// acceptTasks 报名s a chore.  Accepting is not progress — it only makes the
// board willing to score the chore — so a failure here is logged, never fatal.
func (c *Client) acceptTasks(ctx context.Context, a *Auth, codes []string, mp bool) error {
	if len(codes) == 0 {
		return nil
	}
	body, err := json.Marshal(map[string]any{"task_codes": codes})
	if err != nil {
		return err
	}
	env, err := c.growthJSON(ctx, a, http.MethodPost, tasksAcceptPath, body, mp)
	if err != nil {
		return err
	}
	if env.Code != 0 {
		return fmt.Errorf("accepting %s was refused: code=%d %s",
			strings.Join(codes, ","), env.Code, truncate(strings.TrimSpace(env.Msg), 120))
	}
	return nil
}

// taskClaim is the claim response body.  already_claimed is a success with no
// credit, which is exactly how the vendor reports a double claim.
type taskClaim struct {
	Credit         int64 `json:"credit"`
	Energy         int64 `json:"energy"`
	AlreadyClaimed bool  `json:"already_claimed"`
}

// claimReward takes the reward for a finished chore.  The miniprogram channel
// claims on the chat host; the web channel claims on the browser host and needs
// the browser identity instead of the CLI one.
func (c *Client) claimReward(ctx context.Context, a *Auth, code string, mp bool) (taskClaim, error) {
	path := fmt.Sprintf(tasksClaimFmt, url.PathEscape(code))
	base := c.up.webBase(a)
	decorate := func(req *http.Request) { c.webClaimHeaders(req, a) }
	if mp {
		base = c.up.chatBase(a)
		decorate = func(req *http.Request) {
			c.up.BillingHeaders(req, a)
			req.Header.Set(mpPlatformHeader, mpPlatformValue)
		}
	}
	env, err := c.taskJSON(ctx, http.MethodPost, base+path, nil, decorate)
	if err != nil {
		// The miniprogram route answers 400 for a chore it does not own; the web
		// route is the right channel then.
		var ue *Error
		if mp && asError(err, &ue) && ue.Status == http.StatusBadRequest {
			return c.claimReward(ctx, a, code, false)
		}
		return taskClaim{}, err
	}
	if env.Code != 0 {
		return taskClaim{}, fmt.Errorf("claiming %s was refused: code=%d %s",
			code, env.Code, truncate(strings.TrimSpace(env.Msg), 120))
	}
	var claim taskClaim
	if len(env.Data) > 0 {
		_ = json.Unmarshal(env.Data, &claim)
	}
	return claim, nil
}

// webClaimHeaders mirrors the browser profile the growth-centre page uses.
// The claim route rejects the CLI identity.
func (c *Client) webClaimHeaders(req *http.Request, a *Auth) {
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-client-platform", "web")
	req.Header.Set("User-Agent", c.up.userAgent(a))
	if uid := a.UIDValue(); uid != "" {
		req.Header.Set("X-User-Id", uid)
	}
	if eid := a.EnterpriseIDValue(); eid != "" {
		req.Header.Set("X-Enterprise-Id", eid)
		req.Header.Set("X-Tenant-Id", eid)
	}
	if d := a.DomainValue(); d != "" {
		req.Header.Set("X-Domain", d)
	}
	origin := c.up.webBase(a)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/profile/growth-center")
}

// reportArray posts an activity-report array to one endpoint.
func (c *Client) reportArray(ctx context.Context, endpoint string, events []map[string]any, decorate func(*http.Request)) error {
	rows := make([]any, 0, len(events))
	for _, ev := range events {
		rows = append(rows, ev)
	}
	body, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	env, err := c.taskJSON(ctx, http.MethodPost, endpoint, body, decorate)
	if err != nil {
		return err
	}
	if env.Code != 0 {
		return fmt.Errorf("the activity report was refused: code=%d %s",
			env.Code, truncate(strings.TrimSpace(env.Msg), 120))
	}
	return nil
}

// --- runners ----------------------------------------------------------------

// taskRefusal marks a vendor-side refusal (a prerequisite that has not been
// credited, an anti-cheat rollback).  RunTask turns it into TaskResult{OK:false}
// rather than a Go error, because a refusal is a result.
type taskRefusal struct{ msg string }

func (e *taskRefusal) Error() string { return e.msg }

func refuse(format string, args ...any) error {
	return &taskRefusal{msg: fmt.Sprintf(format, args...)}
}

// taskRunner is one ported chore.
type taskRunner struct {
	code string
	desc string
	// mp marks the runners that live on the miniprogram channel.
	mp  bool
	run func(ctx context.Context, c *Client, a *Auth, t *growthTask) (string, error)
}

// taskRunners is the dispatch table.  Order is dependency order, matching the
// reference: first_buddy must not be attempted before chat_5 has been credited.
var taskRunners = []taskRunner{
	{code: "chat_5", desc: "CLI chat activity reports", run: runChat5},
	{code: "first_buddy", desc: "adopt the buddy", run: runFirstBuddy},
	{code: "Model_chat_GLM5.2", desc: "a real GLM-5.2 chat plus its report", run: runModelChat},
	{code: "RichMeow_Chat", desc: "desktop chat event sequence", run: runRichMeow},
	{code: "Buddy_App", desc: "buddyapp desktop event sequence", run: runBuddyApp},
	{code: "Buddy_App_QQ", desc: "buddyapp desktop event sequence", run: runBuddyApp},
	{code: "automation_1", desc: "desktop automation-create event", run: runAutomationCreate},
	{code: "Library_read", desc: "web element click on a资料库 document", run: runLibraryRead},
	{code: "template_5", desc: "five desktop template-use sequences", run: runTemplateUse},
	{code: "playbook_prompt", desc: "desktop playbook prompt sequence", run: runPlaybookPrompt},
	{code: "create_canvas", desc: "desktop design-canvas sequence", run: runCreateCanvas},
	{code: "expert_5", desc: "summon and really use five live platform experts", run: runExpertUse},
	{code: "Expert_team_use_3", desc: "summon and really use three live expert teams", run: runExpertTeamUse},
	{code: "Hp_Appearance", desc: "apply the 和平精英 theme", run: runAppearance},
	{code: "black_cat", desc: "night chats (23:00-08:00 only)", run: runBlackCat},
	{code: "school_season", desc: "miniprogram school-season chat events", mp: true, run: runSchoolSeason},
	{code: "Sequential_Tasks_1", desc: "miniprogram sequential chat events", mp: true, run: runSequential1},
	{code: "Sequential_Tasks_3", desc: "miniprogram sequential chat x5", mp: true, run: runSequential1},
	{code: "Sequential_Tasks_4", desc: "miniprogram scheduled-task creation (provisional criteria)", mp: true, run: runSequential4},
	{code: "Sequential_Tasks_6", desc: "miniprogram sequential chat x10", mp: true, run: runSequential1},
	{code: "Sequential_Tasks_2", desc: "miniprogram expert-usage event (live market id)", mp: true, run: runMPExpertUse},
	{code: "Sequential_Tasks_5", desc: "miniprogram chat declaring GLM-5.2", mp: true, run: runMPChatModel},
	{code: "Sequential_Tasks_7", desc: "miniprogram playbook CTA + prompt events", mp: true, run: runMPPlaybook},
	{code: "expert_actual_use", desc: "real expert chat for the server requestId, then the summon + actual-use events", run: runExpertActualUse},
}

var taskRunnerByCode = func() map[string]*taskRunner {
	m := make(map[string]*taskRunner, len(taskRunners))
	for i := range taskRunners {
		m[taskRunners[i].code] = &taskRunners[i]
	}
	return m
}()

func taskRunnerFor(code string) (*taskRunner, bool) {
	r, ok := taskRunnerByCode[strings.TrimSpace(code)]
	return r, ok
}

// RunTask implements core.TaskProvider.  It dispatches one chore to its runner
// and, when the chore is finished, claims the reward.
//
// Error policy, mirroring Checkin:
//   - an unknown code, an unknown account, or no usable account -> Go error
//   - a vendor refusal, a transport failure, or a chore the account does not
//     have -> TaskResult with OK=false (a result, not an error)
//   - already claimed -> TaskResult with OK=true (an idempotent no-op)
func (c *Client) RunTask(ctx context.Context, accountID, code string) (res core.TaskResult, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	trimmed := strings.TrimSpace(code)
	res = core.TaskResult{
		Code:      trimmed,
		AccountID: strings.TrimSpace(accountID),
		At:        time.Now().UTC().Format(time.RFC3339),
	}
	if c == nil {
		return res, errors.New("workbuddy: no client")
	}
	runner, ok := taskRunnerFor(trimmed)
	if !ok {
		// A scheduled chore (batches.go) has neither a board runner nor a
		// board row: the official client calls that endpoint on a timetable.
		if _, isChore := batchChoreFor(trimmed); !isChore {
			return res, fmt.Errorf("workbuddy: task %q has no runner; it needs in-client interaction", trimmed)
		}
	}

	c.refreshAccounts(false)
	a, aerr := c.taskAccount(accountID)
	if aerr != nil {
		return res, aerr
	}
	if a == nil {
		return res, errors.New("workbuddy: no usable account is configured")
	}
	res.AccountID = a.ID()

	start := time.Now()
	defer func() { res.ElapsedMS = time.Since(start).Milliseconds() }()

	// A scheduled chore has no row to read and no reward to claim, so it is
	// dispatched before readTask -- which would otherwise refuse it with "this
	// account's board has no task" and the chore would never reach the vendor.
	if chore, isChore := batchChoreFor(trimmed); isChore {
		return chore(ctx, c, a, res), nil
	}

	mp := runner.mp || isMPTaskCode(trimmed)

	// Read the row before running: an already-claimed chore is a no-op, and a
	// chore this account simply does not have must not look like a failed run.
	before, rerr := c.readTask(ctx, a, trimmed, mp)
	if rerr != nil {
		// The attempt could not be made at all, which §6 of the module contract
		// keeps as a real error rather than a result.
		c.pool.MarkFailure(a, rerr)
		res.Error = core.Redact(describeFailure(rerr))
		return res, rerr
	}
	if before == nil {
		res.OK = false
		res.Error = fmt.Sprintf("this account's board has no task %q", trimmed)
		return res, nil
	}
	if before.claimed() {
		res.OK = true
		res.Credit, res.Energy = before.RewardCredit, before.RewardEnergy
		res.Message = "already claimed; nothing to do"
		return res, nil
	}

	// Run unconditionally, the way the reference does.  Every runner is a no-op
	// once its own progress is met, and a miniprogram chore still has to send
	// its accept call before the reward can be claimed.
	note, err := runner.run(ctx, c, a, before)
	if err != nil {
		var ref *taskRefusal
		if errors.As(err, &ref) {
			// A vendor refusal is a result, not an error (§6).  Nothing was
			// claimed, so the reward fields stay zero.
			res.OK = false
			res.Error = core.Redact(ref.msg)
			return res, nil
		}
		c.pool.MarkFailure(a, err)
		res.Error = core.Redact(describeFailure(err))
		return res, nil
	}

	// The run reported events; drop the cached board so the re-read can see the
	// scorer's answer.
	c.invalidateTaskCache(a)
	after := c.waitClaimable(ctx, a, trimmed, mp)

	switch {
	case after != nil && after.claimed():
		res.OK = true
		res.Message = firstNonEmpty(note, "already claimed")
		res.Credit, res.Energy = after.RewardCredit, after.RewardEnergy
		return res, nil

	case after != nil && after.claimable():
		claim, cerr := c.claimReward(ctx, a, trimmed, mp)
		res.OK = true
		if cerr != nil {
			// The chore was completed; a failed claim does not undo that, and the
			// reference keeps it out of the success path too.
			res.Message = firstNonEmpty(note, "the chore is complete") +
				"; the reward could not be claimed: " + core.Redact(describeFailure(cerr))
			return res, nil
		}
		res.Credit, res.Energy = claim.Credit, claim.Energy
		if claim.AlreadyClaimed {
			res.Message = firstNonEmpty(note, "the reward was already claimed")
			return res, nil
		}
		res.Message = strings.TrimSpace(firstNonEmpty(note, "the chore is complete") +
			fmt.Sprintf("; claimed %d credit and %d energy", claim.Credit, claim.Energy))
		return res, nil

	case after != nil:
		// Events were reported but the target is not credited yet.  Say so
		// plainly instead of calling it a failure: scoring is asynchronous.
		c.pool.MarkSuccess(a)
		res.OK = true
		res.Message = taskProgressTail(note, after)
		return res, nil

	default:
		c.pool.MarkSuccess(a)
		res.OK = true
		res.Message = firstNonEmpty(note, "the events were reported") +
			"; the board could not be re-read to confirm the progress"
		return res, nil
	}
}

// taskProgressTail explains an uncredited-but-reported run.
func taskProgressTail(note string, t *growthTask) string {
	head := firstNonEmpty(note, "the events were reported")
	if t.Target <= 0 {
		return head + "; the vendor has not credited the chore yet"
	}
	return fmt.Sprintf("%s; progress is %d/%d — the vendor scores asynchronously, re-check the board in a few seconds",
		head, t.Current, t.Target)
}

// --- plain CLI chat activity ------------------------------------------------

// reportChatActivity posts one `chat_request_send` activity event on the CLI
// fingerprint.  The full field set is mandatory: the vendor answers 200 and
// silently drops an event that omits userId.
func (c *Client) reportChatActivity(ctx context.Context, a *Auth, conversationID, requestID, modelID, modelName string) error {
	conv := strings.TrimSpace(conversationID)
	if conv == "" {
		conv = "wb2api-task-" + strconv.FormatInt(time.Now().UnixMilli(), 10)
	}
	reqID := strings.TrimSpace(requestID)
	if reqID == "" {
		reqID = conv
	}
	if strings.TrimSpace(modelID) == "" {
		modelID = defaultChatModelID
	}
	if strings.TrimSpace(modelName) == "" {
		modelName = modelID
		if modelID == defaultChatModelID {
			modelName = defaultChatModelName
		}
	}
	now := time.Now().UnixMilli()
	ev := map[string]any{
		"eventCode":             "chat_request_send",
		"timestamp":             now,
		"presentAt":             now,
		"reportDelay":           0,
		"mode":                  "craft",
		"conversationId":        conv,
		"requestId":             reqID,
		"inputLength":           12,
		"requestModelId":        modelID,
		"requestModelName":      modelName,
		"isPlan":                false,
		"isAutoExecuteTerminal": false,
		"isAutoModify":          false,
		"codebaseEnable":        false,
		"maxToken":              0,
		"maxSteps":              0,
		"temperature":           0,
		"maxRetries":            0,
		"mentionContexts":       []any{},
		"knowledgeId":           "",
		"knowledgeName":         "",
		"codebaseId":            "",
		"mentionContextCount":   0,
		"command":               "",
		"expertId":              "",
		"recommendId":           "",
		"skillId":               "",
		"skillCount":            0,
		"totalCount":            0,
		"fileUri":               "",
		"traceId":               "",
		"rootRequestId":         reqID,
		"parentConversationId":  conv,
		"agentName":             "default",
		"agentType":             "conversation",
		"userId":                a.UIDValue(),
	}
	return c.reportArray(ctx, c.up.billingBase(a)+reportPath, []map[string]any{ev}, func(req *http.Request) {
		c.up.BillingHeaders(req, a)
	})
}

// runChat5 reports the five (or the board's target) CLI chat events that credit
// the daily chat chore.
func runChat5(ctx context.Context, c *Client, a *Auth, t *growthTask) (string, error) {
	target := t.Target
	if target <= 0 {
		target = 5
	}
	need := target - t.Current
	if need <= 0 {
		return "progress is already complete", nil
	}
	if need > taskMaxEvents {
		need = taskMaxEvents
	}
	for i := int64(0); i < need; i++ {
		conv := fmt.Sprintf("wb2api-chat5-%d-%d", time.Now().UnixMilli(), i)
		if err := c.reportChatActivity(ctx, a, conv, "", "", ""); err != nil {
			return "", err
		}
		if i < need-1 && !sleepCtx(ctx, reportGap) {
			return "", ctx.Err()
		}
	}
	return "", nil
}

// runFirstBuddy adopts the buddy.  The adoption gate only opens after the
// account has been seen chatting on the CLI fingerprint, so the prerequisite
// report comes first — and a gate that is still shut is reported as a result,
// not retried.
func runFirstBuddy(ctx context.Context, c *Client, a *Auth, t *growthTask) (string, error) {
	conv := "wb2api-adopt-" + strconv.FormatInt(time.Now().UnixMilli(), 10)
	if err := c.reportChatActivity(ctx, a, conv, "", "", ""); err != nil {
		return "", err
	}
	if !sleepCtx(ctx, reportGap) {
		return "", ctx.Err()
	}
	if err := c.growthPost(ctx, a, "/activity/growth/buddy/agreement", map[string]any{"agree": true}, false); err != nil {
		return "", err
	}
	if err := c.growthPost(ctx, a, "/activity/growth/buddy/first", map[string]any{}, false); err != nil {
		if buddyGateClosed(err) {
			// The vendor is refusing, not failing: the prerequisite event has
			// been reported, but the gate has not opened yet.  Hand it back as a
			// refusal so the caller reports OK:false instead of a success with a
			// reward nobody received.
			return "", refuse("the adoption gate has not opened yet; the prerequisite event was " +
				"reported, but the vendor has not credited it — re-run tomorrow rather than today")
		}
		return "", err
	}
	return "buddy adopted", nil
}

// buddyGateClosed recognises the vendor's "not yet" answer to BuddyFirst.
func buddyGateClosed(err error) bool {
	if err == nil {
		return false
	}
	var ue *Error
	if asError(err, &ue) {
		return strings.Contains(strings.ToLower(ue.Msg), buddyGateMarker)
	}
	return strings.Contains(strings.ToLower(err.Error()), buddyGateMarker)
}

// runModelChat is Model_chat_GLM5.2: 报名 the chore, run one real GLM-5.2 chat
// (the event alone is not enough for this one), then report it.
func runModelChat(ctx context.Context, c *Client, a *Auth, t *growthTask) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model": "glm-5.2",
		"messages": []map[string]string{
			{"role": "user", "content": "hi，请回复一句话"},
		},
		"stream": true,
	})
	if err != nil {
		return "", err
	}
	if err := c.acceptTasks(ctx, a, []string{t.TaskCode}, false); err != nil {
		c.deps.Logf("workbuddy: accepting %s failed: %v", t.TaskCode, err)
	}
	if !sleepCtx(ctx, reportGap) {
		return "", ctx.Err()
	}

	conv := "wb2api-conv-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	rc, status, raw, err := c.up.ChatStream(ctx, a, body, "", ChatMeta{ConversationID: conv, ConversationRequestID: conv})
	if err != nil {
		return "", err
	}
	if status >= 400 {
		if rc != nil {
			rc.Close()
		}
		return "", fmt.Errorf("the chat was refused: http %d %s", status, truncate(string(raw), 200))
	}
	if rc != nil {
		_, cerr := io.Copy(io.Discard, io.LimitReader(rc, 1<<20))
		rc.Close()
		if cerr != nil {
			return "", cerr
		}
	}
	if !sleepCtx(ctx, reportGap) {
		return "", ctx.Err()
	}
	conv = "wb2api-glm52-" + strconv.FormatInt(time.Now().UnixMilli(), 10)
	if err := c.reportChatActivity(ctx, a, conv, "", "glm-5.2", "GLM-5.2"); err != nil {
		return "", err
	}
	return "GLM-5.2 chat reported", nil
}

// runBlackCat does the night-chat chore.  Nothing can be credited outside the
// 23:00-08:00 window, so that is reported rather than forced.
func runBlackCat(ctx context.Context, c *Client, a *Auth, t *growthTask) (string, error) {
	if !inNightWindow(time.Now()) {
		return "outside the 23:00-08:00 counting window; nothing was reported", nil
	}
	need := t.Target - t.Current
	if t.Target <= 0 || need <= 0 {
		return "progress is already complete", nil
	}
	if need > taskMaxEvents {
		need = taskMaxEvents
	}
	for i := int64(0); i < need; i++ {
		body, err := json.Marshal(map[string]any{
			"model": "glm-5.2",
			"messages": []map[string]string{
				{"role": "user", "content": "1+1等于几？直接回答。"},
			},
			"stream": true,
		})
		if err != nil {
			return "", err
		}
		conv := fmt.Sprintf("wb2api-night-%d-%d", time.Now().UnixMilli(), i)
		rc, status, raw, err := c.up.ChatStream(ctx, a, body, "", ChatMeta{ConversationID: conv, ConversationRequestID: conv})
		if err != nil {
			return "", err
		}
		if status >= 400 {
			if rc != nil {
				rc.Close()
			}
			return "", fmt.Errorf("the night chat was refused: http %d %s", status, truncate(string(raw), 200))
		}
		if rc != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<20))
			rc.Close()
		}
		conv = fmt.Sprintf("wb2api-night-%d-%d", time.Now().UnixMilli(), i)
		if err := c.reportChatActivity(ctx, a, conv, "", "glm-5.2", "GLM-5.2"); err != nil {
			return "", err
		}
		if !sleepCtx(ctx, 4*time.Second) {
			return "", ctx.Err()
		}
	}
	return "night chats reported", nil
}

// inNightWindow mirrors the vendor's local counting window.
func inNightWindow(now time.Time) bool {
	h := now.Hour()
	return h >= 23 || h < 8
}

// --- desktop fingerprint ----------------------------------------------------

// desktopFingerprint is the desktop client's identity.  It is stable per
// account so one account always looks like one desktop install.
func desktopFingerprint(a *Auth) map[string]any {
	uid := a.UIDValue()
	nick := a.NicknameValue()
	now := time.Now().UnixMilli()
	return map[string]any{
		"timezone":     "Asia/Shanghai",
		"reportDelay":  fpReportDelay,
		"userId":       uid,
		"username":     nick,
		"userNickname": nick,
		"product":      "SaaS",
		"releaseDate":  fpReleaseDate,
		"commit":       desktopCommit,
		"ideName":      "WorkBuddy",
		"ideType":      "WorkBuddy",
		"ideVersion":   desktopVersion,
		"machineId":    deriveAccountStableID(uid, "machine"),
		"sessionId":    deriveAccountStableID(uid, "session"),
		"extName":      "workbuddy-desktop",
		"extVersion":   desktopVersion,
		"os":           "win32",
		"arch":         "x64",
		"osVersion":    "10.0.26220",
		"cpuCores":     20,
		"memorySize":   24,
		"timestamp":    now,
		"presentAt":    now,
	}
}

// reportDesktopEvent posts desktop-fingerprint events to the chat host.  The
// business keys win over the fingerprint when both name a field.
func (c *Client) reportDesktopEvent(ctx context.Context, a *Auth, events ...map[string]any) error {
	fp := desktopFingerprint(a)
	merged := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		row := make(map[string]any, len(fp)+len(ev))
		for k, v := range fp {
			row[k] = v
		}
		for k, v := range ev {
			row[k] = v
		}
		merged = append(merged, row)
	}
	base := c.up.chatBase(a)
	return c.reportArray(ctx, base+reportPath, merged, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("Content-Type", "application/json;charset=UTF-8")
		req.Header.Set("User-Agent", desktopUserAgent)
		req.Header.Set("X-Domain", base)
		req.Header.Set("X-Product", "SaaS")
		req.Header.Set("X-Request-ID", deriveAccountStableID(a.UIDValue(), "req")+
			strconv.FormatInt(time.Now().UnixNano()%1000000, 10))
		if uid := a.UIDValue(); uid != "" {
			req.Header.Set("X-User-Id", uid)
		}
	})
}

// desktopChatSequence is the six-event sequence the desktop client emits for
// one assistant exchange.  The chore judges read it as a real chat.
func desktopChatSequence(conversationID, requestID, messageID, modelID, modelName string) []map[string]any {
	now := time.Now().UnixMilli()
	assistantID := messageID + "-assistant"

	agentTask := map[string]any{
		"eventCode":          "agent_task_created",
		"source":             "LOCAL",
		"name":               "working",
		"task_target":        "local",
		"mode":               "craft",
		"requestModelId":     modelID,
		"requestModelName":   modelName,
		"has_repo":           false,
		"repo_type":          "none",
		"workspace_type":     "",
		"has_connector":      false,
		"connector_types":    []any{},
		"has_mention":        false,
		"mention_types":      []any{},
		"has_template":       false,
		"action":             "",
		"template_name":      "",
		"has_expert":         false,
		"expert_id":          "",
		"expert_name":        "",
		"expert_industry_id": "",
		"has_skill":          false,
		"skill_names":        []any{},
		"conversationId":     conversationID,
		"messageId":          messageID,
		"buddyId":            "",
		"buddyName":          "",
		"timestamp":          now,
	}
	messageSend := map[string]any{
		"eventCode":                         "chat_message_send",
		"conversationId":                    conversationID,
		"messageId":                         assistantID,
		"historyCount":                      0,
		"isContextTruncated":                false,
		"currentStepCount":                  1,
		"traceId":                           requestID,
		"rootRequestId":                     requestID,
		"parentConversationId":              conversationID,
		"agentName":                         "cli",
		"agentType":                         "main",
		"codebuddy.session_id":              conversationID,
		"codebuddy.conversation_request_id": requestID,
		"timestamp":                         now,
	}
	requestSend := map[string]any{
		"eventCode":                         "chat_request_send",
		"conversationId":                    conversationID,
		"inputLength":                       24,
		"isPlan":                            false,
		"isAutoExecuteTerminal":             false,
		"isAutoModify":                      false,
		"codebaseEnable":                    false,
		"maxToken":                          0,
		"maxSteps":                          500,
		"temperature":                       0,
		"maxRetries":                        0,
		"mentionContexts":                   []any{},
		"knowledgeId":                       "",
		"knowledgeName":                     "",
		"codebaseId":                        "",
		"mentionContextCount":               0,
		"command":                           "",
		"recommendId":                       "",
		"skillId":                           "",
		"skillCount":                        0,
		"totalCount":                        0,
		"traceId":                           requestID,
		"rootRequestId":                     requestID,
		"parentConversationId":              conversationID,
		"agentName":                         "cli",
		"agentType":                         "main",
		"codebuddy.session_id":              conversationID,
		"codebuddy.conversation_request_id": requestID,
		"timestamp":                         now,
	}
	messageResponse := map[string]any{
		"eventCode":                         "chat_message_response",
		"conversationId":                    conversationID,
		"messageId":                         assistantID,
		"responseModelId":                   modelID,
		"inputToken":                        120,
		"outputToken":                       80,
		"totalToken":                        200,
		"cachedTokens":                      0,
		"cachedWriteTokens":                 0,
		"cachedMissTokens":                  0,
		"isSuccessful":                      true,
		"messageErrorCode":                  "",
		"finishReason":                      "stop",
		"firstTokenAt":                      now,
		"traceId":                           requestID,
		"rootRequestId":                     requestID,
		"parentConversationId":              conversationID,
		"agentName":                         "cli",
		"agentType":                         "main",
		"codebuddy.session_id":              conversationID,
		"codebuddy.conversation_request_id": requestID,
		"timestamp":                         now,
	}
	messageStatus := map[string]any{
		"eventCode":                         "chat_message_status",
		"conversationId":                    conversationID,
		"messageId":                         assistantID,
		"messageErrorCode":                  "0",
		"traceId":                           requestID,
		"rootRequestId":                     requestID,
		"parentConversationId":              conversationID,
		"agentName":                         "cli",
		"agentType":                         "main",
		"codebuddy.session_id":              conversationID,
		"codebuddy.conversation_request_id": requestID,
		"timestamp":                         now,
	}
	requestResponse := map[string]any{
		"eventCode":                         "chat_request_response",
		"conversationId":                    conversationID,
		"mode":                              "craft",
		"toolCallCount":                     0,
		"inputToken":                        120,
		"outputToken":                       80,
		"totalToken":                        200,
		"cachedTokens":                      0,
		"cachedWriteTokens":                 0,
		"cachedMissTokens":                  0,
		"isSuccessful":                      true,
		"messageErrorCode":                  "",
		"finishReason":                      "stop",
		"rootRequestId":                     requestID,
		"parentConversationId":              conversationID,
		"agentName":                         "cli",
		"agentType":                         "main",
		"codebuddy.conversation_request_id": requestID,
		"timestamp":                         now,
	}
	return []map[string]any{agentTask, messageSend, requestSend, messageResponse, messageStatus, requestResponse}
}

// --- desktop runners --------------------------------------------------------

func runRichMeow(ctx context.Context, c *Client, a *Auth, t *growthTask) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb2api-richmeow-%d", ms)
	req := fmt.Sprintf("wb2api-richmeow-req-%d", ms)
	events := desktopChatSequence(conv, req, "msg-richmeow", taskModelID, taskModelID)
	if err := c.reportDesktopEvent(ctx, a, events...); err != nil {
		return "", err
	}
	return "desktop chat sequence reported", nil
}

func runBuddyApp(ctx context.Context, c *Client, a *Auth, t *growthTask) (string, error) {
	if err := c.reportDesktopEvent(ctx, a, desktopBuddyAppSequence(buddyAppID, buddyAppName)...); err != nil {
		return "", err
	}
	return "buddy app sequence reported", nil
}

// desktopBuddyAppSequence is the five-event buddyapp flow.
func desktopBuddyAppSequence(buddyID, buddyName string) []map[string]any {
	now := time.Now().UnixMilli()
	base := map[string]any{
		"buddyId":   buddyID,
		"buddyName": buddyName,
		"mode":      "LOCAL",
		"timestamp": now,
	}
	with := func(eventCode string, extra map[string]any) map[string]any {
		row := make(map[string]any, len(base)+len(extra)+1)
		for k, v := range base {
			row[k] = v
		}
		row["eventCode"] = eventCode
		for k, v := range extra {
			row[k] = v
		}
		return row
	}
	elem := func(id, name string) map[string]any {
		return map[string]any{"elementId": id, "elementName": name}
	}
	return []map[string]any{
		with("buddyapp_discover_click", nil),
		with("buddyapp_show", map[string]any{
			"elementId": "buddyapp_card", "elementName": buddyName, "position": 2,
		}),
		with("buddyapp_enter_click", map[string]any{
			"elementId": "buddyapp_enter", "elementName": buddyName, "position": 2, "isFirstPage": "1",
		}),
		with("buddyapp_auth_confirm_click", elem("buddyapp_auth_confirm", "授权")),
		with("buddyapp_bindaccount_skip_click", elem("buddyapp_bindaccount_skip", "跳过")),
	}
}

// desktopAutomationCreateEvent is the event that credits a "create a scheduled
// task" chore.  It is shared by the desktop chore (automation_1) and by the
// miniprogram Sequential_Tasks_4 chore: the reference's miniprogram source has
// no automation emission point of its own, so it points that chore at this same
// PC-side event.  Keeping one constructor means the two chores cannot drift.
func desktopAutomationCreateEvent() map[string]any {
	return map[string]any{
		"eventCode":       "automated_task_create_suc",
		"name":            "wb2api 自动化",
		"source":          "manually",
		"modelId":         taskModelID,
		"modelIsThinking": true,
		"connectorCount":  0,
		"skills":          "",
		"skillCount":      0,
		"scheduleType":    "once",
		"mode":            "LOCAL",
		"timestamp":       time.Now().UnixMilli(),
	}
}

func runAutomationCreate(ctx context.Context, c *Client, a *Auth, t *growthTask) (string, error) {
	if err := c.reportDesktopEvent(ctx, a, desktopAutomationCreateEvent()); err != nil {
		return "", err
	}
	return "automation create reported", nil
}

// taskTemplates is the five-template rotation the reference uses for
// template_5.
var taskTemplates = []struct{ id, name string }{
	{"1", "深度研究"},
	{"2", "周报生成"},
	{"3", "竞品分析"},
	{"4", "活动策划"},
	{"5", "代码评审"},
}

func runTemplateUse(ctx context.Context, c *Client, a *Auth, t *growthTask) (string, error) {
	ms := time.Now().UnixMilli()
	for i, tp := range taskTemplates {
		conv := fmt.Sprintf("wb2api-tpl-%d-%d", ms, i)
		req := fmt.Sprintf("wb2api-tpl-req-%d-%d", ms, i)
		if err := c.reportDesktopEvent(ctx, a, desktopTemplateUseSequence(conv, req, tp.id, tp.name)...); err != nil {
			return "", err
		}
		if i < len(taskTemplates)-1 && !sleepCtx(ctx, 300*time.Millisecond) {
			return "", ctx.Err()
		}
	}
	return "five template uses reported", nil
}

// desktopTemplateUseSequence is one chat sequence plus the two template events.
func desktopTemplateUseSequence(conv, req, templateID, templateName string) []map[string]any {
	events := desktopChatSequence(conv, req, "msg-"+templateID, taskModelID, taskModelID)
	return append(events,
		map[string]any{
			"eventCode":      "agent_task_created_with_template",
			"mode":           "working",
			"isCustomModel":  false,
			"id":             templateID,
			"name":           templateName,
			"requestId":      req,
			"conversationId": conv,
			"timestamp":      time.Now().UnixMilli(),
		},
		map[string]any{
			"eventCode":      "template_used",
			"template_id":    templateID,
			"task_mode":      "working",
			"id":             templateID,
			"name":           templateName,
			"conversationId": conv,
			"timestamp":      time.Now().UnixMilli(),
		},
	)
}

func runPlaybookPrompt(ctx context.Context, c *Client, a *Auth, t *growthTask) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb2api-pb-%d", ms)
	req := fmt.Sprintf("wb2api-pb-req-%d", ms)
	events := desktopPlaybookPromptSequence(conv, req, playbookCaseID, playbookCaseName)
	if err := c.reportDesktopEvent(ctx, a, events...); err != nil {
		return "", err
	}
	return "playbook prompt reported", nil
}

// desktopPlaybookPromptSequence is one chat sequence plus the three playbook
// events the chore counts.
func desktopPlaybookPromptSequence(conv, req, caseID, caseName string) []map[string]any {
	doc := map[string]any{
		"id":           caseID,
		"name":         caseName,
		"type":         "document",
		"categoryId":   "",
		"categoryName": "",
	}
	with := func(eventCode string, extra map[string]any) map[string]any {
		row := make(map[string]any, len(doc)+len(extra)+2)
		for k, v := range doc {
			row[k] = v
		}
		row["eventCode"] = eventCode
		row["timestamp"] = time.Now().UnixMilli()
		for k, v := range extra {
			row[k] = v
		}
		return row
	}
	events := desktopChatSequence(conv, req, "msg-pb", taskModelID, taskModelID)
	return append(events,
		with("web_element_click", map[string]any{
			"pageName": "playbook_detail", "elementId": "playbook_ctaClick",
			"elementName": caseName, "source": "discover",
		}),
		with("playbook_cta_click", map[string]any{"source": "discover", "position": 0}),
		with("playbook_prompt_send", map[string]any{"conversationId": conv, "requestId": req}),
	)
}

func runCreateCanvas(ctx context.Context, c *Client, a *Auth, t *growthTask) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb2api-canvas-%d", ms)
	req := fmt.Sprintf("wb2api-canvas-req-%d", ms)
	events := desktopDesignCanvasSequence(conv, req)
	if err := c.reportDesktopEvent(ctx, a, events...); err != nil {
		return "", err
	}
	return "design canvas reported", nil
}

// desktopDesignCanvasSequence is one chat sequence plus the two canvas events.
func desktopDesignCanvasSequence(conv, req string) []map[string]any {
	now := time.Now().UnixMilli()
	events := desktopChatSequence(conv, req, "msg-canvas", taskModelID, taskModelID)
	suffix := req
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	return append(events,
		map[string]any{
			"eventCode":      "wbx_design_canvas_task_create",
			"conversationId": conv,
			"requestId":      req,
			"source":         "summon_keyword",
			"cost":           12000,
			"isSuccessful":   true,
			"timestamp":      now,
		},
		map[string]any{
			"eventCode":      "wbx_design_canvas_open",
			"conversationId": conv,
			"requestId":      req,
			"id":             "ardot-file-" + suffix,
			"source":         "summon_keyword",
			"type":           "page",
			"cost":           13000,
			"isSuccessful":   true,
			"timestamp":      now,
		},
	)
}

func runAppearance(ctx context.Context, c *Client, a *Auth, t *growthTask) (string, error) {
	if err := c.setAppearanceTheme(ctx, a, appearanceThemeKey); err != nil {
		return "", err
	}
	if !sleepCtx(ctx, 2*time.Second) {
		return "", ctx.Err()
	}
	ev := map[string]any{
		"eventCode": "appearance_skin_apply",
		"action":    "apply",
		"source":    "settings_close",
		"id":        appearanceThemeKey,
		"vipLevel":  0,
		"series":    "",
		"type":      "unknown",
		"timestamp": time.Now().UnixMilli(),
	}
	if err := c.reportDesktopEvent(ctx, a, ev); err != nil {
		return "", err
	}
	return "theme applied", nil
}

// setAppearanceTheme switches the desktop skin; the apply event alone does not
// credit Hp_Appearance.
func (c *Client) setAppearanceTheme(ctx context.Context, a *Auth, key string) error {
	body, err := json.Marshal(map[string]any{"kind": "theme", "resource_key": key})
	if err != nil {
		return err
	}
	env, err := c.taskJSON(ctx, http.MethodPost, c.up.chatBase(a)+appearanceSetPth, body, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json;charset=UTF-8")
		req.Header.Set("User-Agent", desktopUserAgent)
		req.Header.Set("X-Product", "SaaS")
		if uid := a.UIDValue(); uid != "" {
			req.Header.Set("X-User-Id", uid)
		}
	})
	if err != nil {
		return err
	}
	if env.Code != 0 {
		return fmt.Errorf("applying the theme was refused: code=%d %s",
			env.Code, truncate(strings.TrimSpace(env.Msg), 120))
	}
	return nil
}

// --- web-fingerprint runner -------------------------------------------------

func runLibraryRead(ctx context.Context, c *Client, a *Auth, t *growthTask) (string, error) {
	if err := c.reportWebEvent(ctx, a, "web_element_click", libraryDocURL, libraryDocElem, libraryDocName); err != nil {
		return "", err
	}
	return "library document opened", nil
}

// reportWebEvent posts one browser-fingerprint event to the console host.
func (c *Client) reportWebEvent(ctx context.Context, a *Auth, eventCode, pageURL, elementID, elementName string) error {
	now := time.Now().UnixMilli()
	ev := map[string]any{
		"eventCode":    eventCode,
		"timestamp":    now,
		"reportDelay":  0,
		"pageURL":      pageURL,
		"elementId":    elementID,
		"elementName":  elementName,
		"os":           "Win32",
		"arch":         "",
		"osVersion":    "10.0",
		"userAgent":    webEventUserAgent,
		"machineId":    deriveAccountStableID(a.UIDValue(), "webmachine"),
		"userId":       a.UIDValue(),
		"userNickname": a.NicknameValue(),
		"enterpriseId": a.EnterpriseIDValue(),
	}
	origin := c.up.webBase(a)
	return c.reportArray(ctx, origin+reportPath, []map[string]any{ev}, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("x-client-platform", "web")
		req.Header.Set("Origin", origin)
		req.Header.Set("Referer", pageURL)
		req.Header.Set("User-Agent", webEventUserAgent)
		if uid := a.UIDValue(); uid != "" {
			req.Header.Set("X-User-Id", uid)
		}
	})
}

// webEventUserAgent is the browser the reference reports these events from.
const webEventUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"

// --- miniprogram runners ----------------------------------------------------

// runSchoolSeason is the school-season chore: the miniprogram chat event with
// the activityId that lights it up.
func runSchoolSeason(ctx context.Context, c *Client, a *Auth, t *growthTask) (string, error) {
	return runMPChatTask(ctx, c, a, t, true)
}

// runSequential1 covers the sequential chat chores (Sequential_Tasks_1 / _3 /
// _6).  They differ only in their target, which the board already reports.
func runSequential1(ctx context.Context, c *Client, a *Auth, t *growthTask) (string, error) {
	return runMPChatTask(ctx, c, a, t, false)
}

// runMPChatTask is the shared miniprogram loop.  It is the one place where the
// anti-abuse spacing matters most: the reference measured that firing these
// events at 2s intervals gets the entire batch rolled back, while ~45s of
// spacing with jitter survives and claims cleanly.
func runMPChatTask(ctx context.Context, c *Client, a *Auth, t *growthTask, withActivityID bool) (string, error) {
	if err := c.acceptTasks(ctx, a, []string{t.TaskCode}, true); err != nil {
		c.deps.Logf("workbuddy: accepting miniprogram task %s failed: %v", t.TaskCode, err)
	}
	if !sleepCtx(ctx, mpActionGap) {
		return "", ctx.Err()
	}

	// The accept call can answer 200 before the chore actually registers, so
	// re-read instead of trusting it.
	cur := t
	if settled, rerr := c.readTask(ctx, a, t.TaskCode, true); rerr == nil && settled != nil {
		cur = settled
	}
	if cur.claimed() {
		return "already claimed", nil
	}
	need := cur.Target - cur.Current
	if cur.Target <= 0 {
		need = 0
	}
	if need > taskMaxEvents {
		need = taskMaxEvents
	}
	for i := int64(0); i < need; i++ {
		// The first event waits too: the spacing is what keeps the batch from
		// being rolled back, so there is no "skip the sleep for i==0" shortcut.
		wait := mpChatEventGap
		if mpChatJitter > 0 {
			wait += time.Duration(rand.Int64N(int64(mpChatJitter)))
		}
		if !sleepCtx(ctx, wait) {
			return "", ctx.Err()
		}
		conv := fmt.Sprintf("wb2api-mp-%d-%d", time.Now().UnixMilli(), i)
		activity := ""
		if withActivityID {
			activity = mpSchoolActivityID
		}
		if err := c.reportMPEvent(ctx, a, mpChatEvent(conv, activity)); err != nil {
			return "", err
		}
	}
	return "miniprogram chat events reported", nil
}

// mpChatEvent builds one miniprogram chat_request_send event.
func mpChatEvent(conversationID, activityID string) map[string]any {
	rid := "wb2api-" + clientToken()
	ev := map[string]any{
		"eventCode":                         "chat_request_send",
		"inputLength":                       14,
		"isPlan":                            false,
		"isAutoExecuteTerminal":             false,
		"isAutoModify":                      false,
		"codebaseEnable":                    false,
		"maxToken":                          0,
		"maxSteps":                          500,
		"temperature":                       0,
		"maxRetries":                        0,
		"mentionContexts":                   []any{},
		"knowledgeId":                       "",
		"knowledgeName":                     "",
		"codebaseId":                        "",
		"mentionContextCount":               0,
		"command":                           "",
		"recommendId":                       "",
		"skillId":                           "",
		"skillCount":                        0,
		"totalCount":                        0,
		"traceId":                           rid,
		"rootRequestId":                     rid,
		"parentConversationId":              conversationID,
		"conversationId":                    conversationID,
		"messageId":                         "msg-" + tailOf(rid, 8),
		"agentName":                         "mp",
		"agentType":                         "main",
		"codebuddy.session_id":              conversationID,
		"codebuddy.conversation_request_id": rid,
		"timestamp":                         time.Now().UnixMilli(),
	}
	if activityID != "" {
		ev["activityId"] = activityID
	}
	return ev
}

// reportMPEvent posts miniprogram events with the miniprogram identity.  The
// fingerprint is merged first and the business keys win.
func (c *Client) reportMPEvent(ctx context.Context, a *Auth, events ...map[string]any) error {
	base := map[string]any{
		"timestamp":    time.Now().UnixMilli(),
		"ideType":      "WorkBuddy_MP",
		"ideVersion":   mpExtVersion,
		"extName":      "workbuddy-mp",
		"extVersion":   mpExtVersion,
		"product":      "SaaS",
		"ideName":      "wx_app_cloud",
		"platform":     "mini_program",
		"os":           "windows",
		"osVersion":    "11",
		"arch":         "x64",
		"machineId":    mpMachineID,
		"timezone":     "Asia/Shanghai",
		"userId":       a.UIDValue(),
		"userNickname": a.NicknameValue(),
	}
	merged := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		row := make(map[string]any, len(base)+len(ev))
		for k, v := range base {
			row[k] = v
		}
		for k, v := range ev {
			row[k] = v
		}
		merged = append(merged, row)
	}
	return c.reportArray(ctx, c.up.billingBase(a)+reportPath, merged, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		if uid := a.UIDValue(); uid != "" {
			req.Header.Set("X-User-Id", uid)
		}
		req.Header.Set("X-Client-Product", "workbuddy-mp")
		req.Header.Set("X-Client-Version", mpExtVersion)
		req.Header.Set("X-Client-Platform", "mp-weixin")
		req.Header.Set("X-Platform", "wechatmp")
	})
}

// --- small helpers ----------------------------------------------------------

// clientToken returns a random token in the vendor's dash-grouped hex shape
// (xxxx-xx-xx-xx-xxxxxx).  It reuses newMessageID so the entropy fallback is
// shared.
func clientToken() string {
	h := newMessageID()
	return h[0:4] + "-" + h[4:6] + "-" + h[6:8] + "-" + h[8:10] + "-" + h[10:16]
}

// tailOf returns the last n bytes of s.
func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// sleepCtx waits for d, reporting false when the context ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// compile-time proof that this module really satisfies the frozen interface.
var _ core.TaskProvider = (*Client)(nil)
