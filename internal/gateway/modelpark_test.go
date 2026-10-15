package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"

	"client2api/internal/core"
	"client2api/internal/modelpark"
)

// parkedClient mimics a module that rotates its own accounts: it takes the
// gateway's per-account slot before every attempt and reports busy when no
// account is left.  absorb controls whether the first 429 ends the Chat call
// (the gateway then records it) or is absorbed by rotating to the next account
// (an integrated module reports it through NoteAccountFailure instead).
type parkedClient struct {
	name          string
	accounts      []string
	attempted     []string
	served        string
	failOnce      bool
	absorb        bool
	reportFailure bool
}

func (c *parkedClient) Name() string { return c.name }

func (c *parkedClient) Models(context.Context) ([]core.Model, error) {
	return []core.Model{{ID: "m1"}}, nil
}

func (c *parkedClient) Chat(_ context.Context, req *core.ChatRequest) (core.Stream, error) {
	for _, id := range c.accounts {
		c.attempted = append(c.attempted, id)
		if err := req.AcquireAccountSlot(id); err != nil {
			continue
		}
		if id == c.accounts[0] && !c.failOnce {
			c.failOnce = true
			failure := core.Fail(c.name, id, core.FailureRateLimited, http.StatusTooManyRequests, errors.New("too many requests"))
			if c.reportFailure {
				req.NoteAccountFailure(id, failure)
			}
			if c.absorb {
				continue
			}
			return nil, failure
		}
		c.served = id
		core.NoteServedBy(req, id)
		return &fakeStream{events: usageEvents(1, 1)}, nil
	}
	return nil, fmt.Errorf("%w: no account is able to serve this model", core.ErrBusy)
}

func (c *parkedClient) Status(context.Context) core.Status {
	return core.Status{Name: c.name, Ready: true}
}

func newModelParkServer(t *testing.T, c *parkedClient, parks *modelpark.Store) *http.Server {
	t.Helper()
	reg := core.NewRegistry()
	reg.Add(c)
	return NewServer(Options{
		Registry:   reg,
		Version:    "test",
		Logger:     log.New(io.Discard, "", 0),
		ModelParks: parks,
	})
}

func TestAccountSlotRefusesAParkedModel(t *testing.T) {
	parks := modelpark.New()
	srv := &server{opts: Options{ModelParks: parks}}
	parks.Park("cline", "acct-a", "m1", "429", 0)

	if _, err := srv.acquireAccountSlot("cline", "acct-a", "m1"); !errors.Is(err, core.ErrBusy) || !IsModelParkRefusal(err) {
		t.Fatalf("parked model error = %v, want a park refusal wrapping ErrBusy", err)
	}
	release, err := srv.acquireAccountSlot("cline", "acct-a", "m2")
	if err != nil {
		t.Fatalf("a different model was refused: %v", err)
	}
	release()
	release, err = srv.acquireAccountSlot("cline", "acct-b", "m1")
	if err != nil {
		t.Fatalf("another account was refused: %v", err)
	}
	release()
	// A module that did not name the account, or a request with no model, is
	// not gated: the store cannot be matched against either.
	if _, err := srv.acquireAccountSlot("cline", "acct-a", ""); err != nil {
		t.Fatalf("an empty model was refused: %v", err)
	}
	if _, err := srv.acquireAccountSlot("cline", "", "m1"); err != nil {
		t.Fatalf("an empty account was refused: %v", err)
	}
}

func TestRateLimitParksTheModelAndRotatesToAnotherAccount(t *testing.T) {
	parks := modelpark.New()
	c := &parkedClient{name: "rot", accounts: []string{"acct-a", "acct-b"}}
	srv := newModelParkServer(t, c, parks)

	rec := chat(t, srv, bufferedBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if c.served != "acct-b" {
		t.Fatalf("served = %q, want acct-b after acct-a was parked", c.served)
	}
	if _, ok := parks.Active("rot", "acct-a", "m1"); !ok {
		t.Fatal("the failing (account, model) pair was not parked")
	}
	if _, ok := parks.Active("rot", "acct-b", "m1"); ok {
		t.Fatal("the healthy account was parked too")
	}
}

func TestModuleReportedFailureParksEvenWhenTheModuleRecovers(t *testing.T) {
	parks := modelpark.New()
	c := &parkedClient{
		name:          "rot",
		accounts:      []string{"acct-a", "acct-b"},
		absorb:        true,
		reportFailure: true,
	}
	srv := newModelParkServer(t, c, parks)

	rec := chat(t, srv, bufferedBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if c.served != "acct-b" {
		t.Fatalf("served = %q, want acct-b", c.served)
	}
	if _, ok := parks.Active("rot", "acct-a", "m1"); !ok {
		t.Fatal("a failure the module absorbed was not parked")
	}
}

func TestFailureReasonNamesAParkedModel(t *testing.T) {
	err := &modelParkRefusal{model: "m1", until: "12:00:00"}
	got := failureReason(err)
	if !strings.Contains(got, "限流冷却") {
		t.Fatalf("failureReason(park refusal) = %q, want it to name the rate-limit cooldown", got)
	}
	if strings.Contains(got, "本地并发已满") {
		t.Fatalf("failureReason(park refusal) = %q, should not read as local backpressure", got)
	}
}

func TestFailureReasonNamesAnExhaustedPlatform(t *testing.T) {
	got := failureReason(core.ErrPlatformExhausted)
	if !strings.Contains(got, "平台暂无可用账号") {
		t.Fatalf("failureReason(ErrPlatformExhausted) = %q, want it to name the exhausted platform", got)
	}
	// A module that also classified the refusal keeps the more specific label:
	// "the pool is empty" must not hide "the upstream rate limited us".
	joined := errors.Join(core.ErrPlatformExhausted,
		core.Fail("wb", "a1", core.FailureRateLimited, http.StatusTooManyRequests, errors.New("429")))
	if got := failureReason(joined); !strings.Contains(got, "上游限流") {
		t.Fatalf("failureReason(exhausted+classified) = %q, want the classified label", got)
	}
}
