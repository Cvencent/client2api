package openrouter

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"client2api/internal/core"
)

// AccountBalance reports the credential's remaining credit.
//
// OpenRouter has two relevant endpoints:
//
//   - `GET /credits` → `{"data":{"total_credits":…,"total_usage":…}}`, which is
//     account-wide but documented as "Management key required" — an ordinary
//     inference key gets 403 there.
//   - `GET /key` → the per-key view, with `limit`, `limit_remaining` and
//     `usage`.  This is the fallback, and it is the preferred source when the
//     key actually has a spending limit.
//
// core.Balance carries int64 amounts, so USD is reported in CENTS and Unit says
// so.  A vendor refusal here is an ERROR rather than a zero: a wrong number on
// a credit screen is worse than no screen.
func (c *Client) AccountBalance(ctx context.Context, id string, soon time.Duration) (core.Balance, error) {
	c.ensure()
	ctx = ctxOrBackground(ctx)
	rec, ok := c.pool.byID(id)
	if !ok {
		return core.Balance{}, fmt.Errorf("account %q not found", id)
	}
	if rec.APIKey == "" {
		return core.Balance{}, fmt.Errorf("account %q has no API key", id)
	}

	creditsBody, creditsStatus, creditsErr := c.getJSON(ctx, rec, c.cfg.creditsURL())
	if creditsErr == nil && creditsStatus == http.StatusOK {
		if bal, ok := parseCredits(creditsBody); ok {
			c.noteSuccess(rec)
			return bal, nil
		}
	}

	keyBody, keyStatus, keyErr := c.getJSON(ctx, rec, c.cfg.keyURL())
	if keyErr == nil && keyStatus == http.StatusOK {
		if bal, ok := parseKeyBalance(keyBody); ok {
			c.noteSuccess(rec)
			return bal, nil
		}
		return core.Balance{}, fmt.Errorf(
			"openrouter: /key reports no spending limit for this credential (limit_remaining is null) and /credits is not available to it: %s",
			describeHTTPFailure(creditsStatus, creditsErr, creditsBody))
	}

	// Neither source worked: report the more specific of the two.
	if keyErr != nil {
		return core.Balance{}, keyErr
	}
	if keyStatus != http.StatusOK {
		c.noteFailure(rec.ID, classifyFailure(keyStatus, 0, errorTextOf(keyBody)), errorTextOf(keyBody))
		return core.Balance{}, fmt.Errorf("openrouter: balance lookup failed (HTTP %d): %s", keyStatus, errorTextOf(keyBody))
	}
	if creditsErr != nil {
		return core.Balance{}, creditsErr
	}
	if creditsStatus != http.StatusOK {
		c.noteFailure(rec.ID, classifyFailure(creditsStatus, 0, errorTextOf(creditsBody)), errorTextOf(creditsBody))
		return core.Balance{}, fmt.Errorf("openrouter: credit lookup failed (HTTP %d): %s", creditsStatus, errorTextOf(creditsBody))
	}
	return core.Balance{}, fmt.Errorf("openrouter: the vendor returned no usable balance")
}

// describeHTTPFailure renders whichever half of the credits attempt failed.
func describeHTTPFailure(status int, err error, body []byte) string {
	switch {
	case err != nil:
		return truncate(core.Redact(err.Error()), 200)
	case status != 0:
		// An empty body would otherwise render as a dangling "HTTP 500: ".
		if text := errorTextOf(body); text != "" {
			return fmt.Sprintf("HTTP %d: %s", status, text)
		}
		return fmt.Sprintf("HTTP %d", status)
	}
	return "no response"
}

// --- wire decoding --------------------------------------------------------

type creditsPayload struct {
	Data struct {
		TotalCredits *float64 `json:"total_credits"`
		TotalUsage   *float64 `json:"total_usage"`
	} `json:"data"`
}

// parseCredits decodes `GET /credits`.  Both fields are required by the
// vendor's schema; a body missing one is not a balance.
func parseCredits(body []byte) (core.Balance, bool) {
	var p creditsPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return core.Balance{}, false
	}
	if p.Data.TotalCredits == nil || p.Data.TotalUsage == nil {
		return core.Balance{}, false
	}
	total := usdCents(*p.Data.TotalCredits)
	used := usdCents(*p.Data.TotalUsage)
	bal := core.Balance{Total: total, Credits: total - used, Unit: balanceUnit}
	if bal.Credits < 0 {
		bal.Credits = 0
	}
	return bal, true
}

type keyPayload struct {
	Data struct {
		Label          *string  `json:"label"`
		Limit          *float64 `json:"limit"`
		LimitRemaining *float64 `json:"limit_remaining"`
		LimitReset     *string  `json:"limit_reset"`
		Usage          *float64 `json:"usage"`
		IsFreeTier     bool     `json:"is_free_tier"`
	} `json:"data"`
}

// parseKeyBalance decodes `GET /key`.  It reports ok=false when the key has no
// spending limit at all, because then there is no "remaining" number to show —
// inventing one would be a lie, and the caller turns that into an error.
func parseKeyBalance(body []byte) (core.Balance, bool) {
	var p keyPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return core.Balance{}, false
	}
	if p.Data.LimitRemaining == nil {
		return core.Balance{}, false
	}
	bal := core.Balance{Credits: usdCents(*p.Data.LimitRemaining), Unit: balanceUnit}
	if p.Data.Limit != nil {
		bal.Total = usdCents(*p.Data.Limit)
	}
	if bal.Credits < 0 {
		bal.Credits = 0
	}
	return bal, true
}

// balanceUnit says what the int64 amounts mean.  The vendor reports
// fractional dollars, so the module scales to cents to stay integral.
const balanceUnit = "USD cents"

func usdCents(v float64) int64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return int64(math.Round(v * 100))
}

// --- shared authenticated GET --------------------------------------------

// getJSON performs an authenticated GET and returns the body and status.  A
// non-2xx status is NOT an error here: the caller decides what it means (the
// credits/key pair treats 403 as "try the other endpoint").
func (c *Client) getJSON(ctx context.Context, rec accountRecord, url string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctxOrBackground(ctx), c.cfg.modelsTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("openrouter: build request: %w", err)
	}
	c.cfg.applyHeaders(req, rec.APIKey)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, 0, c.classifyUpstream(rec.ID, err)
	}
	defer resp.Body.Close()
	return readLimited(resp.Body, maxErrorBytes), resp.StatusCode, nil
}

// balanceNote is the one-line summary the panel shows next to a balance.
func balanceNote(b core.Balance) string {
	if b.Total <= 0 {
		return fmt.Sprintf("%d %s remaining", b.Credits, b.Unit)
	}
	return fmt.Sprintf("%d of %d %s remaining", b.Credits, b.Total, b.Unit)
}

// keyLabel is the vendor's own name for a key.  It is used only for display and
// is masked when the vendor simply echoed the key back (the documented example
// is literally "sk-or-v1-au7...890").
func keyLabel(body []byte) string {
	var p keyPayload
	if err := json.Unmarshal(body, &p); err != nil || p.Data.Label == nil {
		return ""
	}
	label := strings.TrimSpace(*p.Data.Label)
	if label == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(label), "sk-or-") {
		return core.MaskSecret(label)
	}
	return truncate(core.Redact(label), 80)
}
