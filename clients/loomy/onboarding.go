package loomy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// This file implements core.TaskProvider over Loomy's 新手任务 board -- the
// eight one-off chores whose completion the vendor's desktop client reports
// through its onboarding service.
//
// WHY THE CHORES ARE SPELLED OUT HERE.  The vendor's service answers with
// completion flags and nothing else:
//
//	{"tasks":{"first_message":false, ...},"earned":0,"total":10000}
//
// It never sends a chore's title or its price, and the vendor's own client does
// not ask it to: that client keeps the registry locally, shows it, and
// recomputes `earned` from it rather than trusting the number the server
// returns.  A board built only from the response would therefore be a column of
// bare keys.  This module keeps the same registry, with the same wording and
// the same prices, and sums to the same 10000 the live service reports as
// `total`.
//
// WHAT RUNNING A CHORE DOES.  The completion endpoint is not a "do the chore"
// endpoint: it is how the vendor's client tells the backend that the human
// finished a chore, and the vendor's service documents it as idempotent.  The
// gateway posts that same call on demand, which is the only thing a headless
// gateway can do -- it cannot send your first message or create your first
// 搭子.  Every row that can be run says so in its note; nothing here pretends
// the chore itself was performed.
//
// VERIFIED, and what is not.  GET /api/v1/onboarding/tasks was called against a
// live session and answered {"code":"000000","desc":"成功","data":{"tasks":{the
// eight keys, all false},"earned":0,"total":10000}}.  POST .../complete was
// called with a key the registry does not contain and answered business code
// 100001, "未知的 task key" -- which is why an unknown code is refused locally
// here instead of being forwarded as a guess.  The POST's happy path was
// deliberately NOT run against the live account: completing a real chore would
// have spent the operator's onboarding reward to re-test a request whose shape
// the vendor's own client already pins exactly.

const (
	// onboardingTasksPath is the read half and onboardingCompletePath the write
	// half.  Both hang off apiBase(), the same host the credit ledger lives on:
	// the installed client's .env.prod points its points base URL at that host,
	// and its onboarding service then appends /api/v1/onboarding/tasks.
	onboardingTasksPath    = "/onboarding/tasks"
	onboardingCompletePath = "/onboarding/tasks/complete"

	// onboardingBoardTTL bounds how often the board re-reads upstream.  The
	// panel re-reads /tasks every few seconds while a run is in flight and this
	// is a network call, so a few seconds of staleness saves a lot of traffic.
	// A run drops the cache, so a completion is never stale.
	onboardingBoardTTL = 10 * time.Second
)

// onboardingRunNote is what the board says beside a chore it can report.
//
// It is deliberately blunt.  The vendor's client posts this completion after
// the human performed the chore; the gateway posts it on demand, and the note
// says so rather than implying the chore was performed.
const onboardingRunNote = "点「执行」= 代你向厂商上报该任务已完成（厂商自己的客户端也是在动作完成后发这一个请求）；" +
	"网关无法替你完成动作本身"

// onboardingChore is one entry of the vendor's own task registry.
type onboardingChore struct {
	Key    string
	Title  string
	Group  string
	Points int64
}

// onboardingRegistry is the vendor's registry: three groups, eight chores, in
// the order the official client lists them.  The points sum to 10000, which is
// the `total` the live service reports.
var onboardingRegistry = []onboardingChore{
	{Key: "first_message", Title: "发送你的第一条消息", Group: "初识 Loomy", Points: 500},
	{Key: "pick_skill", Title: "试试选择一个技能", Group: "初识 Loomy", Points: 1000},
	{Key: "generate_ppt", Title: "生成第一份 PPT", Group: "初识 Loomy", Points: 1500},
	{Key: "set_schedule", Title: "设置定时任务", Group: "打造你的专属 Loomy", Points: 1000},
	{Key: "install_skill", Title: "在技能广场安装一个技能", Group: "打造你的专属 Loomy", Points: 1500},
	{Key: "configure_remote", Title: "配置远程控制", Group: "打造你的专属 Loomy", Points: 1000},
	{Key: "create_soul", Title: "创建你的第一个搭子", Group: "遇见你的 AI 搭子", Points: 1500},
	{Key: "share_soul", Title: "把搭子分享给朋友", Group: "遇见你的 AI 搭子", Points: 2000},
}

// onboardingChoreFor looks a chore up.  RunTask refuses anything else, so a
// typo is answered by this module rather than forwarded to the vendor as a
// guess -- which is exactly what the vendor's own service does with an unknown
// key ("未知的 task key", business code 100001).
func onboardingChoreFor(key string) (onboardingChore, bool) {
	for _, ch := range onboardingRegistry {
		if ch.Key == key {
			return ch, true
		}
	}
	return onboardingChore{}, false
}

// onboardingTasks reads the vendor's completion flags.  A key the vendor omits
// is simply absent from the set.
func (u *upstream) onboardingTasks(ctx context.Context, token string) (map[string]bool, error) {
	raw, err := u.business(ctx, http.MethodGet, onboardingTasksPath, nil, token, nil)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Tasks map[string]bool `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("loomy: decoding onboarding tasks: %w", err)
	}
	if payload.Tasks == nil {
		payload.Tasks = map[string]bool{}
	}
	return payload.Tasks, nil
}

// onboardingComplete reports a chore as finished and returns the vendor's own
// `alreadyCompleted` flag plus whatever balance it reported.
//
// The vendor's client treats the call as idempotent and surfaces
// `alreadyCompleted` rather than an error, so a repeat is a success here too.
func (u *upstream) onboardingComplete(ctx context.Context, token, key string) (bool, *float64, error) {
	body, err := json.Marshal(map[string]string{"key": key})
	if err != nil {
		return false, nil, err
	}
	raw, err := u.business(ctx, http.MethodPost, onboardingCompletePath, nil, token, body)
	if err != nil {
		return false, nil, err
	}
	var payload struct {
		AlreadyCompleted bool     `json:"alreadyCompleted"`
		Balance          *float64 `json:"balance"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return false, nil, fmt.Errorf("loomy: decoding onboarding completion: %w", err)
	}
	return payload.AlreadyCompleted, payload.Balance, nil
}

// onboardingBoard is this module's short-lived copy of one account's completion
// flags.  Every field is guarded by mu and the zero value is ready to use.
type onboardingBoard struct {
	mu      sync.Mutex
	tasks   map[string]bool
	account string
	at      time.Time
}

// boardFor returns the vendor's completion flags for acc, re-reading upstream
// at most once per onboardingBoardTTL.
//
// The returned map is shared with the cache and must be treated as read-only.
// A failed read is never cached: a board that could not be read has to be able
// to report the failure the moment it happens.
func (c *Client) boardFor(ctx context.Context, acc account) (map[string]bool, error) {
	now := time.Now().UTC()
	c.board.mu.Lock()
	if c.board.tasks != nil && c.board.account == acc.ID && now.Sub(c.board.at) < onboardingBoardTTL {
		tasks := c.board.tasks
		c.board.mu.Unlock()
		return tasks, nil
	}
	c.board.mu.Unlock()

	tasks, err := c.up.onboardingTasks(ctx, acc.AccessToken)
	if err != nil {
		return nil, err
	}
	c.board.mu.Lock()
	c.board.tasks, c.board.account, c.board.at = tasks, acc.ID, now
	c.board.mu.Unlock()
	return tasks, nil
}

// forgetBoard drops the cache, so the read that follows a run sees the
// completion that just landed instead of the state that preceded it.
func (c *Client) forgetBoard() {
	c.board.mu.Lock()
	c.board.tasks, c.board.account, c.board.at = nil, "", time.Time{}
	c.board.mu.Unlock()
}

// taskAccount resolves the credential a board read or a run means.
//
// An empty id means "whichever account you would use anyway", which is core's
// TaskProvider contract and how the panel's task board calls this.  The head of
// the pool's own rotation is used, so the board describes the account a chat
// request would reach.  A non-empty id that does not exist is an error: showing
// an empty board for a typo would look like a vendor problem.
//
// A false second result means there is simply no credential to use.
func (c *Client) taskAccount(id string) (account, bool, error) {
	if trimmed := strings.TrimSpace(id); trimmed != "" {
		acc, ok := c.store.lookup(trimmed)
		if !ok {
			return account{}, false, fmt.Errorf("loomy: no account %q", trimmed)
		}
		return acc, true, nil
	}

	now := time.Now().UTC()
	if list := c.store.candidates(now); len(list) > 0 {
		return list[0], true, nil
	}
	// Every credential is disabled or parked in cooldown.  The vendor's
	// onboarding state is a property of the account rather than of a healthy
	// session, so the board still has something true to say; it falls back to
	// the first stored credential instead of claiming the module has none.
	if list := c.store.snapshot(); len(list) > 0 {
		return list[0], true, nil
	}
	return account{}, false, nil
}

// Tasks implements core.TaskProvider.
//
// The board is the vendor's eight onboarding chores with their completion
// flags, in the vendor's own order and grouping.  A chore that cannot be run is
// still returned: a row that explains itself beats a row that disappears.
func (c *Client) Tasks(ctx context.Context, accountID string) ([]core.TaskInfo, error) {
	acc, ok, err := c.taskAccount(accountID)
	if err != nil {
		return nil, err
	}
	if !ok {
		// No credential at all.  An empty board is the honest answer -- the
		// panel already words this state as normal while no account is ready --
		// and the alternative would be inventing a task code.
		return nil, nil
	}

	done, err := c.boardFor(ctx, acc)
	if err != nil {
		// The task centre is read every time the panel redraws, and a redraw
		// cancels the read in flight.  A caller that walked away is not
		// evidence about the credential, so it must not park the account.
		if !callerGone(err) {
			c.penalise(acc.ID, err, time.Now().UTC())
		}
		return nil, err
	}

	rows := make([]core.TaskInfo, 0, len(onboardingRegistry)+len(done))
	for _, ch := range onboardingRegistry {
		row := core.TaskInfo{
			Code:   ch.Key,
			Title:  ch.Title,
			Group:  ch.Group,
			Credit: ch.Points,
			Target: 1,
		}
		if done[ch.Key] {
			row.Current = 1
			row.Claimed = true
			row.Note = "厂商已回报该任务完成"
		} else {
			// A key the vendor did not mention is NOT DONE, not retired: that
			// is how the vendor's own client reads the same object (it looks
			// each registry key up in `tasks` and treats a miss as false), and
			// second-guessing it here would hide a chore that still runs.  If
			// the vendor really has retired it, the run comes back as the
			// vendor's own refusal, which is a better answer than a guess.
			row.Auto = true
			row.Note = onboardingRunNote
		}
		rows = append(rows, row)
	}

	// A key the vendor reports but the registry does not know is shown rather
	// than dropped: the operator can see the vendor grew a chore, and the row
	// says plainly that this module has no wording or price for it.  Sorted, so
	// the board does not reshuffle between reads.
	extra := make([]string, 0, len(done))
	for key := range done {
		if _, known := onboardingChoreFor(key); !known {
			extra = append(extra, key)
		}
	}
	sort.Strings(extra)
	for _, key := range extra {
		row := core.TaskInfo{
			Code:   key,
			Title:  key,
			Group:  "厂商新增",
			Target: 1,
			Locked: true,
			Note:   "厂商新增的任务，本模块还没有它的文案与积分；升级模块后才会显示并可执行",
		}
		if done[key] {
			row.Current = 1
			row.Claimed = true
			row.Note = "厂商新增的任务，厂商回报已完成；本模块还没有它的文案与积分"
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// RunTask implements core.TaskProvider: it reports one chore as complete.
//
// A vendor refusal is a result with OK false; only an unknown code or an
// unknown account is a Go error, which is what core's contract asks for.
func (c *Client) RunTask(ctx context.Context, accountID, code string) (core.TaskResult, error) {
	chore, known := onboardingChoreFor(strings.TrimSpace(code))
	if !known {
		return core.TaskResult{}, fmt.Errorf("loomy: unknown task %q", code)
	}

	acc, ok, err := c.taskAccount(accountID)
	if err != nil {
		return core.TaskResult{}, err
	}
	res := core.TaskResult{Code: chore.Key, At: time.Now().UTC().Format(time.RFC3339)}
	if !ok {
		res.Error = "loomy: no account to report the task with; import a session first"
		return res, nil
	}
	res.AccountID = acc.ID

	start := time.Now()
	already, balance, err := c.up.onboardingComplete(ctx, acc.AccessToken, chore.Key)
	res.ElapsedMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = redactErr(err)
		if failureKind(err) == core.FailureSessionDead {
			res.Error = "the session is expired or rejected; log in again and import the new token"
			c.penalise(acc.ID, err, time.Now().UTC())
		}
		return res, nil
	}
	// The board is now out of date by exactly this chore.
	c.forgetBoard()

	res.OK = true
	if already {
		// Deliberately not worded as a fresh award: the vendor says nothing was
		// added, and the price in the reward column is a price, not a receipt.
		res.Message = "厂商回报该任务此前已完成（幂等），本次没有新增积分"
	} else {
		// The price comes from the vendor's own registry, which is where the
		// vendor's client reads it too; the completion response carries a
		// balance rather than an award, so the award itself is not observable.
		res.Credit = chore.Points
		res.Message = fmt.Sprintf("厂商已接受「%s」的完成上报，该任务标价 %d %s", chore.Title, chore.Points, balanceUnit)
	}
	if balance != nil {
		if v, ok := roundInt64(balance); ok {
			res.Message += fmt.Sprintf("；厂商返回的余额为 %d %s", v, balanceUnit)
		}
	}
	return res, nil
}
