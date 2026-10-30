package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"client2api/internal/core"
)

const (
	defaultQuotaPerUSD    = 500000.0
	openAIBillingSentinel = 100000000.0
)

// AccountBalance reads one custom relay account's remaining credit. The
// protocol is selected per platform, with an automatic probe order chosen to
// cover the common New API, Sub2API and OpenAI Billing deployments.
//
// A probe that fails because the vendor rejected the credential is fed back
// into the pool.  The sweep is the only thing that observes a key nobody is
// chatting through anymore, so without this a revoked key sits at "ready" in
// the panel and the sweep keeps asking it every five minutes -- which is
// exactly the recurring-log shape an operator reads as "the platform is
// broken".
func (s *source) AccountBalance(ctx context.Context, id string, soon time.Duration) (core.Balance, error) {
	bal, err := s.accountBalance(ctx, id, soon)
	s.noteBalanceVerdict(id, err)
	return bal, err
}

func (s *source) accountBalance(ctx context.Context, id string, _ time.Duration) (core.Balance, error) {
	s.mu.Lock()
	var account storedProvider
	found := false
	for _, row := range s.rows {
		if row.ID == id {
			account = row
			found = true
			break
		}
	}
	cfg := s.cfg
	s.mu.Unlock()
	if !found {
		return core.Balance{}, fmt.Errorf("account %q not found", id)
	}

	protocol := strings.ToLower(strings.TrimSpace(cfg.BalanceProtocol))
	if protocol == "" {
		protocol = "auto"
	}
	switch protocol {
	case "none":
		return core.Balance{}, fmt.Errorf("balance queries are disabled for this platform")
	case "newapi":
		return s.readNewAPIWithConsoleFallback(ctx, cfg, account)
	case "sub2api":
		return s.readSub2APIUsage(ctx, cfg.BaseURL, account)
	case "openai_billing":
		return s.readOpenAIBilling(ctx, cfg.BaseURL, account)
	case "auto":
		return s.readAutoBalance(ctx, cfg, account)
	default:
		return core.Balance{}, fmt.Errorf("unknown balance protocol %q", protocol)
	}
}

// noteBalanceVerdict feeds a probe's terminal answer into the pool.
//
// Only an explicit credential rejection moves anything.  Everything else -- a
// missing billing endpoint, a 5xx, a decode failure on an HTML page -- is news
// about the relay's surface, not about the key, and treating it as a failure
// would park working accounts because they happen not to report a balance.
//
// A rejection parks on the first observation, not after three: the threshold
// exists so one transient blip does not cost capacity to the chat path, but a
// vendor that answers "the AppKey is invalid" while reading a balance is not
// blipping -- the same answer will come back on every request.
func (s *source) noteBalanceVerdict(id string, err error) {
	if err == nil || s.pool == nil {
		return
	}
	if !containsCredentialRejection(err) {
		return
	}
	msg := truncate(core.Redact(err.Error()), 240)
	window := s.cfg.CooldownFor(core.FailureAuth, sourceDefaultCooldown(core.FailureAuth))
	s.pool.noteFailure(id, 1, s.now().Add(window), msg)
	s.mu.Lock()
	if s.invalid == nil {
		s.invalid = map[string]bool{}
	}
	s.invalid[id] = true
	s.lastErr = msg
	if window > 0 {
		if s.cooling == nil {
			s.cooling = map[string]time.Time{}
		}
		s.cooling[id] = s.now().Add(window)
	}
	s.mu.Unlock()
}

// containsCredentialRejection walks the joined error tree looking for a
// balanceHTTPError that names a dead credential.
func containsCredentialRejection(err error) bool {
	if err == nil {
		return false
	}
	var he *balanceHTTPError
	if errors.As(err, &he) {
		return he.fatalCredential()
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, inner := range joined.Unwrap() {
			if containsCredentialRejection(inner) {
				return true
			}
		}
	}
	return false
}

func (s *source) readAutoBalance(ctx context.Context, cfg core.SourceConfig, account storedProvider) (core.Balance, error) {
	// A relay that exposes no billing endpoint at all answers every probe with
	// a 404 (or an HTML page where JSON was expected).  Probing it every five
	// minutes forever is what fills the log with four errors per platform per
	// sweep, so a relay that refuses all four protocols is remembered and
	// skipped until the window lapses.  The window is deliberately hours, not
	// minutes: a relay's billing surface does not appear mid-session, and the
	// cost of being wrong is one missed balance reading, not a missed request.
	if until, known := s.balanceUnsupportedUntil(cfg.BaseURL); known {
		return core.Balance{}, fmt.Errorf("balance endpoint unsupported by this relay (probed again after %s)", until.Format(time.RFC3339))
	}
	var failures []error
	if bal, err := s.readNewAPITokenUsage(ctx, cfg, account); err == nil {
		s.clearBalanceProbe(cfg.BaseURL)
		return bal, nil
	} else {
		failures = append(failures, fmt.Errorf("New API token usage: %w", err))
	}
	if bal, err := s.readSub2APIUsage(ctx, cfg.BaseURL, account); err == nil {
		s.clearBalanceProbe(cfg.BaseURL)
		return bal, nil
	} else {
		failures = append(failures, fmt.Errorf("Sub2API usage: %w", err))
	}
	if account.BalanceAccessToken != "" && account.BalanceUserID != "" {
		if bal, err := s.readNewAPIConsole(ctx, cfg, account); err == nil {
			s.clearBalanceProbe(cfg.BaseURL)
			return bal, nil
		} else {
			failures = append(failures, fmt.Errorf("New API console: %w", err))
		}
	}
	if bal, err := s.readOpenAIBilling(ctx, cfg.BaseURL, account); err == nil {
		s.clearBalanceProbe(cfg.BaseURL)
		return bal, nil
	} else {
		failures = append(failures, fmt.Errorf("OpenAI billing: %w", err))
	}
	s.noteBalanceProbe(cfg.BaseURL)
	return core.Balance{}, errors.Join(failures...)
}

// balanceProbeTTL is how long a relay that refused every billing protocol is
// left alone.  Six hours is longer than any single credential's cooldown and
// far shorter than a day, so a relay that gains a billing endpoint is picked up
// the same working day.
const balanceProbeTTL = 6 * time.Hour

// balanceUnsupportedUntil reports whether a relay is inside its negative-cache
// window, plus when that window ends.
func (s *source) balanceUnsupportedUntil(base string) (time.Time, bool) {
	key := balanceProbeKey(base)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.balanceProbe == nil {
		return time.Time{}, false
	}
	state, ok := s.balanceProbe[key]
	if !ok || state.until.IsZero() {
		return time.Time{}, false
	}
	if s.now().Before(state.until) {
		return state.until, true
	}
	// Lapsed: forget it so the next probe really goes out.
	delete(s.balanceProbe, key)
	return time.Time{}, false
}

// noteBalanceProbe records that a relay refused every protocol.
func (s *source) noteBalanceProbe(base string) {
	key := balanceProbeKey(base)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.balanceProbe == nil {
		s.balanceProbe = map[string]balanceProbeState{}
	}
	s.balanceProbe[key] = balanceProbeState{until: s.now().Add(balanceProbeTTL)}
}

// clearBalanceProbe drops the negative entry the moment a protocol answers, so
// a relay that starts reporting is probed normally from then on.
func (s *source) clearBalanceProbe(base string) {
	key := balanceProbeKey(base)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.balanceProbe != nil {
		delete(s.balanceProbe, key)
	}
}

// balanceProbeKey normalises a base URL so trailing slashes and case do not
// create two entries for one relay.
func balanceProbeKey(base string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(base), "/"))
}

func (s *source) readNewAPIWithConsoleFallback(ctx context.Context, cfg core.SourceConfig, account storedProvider) (core.Balance, error) {
	bal, err := s.readNewAPITokenUsage(ctx, cfg, account)
	if err == nil || account.BalanceAccessToken == "" || account.BalanceUserID == "" {
		return bal, err
	}
	console, consoleErr := s.readNewAPIConsole(ctx, cfg, account)
	if consoleErr == nil {
		return console, nil
	}
	return core.Balance{}, errors.Join(err, consoleErr)
}

func (s *source) readNewAPITokenUsage(ctx context.Context, cfg core.SourceConfig, account storedProvider) (core.Balance, error) {
	endpoint, err := newAPIEndpoint(cfg.BaseURL, "/api/usage/token/")
	if err != nil {
		return core.Balance{}, err
	}
	req, err := http.NewRequestWithContext(ctxOrBackground(ctx), http.MethodGet, endpoint, nil)
	if err != nil {
		return core.Balance{}, err
	}
	prov := storedToConfig(account)
	prov.BaseURL = cfg.BaseURL
	prov.applyHeaders(req)
	raw, err := s.doBalanceRequest(req)
	if err != nil {
		return core.Balance{}, err
	}
	var env struct {
		Data struct {
			TotalGranted   json.Number `json:"total_granted"`
			TotalUsed      json.Number `json:"total_used"`
			TotalAvailable json.Number `json:"total_available"`
			UnlimitedQuota bool        `json:"unlimited_quota"`
		} `json:"data"`
	}
	if err := decodeJSONNumbers(raw, &env); err != nil {
		return core.Balance{}, fmt.Errorf("decode New API token usage: %w", err)
	}
	granted, err := numberInt(env.Data.TotalGranted)
	if err != nil {
		return core.Balance{}, fmt.Errorf("decode New API total_granted: %w", err)
	}
	usedRaw, err := numberInt(env.Data.TotalUsed)
	if err != nil {
		return core.Balance{}, fmt.Errorf("decode New API total_used: %w", err)
	}
	available, err := numberInt(env.Data.TotalAvailable)
	if err != nil {
		return core.Balance{}, fmt.Errorf("decode New API total_available: %w", err)
	}
	quota := quotaPerUSD(cfg)
	unlimited := env.Data.UnlimitedQuota || available < 0
	if available < 0 {
		available = 0
	}
	return core.Balance{
		Credits:   available,
		Used:      float64(usedRaw) / quota,
		Total:     granted,
		Scale:     1 / quota,
		Unlimited: unlimited,
		Unit:      "USD",
	}, nil
}

func (s *source) readNewAPIConsole(ctx context.Context, cfg core.SourceConfig, account storedProvider) (core.Balance, error) {
	if account.BalanceAccessToken == "" || account.BalanceUserID == "" {
		return core.Balance{}, fmt.Errorf("balance_access_token and balance_user_id are required")
	}
	endpoint, err := newAPIEndpoint(cfg.BaseURL, "/api/user/self")
	if err != nil {
		return core.Balance{}, err
	}
	req, err := http.NewRequestWithContext(ctxOrBackground(ctx), http.MethodGet, endpoint, nil)
	if err != nil {
		return core.Balance{}, err
	}
	req.Header.Set("Authorization", "Bearer "+account.BalanceAccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "client2api/1.0 (+openai-compat)")
	for _, name := range []string{"New-API-User", "Veloera-User", "X-Api-User", "voapi-user", "User-id"} {
		req.Header.Set(name, account.BalanceUserID)
	}
	raw, err := s.doBalanceRequest(req)
	if err != nil {
		return core.Balance{}, err
	}
	var env struct {
		Data struct {
			Quota     json.Number `json:"quota"`
			UsedQuota json.Number `json:"used_quota"`
		} `json:"data"`
	}
	if err := decodeJSONNumbers(raw, &env); err != nil {
		return core.Balance{}, fmt.Errorf("decode New API console balance: %w", err)
	}
	credits, err := numberInt(env.Data.Quota)
	if err != nil {
		return core.Balance{}, fmt.Errorf("decode New API console quota: %w", err)
	}
	usedRaw, err := numberInt(env.Data.UsedQuota)
	if err != nil {
		return core.Balance{}, fmt.Errorf("decode New API console used_quota: %w", err)
	}
	quota := quotaPerUSD(cfg)
	return core.Balance{
		Credits: credits,
		Used:    float64(usedRaw) / quota,
		Total:   credits + usedRaw,
		Scale:   1 / quota,
		Unit:    "USD",
	}, nil
}

// Sub2API instances report real US dollars. Store micro-USD so the shared
// Balance type keeps integer credits and the panel applies Scale on display.
func (s *source) readSub2APIUsage(ctx context.Context, base string, account storedProvider) (core.Balance, error) {
	endpoint, err := versionedV1Endpoint(base, "/usage")
	if err != nil {
		return core.Balance{}, err
	}
	req, err := http.NewRequestWithContext(ctxOrBackground(ctx), http.MethodGet, endpoint, nil)
	if err != nil {
		return core.Balance{}, err
	}
	prov := storedToConfig(account)
	prov.BaseURL = base
	prov.applyHeaders(req)
	raw, err := s.doBalanceRequest(req)
	if err != nil {
		return core.Balance{}, err
	}
	var env struct {
		Remaining json.Number `json:"remaining"`
		Balance   json.Number `json:"balance"`
		Used      json.Number `json:"used"`
		Total     json.Number `json:"total"`
		Data      *struct {
			Remaining json.Number `json:"remaining"`
			Balance   json.Number `json:"balance"`
			Used      json.Number `json:"used"`
			Total     json.Number `json:"total"`
		} `json:"data"`
	}
	if err := decodeJSONNumbers(raw, &env); err != nil {
		return core.Balance{}, fmt.Errorf("decode Sub2API usage: %w", err)
	}
	remaining, used, total := env.Remaining, env.Used, env.Total
	if env.Data != nil {
		if strings.TrimSpace(remaining.String()) == "" {
			remaining = env.Data.Remaining
		}
		if strings.TrimSpace(remaining.String()) == "" {
			remaining = env.Data.Balance
		}
		if strings.TrimSpace(used.String()) == "" {
			used = env.Data.Used
		}
		if strings.TrimSpace(total.String()) == "" {
			total = env.Data.Total
		}
	}
	if strings.TrimSpace(remaining.String()) == "" {
		remaining = env.Balance
	}
	remainingUSD, err := numberFloat(remaining)
	if err != nil {
		return core.Balance{}, fmt.Errorf("decode Sub2API remaining: %w", err)
	}
	usedUSD, _ := numberFloat(used)
	totalUSD, _ := numberFloat(total)
	if totalUSD == 0 {
		totalUSD = remainingUSD + usedUSD
	}
	return core.Balance{
		Credits: microUSD(remainingUSD),
		Used:    usedUSD,
		Total:   microUSD(totalUSD),
		Scale:   1e-6,
		Unit:    "USD",
	}, nil
}

func (s *source) readOpenAIBilling(ctx context.Context, base string, account storedProvider) (core.Balance, error) {
	grants, grantsErr := s.readOpenAICreditGrants(ctx, base, account)
	hardLimit, subscriptionErr := s.readOpenAISubscription(ctx, base, account)
	if grantsErr != nil && subscriptionErr != nil {
		return core.Balance{}, errors.Join(grantsErr, subscriptionErr)
	}

	totalUSD := grants.totalGranted
	if hardLimit > 0 && hardLimit < openAIBillingSentinel {
		totalUSD = hardLimit
	}
	remainingUSD := grants.totalAvailable
	if !grants.hasAvailable && hardLimit > 0 && hardLimit < openAIBillingSentinel {
		remainingUSD = hardLimit - grants.totalUsed
	}
	usedUSD := grants.totalUsed
	if usedUSD == 0 && totalUSD > 0 && remainingUSD > 0 {
		usedUSD = totalUSD - remainingUSD
	}
	if remainingUSD < 0 {
		remainingUSD = 0
	}
	return core.Balance{
		Credits: microUSD(remainingUSD),
		Used:    usedUSD,
		Total:   microUSD(totalUSD),
		Scale:   1e-6,
		Unit:    "USD",
	}, nil
}

type openAICreditGrants struct {
	totalGranted   float64
	totalUsed      float64
	totalAvailable float64
	hasAvailable   bool
}

func (s *source) readOpenAICreditGrants(ctx context.Context, base string, account storedProvider) (openAICreditGrants, error) {
	endpoint, err := originEndpoint(base, "/dashboard/billing/credit_grants")
	if err != nil {
		return openAICreditGrants{}, err
	}
	req, err := http.NewRequestWithContext(ctxOrBackground(ctx), http.MethodGet, endpoint, nil)
	if err != nil {
		return openAICreditGrants{}, err
	}
	prov := storedToConfig(account)
	prov.BaseURL = base
	prov.applyHeaders(req)
	raw, err := s.doBalanceRequest(req)
	if err != nil {
		return openAICreditGrants{}, err
	}
	var env struct {
		TotalGranted   json.Number `json:"total_granted"`
		TotalUsed      json.Number `json:"total_used"`
		TotalAvailable json.Number `json:"total_available"`
	}
	if err := decodeJSONNumbers(raw, &env); err != nil {
		return openAICreditGrants{}, fmt.Errorf("decode OpenAI credit grants: %w", err)
	}
	granted, err := numberFloat(env.TotalGranted)
	if err != nil {
		return openAICreditGrants{}, err
	}
	used, err := numberFloat(env.TotalUsed)
	if err != nil {
		return openAICreditGrants{}, err
	}
	available, availErr := numberFloat(env.TotalAvailable)
	return openAICreditGrants{
		totalGranted:   granted,
		totalUsed:      used,
		totalAvailable: available,
		hasAvailable:   availErr == nil && strings.TrimSpace(env.TotalAvailable.String()) != "",
	}, nil
}

func (s *source) readOpenAISubscription(ctx context.Context, base string, account storedProvider) (float64, error) {
	endpoint, err := originEndpoint(base, "/dashboard/billing/subscription")
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctxOrBackground(ctx), http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	prov := storedToConfig(account)
	prov.BaseURL = base
	prov.applyHeaders(req)
	raw, err := s.doBalanceRequest(req)
	if err != nil {
		return 0, err
	}
	var env struct {
		HardLimitUSD json.Number `json:"hard_limit_usd"`
	}
	if err := decodeJSONNumbers(raw, &env); err != nil {
		return 0, fmt.Errorf("decode OpenAI subscription: %w", err)
	}
	limit, err := numberFloat(env.HardLimitUSD)
	if err != nil {
		return 0, err
	}
	if limit <= 0 || limit >= openAIBillingSentinel {
		return 0, nil
	}
	return limit, nil
}

// balanceHTTPError is a non-200 answer from a billing endpoint, carrying the
// status so the caller can tell "this relay has no billing surface" (404, 501,
// an HTML page) from "this credential is dead" (401, a 403 that names the key).
//
// The distinction is what lets a balance probe feed the pool.  A 404 says
// nothing about the account and must never park it; an expired AppKey or a
// banned key says everything and must, or the row keeps showing ready in the
// panel while every request against it fails.
type balanceHTTPError struct {
	status int
	text   string
}

func (e *balanceHTTPError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("HTTP %d: %s", e.status, e.text)
}

// fatalCredential reports whether this answer is the vendor rejecting the
// credential itself, as opposed to the endpoint being absent or the account
// being out of money.
func (e *balanceHTTPError) fatalCredential() bool {
	if e == nil {
		return false
	}
	// A 404 or a 5xx is about the endpoint, not the key.
	if e.status == http.StatusNotFound || e.status >= 500 {
		return false
	}
	switch classifyFailure(e.status, e.text) {
	case kindAuth:
		return true
	}
	return false
}

func (s *source) doBalanceRequest(req *http.Request) ([]byte, error) {
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxErrorBytes {
		return nil, fmt.Errorf("balance response is too large")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &balanceHTTPError{status: resp.StatusCode, text: errorTextOf(raw)}
	}
	return raw, nil
}

func newAPIEndpoint(base, path string) (string, error) {
	return originEndpoint(base, path)
}

func originEndpoint(base, path string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid source base URL %q", base)
	}
	u.Path = path
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func versionedV1Endpoint(base, suffix string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid source base URL %q", base)
	}
	path := strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(path, "/v1") {
		path += "/v1"
	}
	u.Path = path + "/" + strings.TrimLeft(suffix, "/")
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func decodeJSONNumbers(raw []byte, out any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	return dec.Decode(out)
}

func numberInt(n json.Number) (int64, error) {
	if strings.TrimSpace(n.String()) == "" {
		return 0, fmt.Errorf("missing number")
	}
	if i, err := n.Int64(); err == nil {
		return i, nil
	}
	f, err := n.Float64()
	if err != nil {
		return 0, err
	}
	return int64(math.Round(f)), nil
}

func numberFloat(n json.Number) (float64, error) {
	if strings.TrimSpace(n.String()) == "" {
		return 0, fmt.Errorf("missing number")
	}
	f, err := strconv.ParseFloat(n.String(), 64)
	if err != nil {
		return 0, err
	}
	return f, nil
}

func microUSD(v float64) int64 {
	return int64(math.Round(v * 1e6))
}

func quotaPerUSD(cfg core.SourceConfig) float64 {
	if cfg.BalanceQuotaPerUSD > 0 && !math.IsNaN(cfg.BalanceQuotaPerUSD) && !math.IsInf(cfg.BalanceQuotaPerUSD, 0) {
		return cfg.BalanceQuotaPerUSD
	}
	return defaultQuotaPerUSD
}
