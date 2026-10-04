package workbuddy

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// streakTierPayload is a full streak read with one redeemable tier, one locked
// tier and one already-claimed tier: every branch of the redemption loop is
// represented, so a test that only expects the redeemable one is also asserting
// the other two are skipped.
const streakTierPayload = `{
  "streak": {"days": 7, "month_total_days": 12, "next_tier": "14", "next_tier_remaining": 7},
  "makeup_cards": {"balance": 0, "max": 3},
  "redemption_status": {
    "tier_7d_status": "available", "tier_14d_status": "locked", "tier_28d_status": "claimed",
    "remaining_days": 7,
    "tiers": [
      {"tier": "7d", "days": 7, "credit": 100, "energy": 10, "cards": 1, "chances": 2},
      {"tier": "14d", "days": 14, "credit": 200, "energy": 20, "cards": 1, "chances": 2},
      {"tier": "28d", "days": 28, "credit": 400, "energy": 40, "cards": 2, "chances": 5}
    ]
  }
}`

// streakPassRT answers every call the bonus pass makes.  A path the pass has no
// business touching fails the test rather than answering 200, so an unexpected
// call can never pass silently.
func streakPassRT(t *testing.T, over map[string]func(*http.Request) (*http.Response, error)) *fakeRT {
	t.Helper()
	ok := func(data string) func(*http.Request) (*http.Response, error) {
		return func(*http.Request) (*http.Response, error) {
			return jsonResponse(200, wbEnvelope(data)), nil
		}
	}
	def := map[string]func(*http.Request) (*http.Response, error){
		// An empty heatmap means nothing was missed, so no makeup card is spent
		// and the streak read happens exactly once.
		heatmapPath:           ok(`{"cells":[]}`),
		claimGiftPath:         ok(`{"credit":30}`),
		claimCompensationPath: ok(`{"credit":60}`),
		streakPath:            ok(streakTierPayload),
		streakRedeemPath:      ok(`{}`),
		lotterySummaryPath:    ok(`{"chances":2}`),
		lotteryDrawPath:       ok(`{"prize":"credit","credit":20}`),
	}
	return &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		if h, found := over[req.URL.Path]; found {
			return h(req)
		}
		if h, found := def[req.URL.Path]; found {
			return h(req)
		}
		t.Errorf("the bonus pass sent an unexpected request: %s %s", req.Method, req.URL.Path)
		return jsonResponse(500, `{}`), nil
	}}
}

func indexOfPath(paths []string, want string) int {
	for i, p := range paths {
		if p == want {
			return i
		}
	}
	return -1
}

// The pass is three phases in a fixed order: mend, collect, then spend.  The
// draws must come last because the chances they spend are granted by the
// redemption in the middle.
func TestWorkbuddyStreakBonusRunsTheWholePass(t *testing.T) {
	rt := streakPassRT(t, nil)
	c, _ := panelClient(t, rt, cnAccountFiles())
	a := wbCNAuth(t, c)

	res := c.streakBonus(context.Background(), a, core.TaskResult{Code: batchNameCheckin})

	if res.Credit != 190 {
		t.Fatalf("credit = %d, want 190 (30 gift + 60 compensation + 100 for the 7d tier)", res.Credit)
	}
	if res.Energy != 10 {
		t.Fatalf("energy = %d, want 10 (the 7d tier only)", res.Energy)
	}
	if got := wbCalls(rt, streakRedeemPath); got != 1 {
		t.Fatalf("redeem calls = %d, want exactly 1 (the locked and claimed tiers must be skipped)", got)
	}
	if got := wbCalls(rt, lotteryDrawPath); got != 2 {
		t.Fatalf("draw calls = %d, want 2 (the summary said two chances)", got)
	}

	paths := wbPaths(rt)
	order := []string{heatmapPath, claimGiftPath, claimCompensationPath, streakPath, streakRedeemPath, lotterySummaryPath, lotteryDrawPath}
	last := -1
	for _, p := range order {
		i := indexOfPath(paths, p)
		if i < 0 {
			t.Fatalf("%s was never called; saw %v", p, paths)
		}
		if i < last {
			t.Fatalf("%s ran out of order; saw %v", p, paths)
		}
		last = i
	}
}

// The tiers the redemption loop skips are the whole point of reading the status
// strings, so pin the wire body of the one redeem that must happen.
func TestWorkbuddyStreakBonusRedeemsTheTierItPosts(t *testing.T) {
	var redeem *http.Request
	rt := streakPassRT(t, map[string]func(*http.Request) (*http.Response, error){
		streakRedeemPath: func(req *http.Request) (*http.Response, error) {
			redeem = req
			return jsonResponse(200, wbEnvelope(`{}`)), nil
		},
	})
	c, _ := panelClient(t, rt, cnAccountFiles())

	c.streakBonus(context.Background(), wbCNAuth(t, c), core.TaskResult{})

	if redeem == nil {
		t.Fatal("no redeem request was sent")
	}
	if got := wbBody(t, redeem)["tier"]; got != "7d" {
		t.Fatalf("tier = %v, want the one status the upstream called available", got)
	}
}

// The international realm has no CN growth programme, so the pass must not
// spend a single request discovering that by asking.
func TestWorkbuddyStreakBonusSkipsGlobalAccounts(t *testing.T) {
	rt := streakPassRT(t, nil)
	c, _ := panelClient(t, rt, intlAccountFiles())
	a := wbIntlAuth(t, c)

	res := c.streakBonus(context.Background(), a, core.TaskResult{Code: batchNameCheckin})

	if res.Credit != 0 || res.Energy != 0 {
		t.Fatalf("credit/energy = %d/%d, want 0/0", res.Credit, res.Energy)
	}
	if paths := wbPaths(rt); len(paths) != 0 {
		t.Fatalf("the pass made %d request(s) for a global account: %v", len(paths), paths)
	}
}

// A tier that refuses is the ordinary state of a tier whose day has not arrived.
// It must not stop the tiers after it, and it must not stop the draws.
func TestWorkbuddyStreakBonusKeepsGoingWhenATierRefuses(t *testing.T) {
	const twoOpen = `{
	  "streak": {"days": 14, "month_total_days": 20, "next_tier": "28", "next_tier_remaining": 14},
	  "makeup_cards": {"balance": 0, "max": 3},
	  "redemption_status": {
	    "tier_7d_status": "available", "tier_14d_status": "available", "tier_28d_status": "locked",
	    "tiers": [
	      {"tier": "7d", "days": 7, "credit": 100, "energy": 10},
	      {"tier": "14d", "days": 14, "credit": 200, "energy": 20}
	    ]
	  }
	}`
	var tiers []string
	rt := streakPassRT(t, map[string]func(*http.Request) (*http.Response, error){
		streakPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(200, wbEnvelope(twoOpen)), nil
		},
		streakRedeemPath: func(req *http.Request) (*http.Response, error) {
			body := wbBody(t, req)
			tier, _ := body["tier"].(string)
			tiers = append(tiers, tier)
			if tier == "14d" {
				return jsonResponse(http.StatusForbidden, `连续登录天数不足`), nil
			}
			return jsonResponse(200, wbEnvelope(`{}`)), nil
		},
	})
	c, _ := panelClient(t, rt, cnAccountFiles())

	res := c.streakBonus(context.Background(), wbCNAuth(t, c), core.TaskResult{})

	if strings.Join(tiers, ",") != "7d,14d" {
		t.Fatalf("redeem attempts = %v, want both open tiers in order", tiers)
	}
	if res.Credit != 190 {
		t.Fatalf("credit = %d, want 190 (30 + 60 + the 7d tier; the refused 14d tier credits nothing)", res.Credit)
	}
	if got := wbCalls(rt, lotteryDrawPath); got != 2 {
		t.Fatalf("draw calls = %d, want 2: a refused tier must not cost the draws", got)
	}
}

// A failed streak read means the tiers are unknown, so nothing may be redeemed
// and nothing may be drawn.  What the gift and compensation already credited
// stays credited: it really did land.
func TestWorkbuddyStreakBonusStopsAtAFailedStreakRead(t *testing.T) {
	rt := streakPassRT(t, map[string]func(*http.Request) (*http.Response, error){
		streakPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusInternalServerError, `{"msg":"boom"}`), nil
		},
	})
	c, _ := panelClient(t, rt, cnAccountFiles())

	res := c.streakBonus(context.Background(), wbCNAuth(t, c), core.TaskResult{})

	if res.Credit != 90 {
		t.Fatalf("credit = %d, want 90 (the gift and compensation that already landed)", res.Credit)
	}
	if got := wbCalls(rt, streakRedeemPath); got != 0 {
		t.Fatalf("redeem calls = %d, want 0", got)
	}
	if got := wbCalls(rt, lotteryDrawPath); got != 0 {
		t.Fatalf("draw calls = %d, want 0", got)
	}
	if got := wbCalls(rt, lotterySummaryPath); got != 0 {
		t.Fatalf("lottery summary calls = %d, want 0", got)
	}
}

// A broken run is worth a makeup card; a live run, an empty card wallet and an
// unreadable heatmap are all worth nothing.
func TestWorkbuddyMakeupYesterdaySpendsACardOnlyWhenTheRunIsBroken(t *testing.T) {
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	missed := `{"cells":[{"date":"` + yesterday + `T00:00:00+08:00","score":0}]}`
	alive := `{"cells":[{"date":"` + yesterday + `T00:00:00+08:00","score":1}]}`

	tests := []struct {
		name      string
		heatmap   string
		cardBal   int
		wantCards int
	}{
		{"a broken run with a card in hand", missed, 1, 1},
		{"a broken run with no card", missed, 0, 0},
		{"a run that is still alive", alive, 1, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			streak := strings.Replace(streakTierPayload,
				`"makeup_cards": {"balance": 0, "max": 3}`,
				`"makeup_cards": {"balance": `+itoa(tc.cardBal)+`, "max": 3}`, 1)
			var used *http.Request
			rt := streakPassRT(t, map[string]func(*http.Request) (*http.Response, error){
				heatmapPath: func(*http.Request) (*http.Response, error) {
					return jsonResponse(200, wbEnvelope(tc.heatmap)), nil
				},
				streakPath: func(*http.Request) (*http.Response, error) {
					return jsonResponse(200, wbEnvelope(streak)), nil
				},
				makeupCardUsePath: func(req *http.Request) (*http.Response, error) {
					used = req
					return jsonResponse(200, wbEnvelope(`{}`)), nil
				},
			})
			c, _ := panelClient(t, rt, cnAccountFiles())

			c.streakBonus(context.Background(), wbCNAuth(t, c), core.TaskResult{})

			if got := wbCalls(rt, makeupCardUsePath); got != tc.wantCards {
				t.Fatalf("makeup-card calls = %d, want %d", got, tc.wantCards)
			}
			if tc.wantCards == 1 {
				if got := wbBody(t, used)["target_date"]; got != yesterday {
					t.Fatalf("target_date = %v, want yesterday (%s)", got, yesterday)
				}
			}
		})
	}
}

// The scheduled check-in is where the pass belongs: the vendor unlocks a tier by
// consecutive days, so the day a run reaches a tier is the day the check-in runs.
func TestWorkbuddyCheckinBatchRunsTheStreakBonus(t *testing.T) {
	rt := checkinBatchRT(t)
	c, _ := panelClient(t, rt, cnAccountFiles())
	a := wbCNAuth(t, c)

	res := runScheduledCheckin(context.Background(), c, a, core.TaskResult{Code: batchNameCheckin})

	if !res.OK {
		t.Fatalf("the check-in was not credited: %+v", res)
	}
	if got := wbCalls(rt, streakPath); got != 1 {
		t.Fatalf("streak reads = %d, want 1: the check-in must carry the bonus pass", got)
	}
	if got := wbCalls(rt, lotteryDrawPath); got != 2 {
		t.Fatalf("draw calls = %d, want 2", got)
	}
}

// The reference runs its pass over the accounts still standing, so an account
// this check-in just parked is left alone: every call the pass makes would fail
// the same way, and each one is another upstream request.
func TestWorkbuddyCheckinBatchSkipsTheStreakBonusForAParkedAccount(t *testing.T) {
	rt := checkinBatchRT(t)
	rt.handler = func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == streakPath || req.URL.Path == heatmapPath {
			t.Errorf("the bonus pass ran for a parked account: %s %s", req.Method, req.URL.Path)
		}
		// 402 is the vendor's hard-credit answer; the check-in classifies it and
		// parks the account before the pass would run.
		return jsonResponse(http.StatusPaymentRequired, `{"code":14018,"msg":"余额不足"}`), nil
	}
	c, _ := panelClient(t, rt, cnAccountFiles())
	a := wbCNAuth(t, c)

	res := runScheduledCheckin(context.Background(), c, a, core.TaskResult{Code: batchNameCheckin})

	if res.OK {
		t.Fatalf("the check-in reported success on a 402: %+v", res)
	}
	if got := wbCalls(rt, streakPath); got != 0 {
		t.Fatalf("streak reads = %d, want 0", got)
	}
	if got := wbCalls(rt, heatmapPath); got != 0 {
		t.Fatalf("heatmap reads = %d, want 0", got)
	}
}

// checkinBatchRT answers the check-in itself with a credited envelope and every
// bonus-pass call with its happy answer.
func checkinBatchRT(t *testing.T) *fakeRT {
	t.Helper()
	base := streakPassRT(t, nil)
	inner := base.handler
	base.handler = func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case heatmapPath, claimGiftPath, claimCompensationPath, streakPath,
			streakRedeemPath, lotterySummaryPath, lotteryDrawPath:
			return inner(req)
		}
		return jsonResponse(200, wbEnvelope(`{}`)), nil
	}
	return base
}

func TestCompactPrizePayload(t *testing.T) {
	long := strings.Repeat("x", 260)
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"an empty payload", ``, "-"},
		{"a whitespace payload", "  \n ", "-"},
		{"a short payload", `{"prize":"credit"}`, `{"prize":"credit"}`},
		{"a long payload", long, strings.Repeat("x", 220) + "…"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := compactPrizePayload([]byte(tc.in)); got != tc.want {
				t.Fatalf("compactPrizePayload = %q, want %q", got, tc.want)
			}
		})
	}
}
