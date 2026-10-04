package workbuddy

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestWorkbuddyTravelStatusDepartsAndClaims(t *testing.T) {
	t.Run("status", func(t *testing.T) {
		var seen *http.Request
		rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
			seen = req
			return jsonResponse(200, wbEnvelope(
				`{"state":"arrived","daily_limit_reached":true,"record_id":41,"reward_credit":7}`)), nil
		}}
		c, _ := panelClient(t, rt, cnAccountFiles())
		a := wbCNAuth(t, c)

		st, err := c.TravelStatus(context.Background(), a)
		if err != nil {
			t.Fatalf("TravelStatus: %v", err)
		}
		if st.State != "arrived" || !st.DailyLimitReached || st.RecordID != 41 || st.RewardCredit != 7 {
			t.Fatalf("TravelStatus = %+v, want arrived/true/41/7", st)
		}
		req := wbIdentity(t, seen, a)
		if req.Method != http.MethodGet || req.URL.Path != travelStatusPath {
			t.Fatalf("%s %s, want GET %s", req.Method, req.URL.Path, travelStatusPath)
		}
	})

	t.Run("depart", func(t *testing.T) {
		var seen *http.Request
		rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
			seen = req
			return jsonResponse(200, wbEnvelope(`{}`)), nil
		}}
		c, _ := panelClient(t, rt, cnAccountFiles())
		a := wbCNAuth(t, c)

		if err := c.TravelDepart(context.Background(), a, 3); err != nil {
			t.Fatalf("TravelDepart: %v", err)
		}
		req := wbIdentity(t, seen, a)
		if req.Method != http.MethodPost || req.URL.Path != travelDepartPath {
			t.Fatalf("%s %s, want POST %s", req.Method, req.URL.Path, travelDepartPath)
		}
		body := wbBody(t, req)
		if body["location_id"] != float64(3) {
			t.Fatalf("location_id = %v, want 3", body["location_id"])
		}
	})

	t.Run("claim", func(t *testing.T) {
		var seen *http.Request
		rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
			seen = req
			return jsonResponse(200, wbEnvelope(`{"reward_credit":42}`)), nil
		}}
		c, _ := panelClient(t, rt, cnAccountFiles())
		a := wbCNAuth(t, c)

		got, err := c.TravelClaim(context.Background(), a, 41)
		if err != nil {
			t.Fatalf("TravelClaim: %v", err)
		}
		if got != 42 {
			t.Fatalf("reward = %d, want 42", got)
		}
		req := wbIdentity(t, seen, a)
		if req.Method != http.MethodPost || req.URL.Path != travelClaimPath {
			t.Fatalf("%s %s, want POST %s", req.Method, req.URL.Path, travelClaimPath)
		}
		body := wbBody(t, req)
		if body["record_id"] != float64(41) {
			t.Fatalf("record_id = %v, want 41", body["record_id"])
		}
	})
}

// A claim answer without the reward field is a zero, not an error: the caller
// logs a zero exactly like the reference does.
func TestWorkbuddyTravelClaimWithoutARewardFieldIsZero(t *testing.T) {
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		return jsonResponse(200, wbEnvelope(`{"state":"idle"}`)), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())

	got, err := c.TravelClaim(context.Background(), wbCNAuth(t, c), 41)
	if err != nil {
		t.Fatalf("TravelClaim: %v", err)
	}
	if got != 0 {
		t.Fatalf("reward = %d, want 0", got)
	}
}

func TestWorkbuddyBuddyInfoHandlesTheEmptyAnswers(t *testing.T) {
	tests := []struct {
		name string
		data string
		want *Buddy
	}{
		{"profile", `{"buddy":{"id":9,"name":"Mochi"}}`, &Buddy{ID: 9, Name: "Mochi"}},
		{"null", `{"buddy":null}`, nil},
		{"absent", `{}`, nil},
		{"empty object", `{"buddy":{}}`, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
				return jsonResponse(200, wbEnvelope(tc.data)), nil
			}}
			c, _ := panelClient(t, rt, cnAccountFiles())

			got, err := c.BuddyInfo(context.Background(), wbCNAuth(t, c))
			if err != nil {
				t.Fatalf("BuddyInfo: %v", err)
			}
			if tc.want == nil {
				if got != nil {
					t.Fatalf("BuddyInfo = %+v, want nil", got)
				}
				return
			}
			if got == nil || *got != *tc.want {
				t.Fatalf("BuddyInfo = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// GrowthStreak is a read-only oracle: a payload without streak/days is 0, not
// an error, because 0 is the "reported 200 but the event was dropped" signal.
func TestWorkbuddyGrowthStreakReadsDaysAndToleratesAnEmptyAnswer(t *testing.T) {
	tests := []struct {
		name string
		data string
		want int
	}{
		{"days", `{"streak":{"days":5,"month_total_days":9}}`, 5},
		{"without the field", `{}`, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var seen *http.Request
			rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
				seen = req
				return jsonResponse(200, wbEnvelope(tc.data)), nil
			}}
			c, _ := panelClient(t, rt, cnAccountFiles())
			a := wbCNAuth(t, c)

			got, err := c.GrowthStreak(context.Background(), a)
			if err != nil {
				t.Fatalf("GrowthStreak: %v", err)
			}
			if got != tc.want {
				t.Fatalf("days = %d, want %d", got, tc.want)
			}
			req := wbIdentity(t, seen, a)
			if req.Method != http.MethodGet || req.URL.Path != streakPath {
				t.Fatalf("%s %s, want GET %s", req.Method, req.URL.Path, streakPath)
			}
		})
	}
}

// The growth host is per realm: CN talks to copilot.tencent.com, the
// international realm to the global chat base.
func TestWorkbuddyGrowthEndpointsFollowTheRealmHost(t *testing.T) {
	t.Run("cn", func(t *testing.T) {
		rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
			return jsonResponse(200, wbEnvelope(`{"streak":{"days":1}}`)), nil
		}}
		c, _ := panelClient(t, rt, cnAccountFiles())
		if _, err := c.GrowthStreak(context.Background(), wbCNAuth(t, c)); err != nil {
			t.Fatalf("GrowthStreak: %v", err)
		}
		if got := wbHostOf(t, rt); got != "copilot.tencent.com" {
			t.Fatalf("host = %q, want copilot.tencent.com", got)
		}
	})

	t.Run("global", func(t *testing.T) {
		rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
			return jsonResponse(200, wbEnvelope(`{"streak":{"days":1}}`)), nil
		}}
		c, _ := panelClient(t, rt, intlAccountFiles())
		if _, err := c.GrowthStreak(context.Background(), wbIntlAuth(t, c)); err != nil {
			t.Fatalf("GrowthStreak: %v", err)
		}
		host := wbHostOf(t, rt)
		if host != "www.workbuddy.ai" && host != "workbuddy.ai" {
			t.Fatalf("host = %q, want the global chat base", host)
		}
	})
}

// The buddy chore gate: the vendor answers 400 with the "first_buddy task not
// completed yet" marker until adoption has been credited.
func TestWorkbuddyBuddyTravelLockedDetectsTheAdoptionGate(t *testing.T) {
	if buddyTravelLocked(nil) {
		t.Fatal("nil error reported as locked")
	}
	if buddyTravelLocked(errors.New("plain failure")) {
		t.Fatal("an unrelated error reported as locked")
	}
	gate := &Error{Status: http.StatusBadRequest, Msg: "first_buddy task not completed yet"}
	if !buddyTravelLocked(gate) {
		t.Fatalf("the adoption-gate error was not detected: %v", gate)
	}

	// The same answer as the vendor really sends it (HTTP 400 + raw body) must
	// survive classification, so the detector is exercised against a real *Error.
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusBadRequest,
			`{"code":1,"msg":"first_buddy task not completed yet"}`), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())
	err := c.TravelDepart(context.Background(), wbCNAuth(t, c), 1)
	if err == nil {
		t.Fatal("a gated depart returned no error")
	}
	if !buddyTravelLocked(err) {
		t.Fatalf("a real gated depart error was not detected: %v", err)
	}
}
