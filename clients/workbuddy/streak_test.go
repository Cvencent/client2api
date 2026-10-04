package workbuddy

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"testing"
)

// clientTokenShape is the dash-grouped 4-2-2-2-6 hex the vendor's client_token
// field has to look like.
var clientTokenShape = regexp.MustCompile(`^[0-9a-f]{4}(-[0-9a-f]{2}){3}-[0-9a-f]{6}$`)

func TestWorkbuddyGrowthStreakFullReadsTheWholePayload(t *testing.T) {
	const data = `{
	  "streak": {"days": 6, "month_total_days": 11, "next_tier": "14", "next_tier_remaining": 8},
	  "makeup_cards": {"balance": 1, "max": 3},
	  "redemption_status": {
	    "tier_7d_status": "claimed", "tier_14d_status": "available", "tier_28d_status": "locked",
	    "remaining_days": 8,
	    "tiers": [{"tier": "7d", "days": 7, "credit": 100, "energy": 10, "cards": 1, "chances": 2}]
	  }
	}`
	var seen *http.Request
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		seen = req
		return jsonResponse(200, wbEnvelope(data)), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())
	a := wbCNAuth(t, c)

	got, err := c.GrowthStreakFull(context.Background(), a)
	if err != nil {
		t.Fatalf("GrowthStreakFull: %v", err)
	}
	if got == nil {
		t.Fatal("GrowthStreakFull returned nil")
	}
	req := wbIdentity(t, seen, a)
	if req.Method != http.MethodGet {
		t.Fatalf("method = %s, want GET", req.Method)
	}
	if req.URL.Path != streakPath {
		t.Fatalf("path = %s, want %s", req.URL.Path, streakPath)
	}
	if got.Streak.Days != 6 || got.Streak.MonthTotalDays != 11 ||
		got.Streak.NextTier != "14" || got.Streak.NextTierRemaining != 8 {
		t.Fatalf("streak block = %+v, want 6/11/\"14\"/8", got.Streak)
	}
	if got.MakeupCards.Balance != 1 || got.MakeupCards.Max != 3 {
		t.Fatalf("makeup_cards = %+v, want 1/3", got.MakeupCards)
	}
	rs := got.RedemptionStatus
	if rs.Tier7dStatus != "claimed" || rs.Tier14dStatus != "available" || rs.Tier28dStatus != "locked" {
		t.Fatalf("tier statuses = %q/%q/%q", rs.Tier7dStatus, rs.Tier14dStatus, rs.Tier28dStatus)
	}
	if rs.RemainingDays != 8 {
		t.Fatalf("remaining_days = %d, want 8", rs.RemainingDays)
	}
	if len(rs.Tiers) != 1 {
		t.Fatalf("tiers = %+v, want one entry", rs.Tiers)
	}
	if tier := rs.Tiers[0]; tier.Tier != "7d" || tier.Days != 7 || tier.Credit != 100 ||
		tier.Energy != 10 || tier.Cards != 1 || tier.Chances != 2 {
		t.Fatalf("tier = %+v, want the full \"7d\"/7/100/10/1/2 row", tier)
	}
}

func TestWorkbuddyGrowthRedeemTierPostsTierAndToken(t *testing.T) {
	var seen *http.Request
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		seen = req
		return jsonResponse(200, wbEnvelope(`{}`)), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())
	a := wbCNAuth(t, c)

	if err := c.GrowthRedeemTier(context.Background(), a, "14"); err != nil {
		t.Fatalf("GrowthRedeemTier: %v", err)
	}
	req := wbIdentity(t, seen, a)
	if req.Method != http.MethodPost {
		t.Fatalf("method = %s, want POST", req.Method)
	}
	if req.URL.Path != streakRedeemPath {
		t.Fatalf("path = %s, want %s", req.URL.Path, streakRedeemPath)
	}
	body := wbBody(t, req)
	if body["tier"] != "14" {
		t.Fatalf("tier = %v, want the string \"14\"", body["tier"])
	}
	tok, _ := body["client_token"].(string)
	if !clientTokenShape.MatchString(tok) {
		t.Fatalf("client_token = %q, want the 4-2-2-2-6 hex shape", tok)
	}
}

func TestWorkbuddyGrowthRedeemTierSurfacesALockedTier(t *testing.T) {
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusForbidden, `连续登录天数不足`), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())
	a := wbCNAuth(t, c)

	err := c.GrowthRedeemTier(context.Background(), a, "14")
	var ue *Error
	if !asError(err, &ue) {
		t.Fatalf("GrowthRedeemTier error = %v (%T), want an *Error", err, err)
	}
	if ue.Status != http.StatusForbidden {
		t.Fatalf("Status = %d, want 403", ue.Status)
	}
	if ue.Kind == ErrNone {
		t.Fatalf("Kind = ErrNone, want a classified kind")
	}
}

func TestWorkbuddyLotteryChancesAndDraw(t *testing.T) {
	t.Run("chances", func(t *testing.T) {
		var seen *http.Request
		rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
			seen = req
			return jsonResponse(200, wbEnvelope(`{"chances":3,"module":{"enabled":true}}`)), nil
		}}
		c, _ := panelClient(t, rt, cnAccountFiles())
		a := wbCNAuth(t, c)

		got, err := c.LotteryChances(context.Background(), a)
		if err != nil {
			t.Fatalf("LotteryChances: %v", err)
		}
		if got != 3 {
			t.Fatalf("chances = %d, want 3", got)
		}
		req := wbIdentity(t, seen, a)
		if req.Method != http.MethodGet || req.URL.Path != lotterySummaryPath {
			t.Fatalf("%s %s, want GET %s", req.Method, req.URL.Path, lotterySummaryPath)
		}
	})

	t.Run("draw", func(t *testing.T) {
		var seen *http.Request
		rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
			seen = req
			return jsonResponse(200, wbEnvelope(`{"prize":"credit","credit":20}`)), nil
		}}
		c, _ := panelClient(t, rt, cnAccountFiles())
		a := wbCNAuth(t, c)

		raw, err := c.LotteryDraw(context.Background(), a)
		if err != nil {
			t.Fatalf("LotteryDraw: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("draw payload %s: %v", raw, err)
		}
		if got["prize"] != "credit" || got["credit"] != float64(20) {
			t.Fatalf("draw payload = %v, want the raw prize block", got)
		}
		req := wbIdentity(t, seen, a)
		if req.Method != http.MethodPost || req.URL.Path != lotteryDrawPath {
			t.Fatalf("%s %s, want POST %s", req.Method, req.URL.Path, lotteryDrawPath)
		}
		body := wbBody(t, req)
		tok, _ := body["client_token"].(string)
		if !clientTokenShape.MatchString(tok) {
			t.Fatalf("client_token = %q, want the 4-2-2-2-6 hex shape", tok)
		}
	})
}
