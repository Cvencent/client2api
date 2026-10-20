package qoder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// clipool.go owns the live worker processes.
//
// Design notes, all of them load-bearing:
//
//   - One process per (account, conversation).  The worker keeps a transcript,
//     so two conversations sharing a process would answer the second with the
//     first one's context.  The conversation key is already how this module
//     pins a conversation to an account (affinity.go), so the extra key costs
//     nothing the module did not already compute.
//   - A process outlives its turn.  Cold start is 5-13s against ~2s of actual
//     inference, so a per-request process would be unusable.
//   - A process is discarded, never repaired, when its turn ends badly.  The
//     worker's stdin protocol has no cancel, so a half-read turn leaves the
//     pipe undrained; killing it and starting a new one is the only honest
//     recovery.  The next request pays one cold start.
//   - Everything is capped: processes per account, processes in total, and
//     idle lifetime.  A pool with no ceiling is a fork bomb with extra steps.
//
// The workers run under the module's own DataDir and are pointed at a
// per-account --config-dir under it, so nothing lands in the operator's
// ~/.qoder-cn and two accounts never share an identity.

const (
	// cliMaxPerAccount bounds concurrent workers for one account.  It is the
	// concurrency the panel can report, expressed in conversations rather than
	// requests.
	cliMaxPerAccount = 4
	// cliMaxTotal bounds every worker this module runs, so a pool of many
	// accounts cannot multiply unbounded.
	cliMaxTotal = 16
	// cliIdleTTL is how long an unused worker is kept.  Long enough that a
	// follow-up turn in the same conversation reuses it, short enough that a
	// forgotten conversation does not hold an Electron process all day.
	cliIdleTTL = 15 * time.Minute
	// cliSweepInterval is how often idle workers are looked for.
	cliSweepInterval = 60 * time.Second
)

// errCLINotSignedIn means the account has no CLI login under its config root.
// The pool skips such an account for the CLI-served models instead of spawning
// a process that would only answer "Not logged in".
var errCLINotSignedIn = errors.New("qoder: 该账号还没有通过本机 CLI 登录（免费模型需要先在本机登录一次）")

// cliKey identifies one worker: an account plus the conversation it serves.
type cliKey struct {
	account      string
	conversation string
}

// cliPool owns every worker of one Client.
type cliPool struct {
	mu    sync.Mutex
	procs map[cliKey]*cliProc

	stopOnce sync.Once
	stopCh   chan struct{}
	sweepWG  sync.WaitGroup
}

func newCLIPool() *cliPool {
	p := &cliPool{procs: map[cliKey]*cliProc{}, stopCh: make(chan struct{})}
	p.sweepWG.Add(1)
	core.GoSafe("qoder cli pool sweep", nil, func() {
		defer p.sweepWG.Done()
		p.sweepLoop()
	})
	return p
}

// stop terminates every worker and the sweeper.
func (p *cliPool) stop() {
	if p == nil {
		return
	}
	p.stopOnce.Do(func() {
		close(p.stopCh)
		p.sweepWG.Wait()
		p.mu.Lock()
		procs := p.procs
		p.procs = map[cliKey]*cliProc{}
		p.mu.Unlock()
		for _, proc := range procs {
			proc.stop()
		}
	})
}

func (p *cliPool) sweepLoop() {
	ticker := time.NewTicker(cliSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.sweep()
		}
	}
}

// sweep retires workers that have been idle past the TTL.  A worker in the
// middle of a turn is never a candidate: cliProc.idleFor reports zero for it.
func (p *cliPool) sweep() {
	now := time.Now()
	var retired []*cliProc
	p.mu.Lock()
	for key, proc := range p.procs {
		if proc.idleFor(now) < cliIdleTTL {
			continue
		}
		delete(p.procs, key)
		retired = append(retired, proc)
	}
	p.mu.Unlock()
	for _, proc := range retired {
		proc.stop()
	}
}

// counters reports the live pool sizes, for the panel's pool statistics.
func (p *cliPool) counters() (workers, accounts int) {
	if p == nil {
		return 0, 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	for key := range p.procs {
		seen[key.account] = true
	}
	return len(p.procs), len(seen)
}

// start returns a worker for (account, conversation), launching one if needed.
func (p *cliPool) start(ctx context.Context, inst cliInstall, dir, account, conversation, model, effort string) (*cliProc, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if err := core.EnsureDir(dir); err != nil {
		return nil, false, fmt.Errorf("qoder: creating the CLI config directory: %w", err)
	}
	key := cliKey{account: account, conversation: conversation}

	p.mu.Lock()
	if proc, ok := p.procs[key]; ok {
		if proc.alive() {
			proc.noteUse()
			p.mu.Unlock()
			return proc, false, nil
		}
		// A worker that died is replaced here rather than handed back: the
		// caller must never receive a stream that cannot produce anything.
		delete(p.procs, key)
		p.mu.Unlock()
		proc.stop()
		p.mu.Lock()
	}
	inUse := p.countAccountLocked(account)
	if inUse >= cliMaxPerAccount {
		p.mu.Unlock()
		return nil, false, fmt.Errorf("%w: qoder: 账号 %s 的本机 CLI 会话已达上限 %d",
			core.ErrBusy, account, cliMaxPerAccount)
	}
	total := len(p.procs)
	if total >= cliMaxTotal {
		if victim, ok := p.lruVictimLocked(account); ok {
			victimProc := p.procs[victim]
			delete(p.procs, victim)
			p.mu.Unlock()
			if victimProc != nil {
				victimProc.stop()
			}
			return p.start(ctx, inst, dir, account, conversation, model, effort)
		}
		p.mu.Unlock()
		return nil, false, fmt.Errorf("%w: qoder: 本机 CLI 会话已达上限 %d，请稍后重试", core.ErrBusy, cliMaxTotal)
	}
	proc := newCLIProc(inst, dir, account, model, effort)
	p.procs[key] = proc
	p.mu.Unlock()

	if err := proc.start(); err != nil {
		p.forget(key, proc)
		return nil, false, err
	}
	proc.noteUse()
	return proc, true, nil
}

// lruVictimLocked picks the least recently used idle worker.  A worker
// belonging to the account that is already over its share is preferred, so a
// busy pool shrinks where it is busiest.  The caller holds p.mu.
func (p *cliPool) lruVictimLocked(preferAccount string) (cliKey, bool) {
	now := time.Now()
	var (
		bestKey  cliKey
		found    bool
		bestIdle time.Duration
		bestTier int
	)
	for key, proc := range p.procs {
		idle := proc.idleFor(now)
		if idle == 0 {
			continue // mid-turn
		}
		tier := 1
		if key.account == preferAccount {
			tier = 2
		}
		if !found || tier > bestTier || (tier == bestTier && idle > bestIdle) {
			bestKey, bestIdle, bestTier, found = key, idle, tier, true
		}
	}
	return bestKey, found
}

func (p *cliPool) countAccountLocked(account string) int {
	n := 0
	for key := range p.procs {
		if key.account == account {
			n++
		}
	}
	return n
}

// forget removes one worker from the table if it is still the registered one,
// and stops it.
func (p *cliPool) forget(key cliKey, proc *cliProc) {
	p.mu.Lock()
	if cur, ok := p.procs[key]; ok && cur == proc {
		delete(p.procs, key)
	}
	p.mu.Unlock()
	proc.stop()
}

// ---------------------------------------------------------------------------
// turn ownership
// ---------------------------------------------------------------------------

// cliLease is one worker plus the right to exactly one turn on it, exposed to
// the gateway as a core.Stream.
type cliLease struct {
	pool     *cliPool
	key      cliKey
	proc     *cliProc
	turn     *cliTurn
	startErr error
	once     sync.Once
}

// beginLease acquires a worker and starts one turn on it.
func (p *cliPool) beginLease(ctx context.Context, inst cliInstall, dir, account, conversation, model, effort, sticky, freshPrompt string) (core.Stream, error) {
	proc, freshProc, err := p.start(ctx, inst, dir, account, conversation, model, effort)
	if err != nil {
		return nil, err
	}
	// A reused worker owns the conversation transcript, so it gets the newest
	// turn; a worker started for this call owns nothing, so it gets the whole
	// flattened history.
	prompt := sticky
	if freshProc {
		prompt = freshPrompt
	}
	lease := &cliLease{pool: p, key: cliKey{account: account, conversation: conversation}, proc: proc}
	turn, err := proc.begin(ctx, prompt)
	if err != nil {
		lease.release()
		return nil, err
	}
	lease.turn = turn
	return lease, nil
}

// Recv implements core.Stream.
func (l *cliLease) Recv() (core.Event, error) {
	if l.turn == nil {
		return core.Event{}, io.EOF
	}
	return l.turn.Recv()
}

// Close implements core.Stream.  It is idempotent, and it always decides the
// worker's fate: reuse it when the turn settled cleanly, discard it when it did
// not.
func (l *cliLease) Close() error {
	l.once.Do(func() {
		if l.turn != nil {
			_ = l.turn.Close()
		}
		if l.turn != nil && l.turn.settled() && l.proc.alive() {
			l.proc.finish()
			return
		}
		l.release()
	})
	return nil
}

// release drops the worker this lease was holding.
func (l *cliLease) release() {
	l.pool.forget(l.key, l.proc)
}

// ---------------------------------------------------------------------------
// account gating
// ---------------------------------------------------------------------------

// cliAccountReady reports whether a stored account has a CLI login to serve
// from.  It is deliberately a filesystem check on the CLI's own marker rather
// than a probe: spawning a process to ask is what the marker exists to avoid.
func (c *Client) cliAccountReady(acc account) bool {
	return cliSignedIn(c.cliDirForAccount(acc))
}

// cliRootDir is the directory every per-account config root lives under.  It is
// what the panel prints when it tells the operator where CLI state is kept.
func (c *Client) cliRootDir() string {
	if base := strings.TrimSpace(os.Getenv(cliDirEnv)); base != "" {
		return base
	}
	if strings.TrimSpace(c.deps.DataDir) == "" {
		return ""
	}
	return filepath.Join(c.deps.DataDir, "cli")
}

// cliStageLeaf is the config-root name a fresh CLI login is staged in.  It is
// unique per attempt and renamed onto the account id once the vendor has told
// us who signed in.
func (c *Client) cliStageLeaf(nonce string) string {
	return filepath.Join(".staging", "login-"+shortHash(nonce))
}

// adoptCLIDir moves a staged config root onto the account's own name so it
// survives the login session.  A move that cannot happen is not fatal: the
// account record carries the staged leaf instead.
func (c *Client) adoptCLIDir(staged, accountID string) string {
	if strings.TrimSpace(staged) == "" {
		return staged
	}
	from := c.cliConfigDir(staged)
	if !cliSignedIn(from) {
		return staged
	}
	want := cliConfigLeaf(accountID)
	if want == staged {
		return staged
	}
	to := c.cliConfigDir(want)
	if _, err := os.Stat(to); err == nil {
		// The account already has a root: keep that one and drop the staged
		// copy, so a second signed-in identity cannot linger under DataDir.
		_ = os.RemoveAll(from)
		return want
	}
	if err := os.Rename(from, to); err != nil {
		c.logf("qoder: adopting the CLI config root %s -> %s: %v", from, to, err)
		return staged
	}
	_ = os.Remove(filepath.Dir(from))
	return want
}
