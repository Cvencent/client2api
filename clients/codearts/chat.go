package codearts

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"client2api/internal/core"
)

// chat.go is the request path: pick an account, sign the request, send it, and
// decide what to do when the answer is not a stream.
//
// Three behaviours here are not obvious and all three come from the reference
// implementation:
//
//   - a credential whose expiry is inside `refreshMargin` is refreshed BEFORE
//     the first attempt, so a long-lived stream does not die half way through
//     because the security token expired mid-answer;
//   - an auth failure retries once, per account, with a freshly minted
//     credential.  APIG.0602 ("Invalid token") arrives even when the stated
//     expiry has not passed, so the expiry alone cannot be trusted;
//   - a concurrency-queue rejection is not an error the operator sees.  It is
//     polled on the queue-status endpoint until the task starts, the same way
//     the reference implementation does, with a bounded total wait.

// chatTimeout bounds one Chat call end to end, including the queue wait.
func (c *Client) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil || strings.TrimSpace(req.Model) == "" {
		return nil, core.ErrUnsupported
	}
	if c.pool.len() == 0 {
		return nil, core.ErrNotConfigured
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.chatTimeout())
	release := func() { cancel() }

	// `release` is handed to the stream BEFORE its reader goroutine starts, not
	// assigned afterwards: the goroutine calls takeRelease() when it finishes,
	// so a post-construction write would race with it — and would be a genuine
	// race, not just a -race complaint, because a stream that ends instantly
	// can read the field while Chat is still writing it.
	stream, err := c.openStream(ctx, req, release)
	if err != nil {
		cancel()
		return nil, err
	}
	c.inFlight.Add(1)
	return core.TrackStream(stream, func() { c.inFlight.Add(-1) }), nil
}

// openStream runs the account retry loop and returns a live stream.
func (c *Client) openStream(ctx context.Context, req *core.ChatRequest, release func()) (*stream, error) {
	attempts := c.cfg.maxAttempts()
	if attempts < 1 {
		attempts = 1
	}
	var (
		tried     = map[string]bool{}
		rateLimit = map[string]bool{}
		lastErr   error
		lastAcct  string
		// busy records that every account we reached was at its per-account
		// in-flight ceiling, so the pool is full rather than empty.
		busy bool
	)

	for i := 0; i < attempts; i++ {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return nil, c.terminal(lastErr, lastAcct)
			}
			return nil, err
		}
		e := c.pickAccount(req, tried)
		if e == nil {
			if lastErr != nil {
				return nil, c.terminal(lastErr, lastAcct)
			}
			if busy {
				return nil, core.ErrBusy
			}
			return nil, core.ErrNotConfigured
		}
		acctID := e.id()
		tried[acctID] = true
		// Take the account's slot before any upstream call.  A full account is
		// skipped, not blamed, so the next account is tried.
		if err := req.AcquireAccountSlot(acctID); err != nil {
			busy = true
			continue
		}
		// Bind only after the slot is genuinely held, so a busy account that was
		// skipped cannot capture the conversation.
		c.bindServedConversation(req, acctID)
		// The gateway reads this slot to attribute the request in its usage
		// ledger, so it is set before every attempt, not once.
		core.NoteServedBy(req, acctID)

		// Proactive refresh: a credential that is about to expire would die
		// mid-stream.
		if e.acct.expiringWithin(time.Now(), c.cfg.refreshMargin()) && e.acct.refreshable() {
			if err := c.refreshCredential(ctx, e); err != nil {
				if errors.Is(err, errRefreshTerminal) {
					c.pool.markDead(e, err.Error())
				} else {
					c.pool.markFailure(e, kindAuth, err.Error())
				}
				lastErr, lastAcct = err, acctID
				continue
			}
		}

		resp, err := c.sendChat(ctx, e.account(), req)
		if err != nil {
			kind, msg := classifyErr(err)
			if kind == kindQueue {
				// A queue rejection is expected under load, not a broken
				// credential: poll instead of failing, and do not cool the
				// account down.
				started, qerr := c.awaitQueue(ctx, e.account(), req.Model)
				if qerr != nil {
					lastErr, lastAcct = qerr, acctID
					c.pool.markFailure(e, kindQueue, qerr.Error())
					continue
				}
				if !started {
					lastErr, lastAcct = err, acctID
					c.pool.markFailure(e, kindQueue, msg)
					continue
				}
				resp, err = c.sendChat(ctx, e.account(), req)
				if err != nil {
					kind, msg = classifyErr(err)
				}
			}
			if err != nil && kind == kindAuth && e.acct.refreshable() {
				// One silent refresh, then one retry, on the same account:
				// the retry does not consume an attempt slot.
				if rerr := c.refreshCredential(ctx, e); rerr != nil {
					if errors.Is(rerr, errRefreshTerminal) {
						c.pool.markDead(e, rerr.Error())
					} else {
						c.pool.markFailure(e, kindAuth, rerr.Error())
					}
					lastErr, lastAcct = rerr, acctID
					continue
				}
				resp, err = c.sendChat(ctx, e.account(), req)
				if err != nil {
					kind, msg = classifyErr(err)
				}
			}
			if err != nil {
				lastErr, lastAcct = err, acctID
				// A cancelled caller is not evidence about the credential: the
				// operator pressed Stop, or the browser went away.  Cooling the
				// account down for it would take a healthy credential out of
				// the pool, and retrying it on the next candidate would just
				// burn that one too.  classifyErr maps a cancellation to
				// kindTransient, which is on the cooldown list below, so
				// without this guard one interrupted request parks the account
				// for default_short_cooldown.
				//
				// ctx is the derived one -- Chat wrapped it in chat_timeout --
				// so a deadline here may be our own timeout, which *is* worth a
				// cooldown.  Only context.Canceled is unambiguous.
				if errors.Is(err, context.Canceled) {
					return nil, c.terminal(err, acctID)
				}
				switch kind {
				case kindAuth:
					c.pool.markFailure(e, kindAuth, msg)
				case kindQuota:
					c.pool.markFailure(e, kindQuota, msg)
					rateLimit[acctID] = true
				case kindQueue, kindTransient, kindNetwork:
					c.pool.markFailure(e, kind, msg)
				default:
					// A client error is this module's own bug: it must not
					// cool the account down, and retrying it on another
					// account would just repeat the same mistake.
					return nil, c.terminal(err, acctID)
				}
				if !retryable(kind) {
					return nil, c.terminal(err, acctID)
				}
				continue
			}
		}

		c.pool.markUsed(e)
		return newStream(ctx, resp.Body, c.cfg.firstTokenWait(), c.cfg.chunkWait(), release), nil
	}

	if lastErr == nil {
		if busy {
			return nil, core.ErrBusy
		}
		return nil, core.ErrNotConfigured
	}
	return nil, c.terminal(lastErr, lastAcct)
}

// sendChat performs one signed POST to the chat endpoint.
func (c *Client) sendChat(ctx context.Context, acct account, req *core.ChatRequest) (*http.Response, error) {
	sessionID := sessionIDFor(req)
	body, err := c.buildChatBody(req, sessionID)
	if err != nil {
		return nil, fmt.Errorf("codearts: encoding the request: %w", err)
	}
	// The benefit header is signed when — and only when — the model bills
	// against the free quota.  Sending it for a paid model is refused by the
	// gateway, and omitting it for a benefit model is refused too.
	var signedExtra map[string]string
	if c.isBenefitModel(req.Model) {
		signedExtra = map[string]string{"maas_type": "benefit"}
	}
	resp, err := c.doSigned(ctx, acct, http.MethodPost, c.cfg.chatURL(), body, signedExtra, map[string]string{
		"Chat-Id":    newUUID(),
		"Session-Id": sessionID,
		"lang":       "en",
	})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, readErrorBody(resp)
	}
	return resp, nil
}

// sessionIDFor picks the identifier the backend uses to reuse a prefix cache.
//
// It prefers the caller's conversation id, so every turn of one conversation
// shares a cache; a request with no conversation gets a fresh id, because
// reusing one across unrelated requests would make the cache useless.
func sessionIDFor(req *core.ChatRequest) string {
	if req != nil {
		if v := strings.TrimSpace(req.ConversationID); v != "" {
			return v
		}
	}
	return newUUID()
}

// ---------------------------------------------------------------------------
// the concurrency queue
// ---------------------------------------------------------------------------

// awaitQueue polls the queue-status endpoint until the task stops waiting.
//
// It returns true when the task is no longer queued, so the caller should
// retry the chat request.  A `false` return means the wait ran out; the error
// explains which of the two happened.
func (c *Client) awaitQueue(ctx context.Context, acct account, model string) (bool, error) {
	deadline := time.Now().Add(c.cfg.queueMaxWait())
	every := c.cfg.queuePollEvery()
	if every <= 0 {
		every = defaultQueuePollEvery
	}
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if time.Now().After(deadline) {
			return false, fmt.Errorf("codearts: the model is still queued after %s", c.cfg.queueMaxWait())
		}

		q := url.Values{}
		q.Set("model", model)
		q.Set("task_id", newUUID())
		rawURL := c.cfg.queueStatusURL() + "?" + q.Encode()

		var out queueStatus
		err := c.doJSON(ctx, acct, http.MethodGet, rawURL, nil, nil, map[string]string{
			"x-snap-traceid": newUUID(),
			"Agent-Type":     "INFERHUB_AGENT",
			"X-Language":     "en",
		}, &out)
		if err != nil {
			kind, msg := classifyErr(err)
			// A queue-status failure is not the chat request's failure: the
			// request is still worth retrying once the wait is over.
			if kind == kindAuth {
				return false, err
			}
			if msg == "" {
				msg = err.Error()
			}
			c.deps.Log("codearts: queue status: %s", msg)
		} else {
			switch strings.ToLower(strings.TrimSpace(out.Status)) {
			case "waiting":
				c.deps.Log("codearts: queued at position %d", out.QueuePosition)
			case "working", "":
				// The task is being served (or the endpoint said nothing
				// useful): either way it is no longer waiting.
				return true, nil
			case "queue_full":
				return false, fmt.Errorf("codearts: the model's queue is full: %s", cleanErrorText(out.Message))
			case "error":
				return false, fmt.Errorf("codearts: the queue reported an error: %s", cleanErrorText(out.Message))
			default:
				return true, nil
			}
		}
		if !core.SleepCtx(ctx, every) {
			return false, ctx.Err()
		}
	}
}

// queueStatus is the queue-status endpoint's answer.
type queueStatus struct {
	Status        string `json:"status"`
	QueuePosition int    `json:"queue_position"`
	Message       string `json:"message"`
}

// ---------------------------------------------------------------------------
// classification helpers
// ---------------------------------------------------------------------------

// retryable reports whether another account is worth trying after this kind of
// failure.
func retryable(kind errKind) bool {
	switch kind {
	case kindAuth, kindQuota, kindQueue, kindTransient, kindNetwork:
		return true
	default:
		return false
	}
}

// terminal wraps the last failure so the gateway can classify it, rotate if it
// is retryable, and attribute it to the account that produced it.
func (c *Client) terminal(err error, acctID string) error {
	if err == nil {
		return core.ErrNotConfigured
	}
	kind, msg := classifyErr(err)
	if msg == "" {
		msg = err.Error()
	}
	status := 0
	if ue, ok := asUpstreamError(err); ok {
		status = ue.Status
	}
	// The message is redacted before it is wrapped: it travels to the client
	// and into the log.
	return core.Fail("codearts", acctID, failureKind(kind), status, errors.New(core.Redact(msg)))
}

// ---------------------------------------------------------------------------
// model catalogue plumbing for chat
// ---------------------------------------------------------------------------

// Models answers from the cache and refreshes in the background when the cache
// is stale, so a request that needs a model name never waits on the catalogue.
func (c *Client) Models(ctx context.Context) ([]core.Model, error) {
	now := time.Now()
	if !c.modelsFresh(now) {
		c.refreshModelsAsync()
	}
	return c.cachedModels(), nil
}
