package workbuddy

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWorkbuddyClaimGiftAndCompensation(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		credit int64
		call   func(c *Client, a *Auth) (int64, error)
	}{
		{"gift", claimGiftPath, 30, func(c *Client, a *Auth) (int64, error) {
			return c.ClaimGift(context.Background(), a)
		}},
		{"compensation", claimCompensationPath, 60, func(c *Client, a *Auth) (int64, error) {
			return c.ClaimCompensation(context.Background(), a)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var seen *http.Request
			rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
				seen = req
				return jsonResponse(200, wbEnvelope(`{"credit":`+strconv.FormatInt(tc.credit, 10)+`}`)), nil
			}}
			c, _ := panelClient(t, rt, cnAccountFiles())
			a := wbCNAuth(t, c)

			got, err := tc.call(c, a)
			if err != nil {
				t.Fatalf("claim: %v", err)
			}
			if got != tc.credit {
				t.Fatalf("credit = %d, want %d", got, tc.credit)
			}
			req := wbIdentity(t, seen, a)
			if req.Method != http.MethodPost {
				t.Fatalf("method = %s, want POST", req.Method)
			}
			if req.URL.Path != tc.path {
				t.Fatalf("path = %s, want %s", req.URL.Path, tc.path)
			}
			if req.URL.Host != "www.codebuddy.cn" {
				t.Fatalf("host = %s, want the CN billing base", req.URL.Host)
			}
			body := wbBody(t, req)
			if len(body) != 0 {
				t.Fatalf("body = %v, want an empty object", body)
			}
		})
	}
}

// A claim that was already taken comes back as a business-code refusal, and it
// must surface as an *Error rather than a silent zero.
func TestWorkbuddyClaimGiftSurfacesAnAlreadyClaimedRefusal(t *testing.T) {
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		return jsonResponse(200, wbRefusal(12002, "the gift has already been claimed")), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())

	got, err := c.ClaimGift(context.Background(), wbCNAuth(t, c))
	if err == nil {
		t.Fatalf("ClaimGift returned credit=%d and no error, want a refusal", got)
	}
	var ue *Error
	if !asError(err, &ue) {
		t.Fatalf("error = %v (%T), want an *Error", err, err)
	}
	if ue.Status != http.StatusOK {
		t.Fatalf("Status = %d, want the HTTP status (200) the refusal arrived on", ue.Status)
	}
}

func TestWorkbuddyHeatmapYesterdayMissed(t *testing.T) {
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	tests := []struct {
		name string
		data string
		want bool
	}{
		{
			"a zero score on yesterday",
			`{"cells":[{"date":"2000-01-01","score":5},{"date":"` + yesterday + `T00:00:00+08:00","score":0}]}`,
			true,
		},
		{
			"a positive score on yesterday",
			`{"cells":[{"date":"` + yesterday + `","score":1}]}`,
			false,
		},
		{
			"a day that is not in the heatmap at all",
			`{"cells":[{"date":"2000-01-01","score":0}]}`,
			false,
		},
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

			got, err := c.HeatmapYesterdayMissed(context.Background(), a)
			if err != nil {
				t.Fatalf("HeatmapYesterdayMissed: %v", err)
			}
			if got != tc.want {
				t.Fatalf("missed = %v, want %v", got, tc.want)
			}
			req := wbIdentity(t, seen, a)
			if req.Method != http.MethodGet || req.URL.Path != heatmapPath {
				t.Fatalf("%s %s, want GET %s", req.Method, req.URL.Path, heatmapPath)
			}
		})
	}
}

func TestWorkbuddyUseMakeupCardPostsTheTargetDate(t *testing.T) {
	var seen *http.Request
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		seen = req
		return jsonResponse(200, wbEnvelope(`{}`)), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())
	a := wbCNAuth(t, c)

	if err := c.UseMakeupCard(context.Background(), a, "2026-01-31"); err != nil {
		t.Fatalf("UseMakeupCard: %v", err)
	}
	req := wbIdentity(t, seen, a)
	if req.Method != http.MethodPost || req.URL.Path != makeupCardUsePath {
		t.Fatalf("%s %s, want POST %s", req.Method, req.URL.Path, makeupCardUsePath)
	}
	body := wbBody(t, req)
	if body["target_date"] != "2026-01-31" {
		t.Fatalf("target_date = %v, want 2026-01-31", body["target_date"])
	}
}

func TestWorkbuddyUseMakeupCardSurfacesTheNoCardRefusal(t *testing.T) {
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		return jsonResponse(200, wbRefusal(13001, "no makeup card left")), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())

	err := c.UseMakeupCard(context.Background(), wbCNAuth(t, c), "2026-01-31")
	if err == nil {
		t.Fatal("UseMakeupCard with no card returned no error")
	}
	if !strings.Contains(err.Error(), "13001") {
		t.Fatalf("error = %q, want it to name the business code", err)
	}
}
