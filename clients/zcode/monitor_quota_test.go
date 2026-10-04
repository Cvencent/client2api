package zcode

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// The monitor endpoint is where the vendor reports the rolling 5-hour and
// daily windows.  The plan-billing endpoint only knows token grants, so a
// client that reads only the latter shows "no quota" for an account that the
// vendor's own desktop client renders as "5h 88/100".
const monitorQuotaFixture = `{
  "code": 0,
  "msg": "",
  "data": {
    "level": "pro",
    "limits": [
      {
        "type": "TIME_LIMIT",
        "unit": 3,
        "number": 5,
        "usage": 100,
        "currentValue": 12,
        "remaining": 88,
        "percentage": 12,
        "nextResetTime": 1790712000000
      },
      {
        "type": "TOKENS_LIMIT",
        "unit": 4,
        "number": 1,
        "usage": 3000000,
        "currentValue": 750000,
        "remaining": 2250000,
        "percentage": 25,
        "nextResetTime": 1790712000000
      }
    ]
  }
}`

const subscriptionFixture = `{
  "code": 0,
  "data": [
    {
      "productId": "coding-plan-pro",
      "productName": "GLM Coding Pro",
      "inCurrentPeriod": true,
      "status": "VALID",
      "expires_at": 1790956799
    }
  ]
}`

// monitorQuotaRoutes answers the two calls the monitor channel makes.  The
// reference reads the subscription list only to label the plan tier and its
// expiry; the limits themselves come from quota/limit.
func monitorQuotaRoutes() map[string]func(*http.Request) (*http.Response, error) {
	return map[string]func(*http.Request) (*http.Response, error){
		monitorQuotaPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, monitorQuotaFixture), nil
		},
		subscriptionPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, subscriptionFixture), nil
		},
	}
}

// addBigmodelAPIKey adds the credential the monitor endpoint needs: the
// coding-plan API key, not the plan JWT.  z-Switch uses this key for the
// rolling-window monitor and the JWT for plan billing.
func addBigmodelAPIKey(t *testing.T, c *Client, userID string) string {
	t.Helper()
	rec, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{
		fieldKind:   kindAPIKey,
		fieldAPIKey: userID + ".abcdefghijklmnop",
		fieldRegion: regionBigmodel,
	}})
	if err != nil {
		t.Fatalf("AddAccount(api-key): %v", err)
	}
	return rec.ID
}

// TestMonitorQuotaParsesTheFiveHourWindow pins the shape the panel needs: the
// 5-hour row is a duration limit in minutes, and the daily row is a token
// limit.  A parser that only understands plan balances cannot see either.
func TestMonitorQuotaParsesTheFiveHourWindow(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, routeTransport(t, monitorQuotaRoutes()))
	id := addBigmodelAPIKey(t, c, "61161790588087632")

	bal, err := c.AccountBalance(context.Background(), id, 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Unit != balanceUnit {
		t.Fatalf("unit = %q, want %q", bal.Unit, balanceUnit)
	}
	if bal.Credits != 88 {
		t.Fatalf("credits = %d, want the 5h window's remaining 88", bal.Credits)
	}
	if bal.Total != 100 {
		t.Fatalf("total = %d, want the 5h window's 100", bal.Total)
	}

	report, err := c.AccountPackages(context.Background(), id)
	if err != nil {
		t.Fatalf("AccountPackages: %v", err)
	}
	var fiveHour, daily *core.CreditPackage
	for i := range report.Packages {
		p := &report.Packages[i]
		switch p.SubProductCode {
		case "monitor:TIME_LIMIT":
			fiveHour = p
		case "monitor:TOKENS_LIMIT":
			daily = p
		}
	}
	if fiveHour == nil || daily == nil {
		t.Fatalf("packages = %+v, want both the 5h and daily monitor rows", report.Packages)
	}
	if fiveHour.Remain != 88 || fiveHour.Size != 100 {
		t.Errorf("5h package = %+v, want remain 88 / size 100", *fiveHour)
	}
	if daily.Remain != 2250000 || daily.Size != 3000000 {
		t.Errorf("daily package = %+v, want remain 2250000 / size 3000000", *daily)
	}
	if fiveHour.EndTime == "" || daily.EndTime == "" {
		t.Errorf("monitor rows must carry their reset time: 5h=%q daily=%q", fiveHour.EndTime, daily.EndTime)
	}
}

// TestMonitorQuotaKeepsTheSubscriptionTierAndExpiry checks the metadata the
// panel shows beside the numbers.  The reference reads productName/status from
// subscription/list, not from the limits array.
func TestMonitorQuotaKeepsTheSubscriptionTierAndExpiry(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, routeTransport(t, monitorQuotaRoutes()))
	id := addBigmodelAPIKey(t, c, "61161790588087632")

	bal, err := c.AccountBalance(context.Background(), id, time.Hour)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.EarliestAt.IsZero() {
		t.Fatal("a monitor row with nextResetTime must report its reset as EarliestAt")
	}
	report, err := c.AccountPackages(context.Background(), id)
	if err != nil {
		t.Fatalf("AccountPackages: %v", err)
	}
	if len(report.Packages) == 0 || report.Packages[0].Name == "" {
		t.Fatalf("packages = %+v, want named rows", report.Packages)
	}
}

// TestMonitorQuotaUsesTheBigmodelHeaderSet pins the request shape measured by
// the reference: open.bigmodel.cn wants a bearer token, not the ZCode identity
// header block.  Sending the desktop-client headers makes the vendor answer
// "current user has no coding plan" even when the key is a coding-plan key.
func TestMonitorQuotaUsesTheBigmodelHeaderSet(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, routeTransport(t, map[string]func(*http.Request) (*http.Response, error){
		monitorQuotaPath: func(r *http.Request) (*http.Response, error) {
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				return jsonResponse(http.StatusOK, `{"code":500,"msg":"missing bearer"}`), nil
			}
			if r.Header.Get("X-ZCode-App-Version") != "" || r.Header.Get("HTTP-Referer") != "" {
				return jsonResponse(http.StatusOK, `{"code":500,"msg":"wrong header family"}`), nil
			}
			if r.Header.Get("User-Agent") == "" || r.Header.Get("x-request-id") == "" {
				return jsonResponse(http.StatusOK, `{"code":500,"msg":"missing client headers"}`), nil
			}
			return jsonResponse(http.StatusOK, monitorQuotaFixture), nil
		},
		subscriptionPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, subscriptionFixture), nil
		},
	}))
	id := addBigmodelAPIKey(t, c, "61161790588087632")
	if _, err := c.AccountBalance(context.Background(), id, 0); err != nil {
		t.Fatalf("AccountBalance with the bigmodel header set: %v", err)
	}
}

// TestAccountBalanceMergesTheJWTsMonitorAndBillingChannels is the channel
// regression: one Zhipu account can expose a plan JWT and a coding-plan API
// key.  The reference probes both and merges what each can answer, instead of
// pretending the API-key channel has no quota.
func TestAccountBalanceMergesTheJWTsMonitorAndBillingChannels(t *testing.T) {
	const userID = "61161790588087632"
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, routeTransport(t, map[string]func(*http.Request) (*http.Response, error){
		monitorQuotaPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, monitorQuotaFixture), nil
		},
		subscriptionPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, subscriptionFixture), nil
		},
		planBalancePath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, claimBalanceFixture), nil
		},
	}))
	jwtID := addJWTAccountWithUser(t, c, userID)
	keyID := addBigmodelAPIKey(t, c, userID)

	// The API-key row alone must see the monitor windows.
	bal, err := c.AccountBalance(context.Background(), keyID, 0)
	if err != nil {
		t.Fatalf("AccountBalance(api-key): %v", err)
	}
	if bal.Credits != 88 {
		t.Fatalf("api-key credits = %d, want the monitor 5h remainder 88", bal.Credits)
	}

	// Asking through the JWT channel must merge in the sibling API-key
	// channel's monitor rows, because the panel groups them as one account.
	report, err := c.AccountPackages(context.Background(), jwtID)
	if err != nil {
		t.Fatalf("AccountPackages(jwt): %v", err)
	}
	var sawMonitor, sawBilling bool
	for _, p := range report.Packages {
		if p.SubProductCode == "monitor:TIME_LIMIT" {
			sawMonitor = true
		}
		if p.SubProductCode == "e1" {
			sawBilling = true
		}
	}
	if !sawMonitor || !sawBilling {
		t.Fatalf("packages = %+v, want both monitor and billing rows", report.Packages)
	}
}

func addJWTAccountWithUser(t *testing.T, c *Client, userID string) string {
	t.Helper()
	rec, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{
		fieldKind:   kindJWT,
		fieldJWT:    makeJWT(`{"user_id":"` + userID + `"}`),
		fieldRegion: regionBigmodel,
	}})
	if err != nil {
		t.Fatalf("AddAccount(jwt): %v", err)
	}
	return rec.ID
}
