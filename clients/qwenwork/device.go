package qwenwork

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"client2api/internal/core"
)

// device.go implements the PKCE (S256) device-authorisation flow that mints a
// credential for gateway.qwenwork.cn.
//
// There is no login subcommand in cmd/ (this module may not add one), so the
// flow is reachable two ways:
//
//   - RunDeviceFlow is exported and self-contained: a future `client2api -login
//     qwenwork` would call it with the same core.Deps.
//   - Setting "login": true in the client config (or
//     CLIENT2API_QWENWORK_LOGIN=1) makes New start it in the background and
//     print the URL, which is enough for an operator watching the log.
//
// The flow needs a human to open the URL and click "authorise"; nothing here
// can complete it on its own.

const (
	// defaultClientID and defaultRedirectURI are the first-party desktop app's.
	// Reusing them is what carries the account-ban risk noted in the README;
	// they are overridable in the config so an operator can register their own.
	defaultClientID    = "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb"
	defaultRedirectURI = "qwenwork-cn://"
	deviceAuthPath     = "/device/selectAccounts"

	// pkceAlphabet is the RFC 7636 unreserved character set.
	pkceAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	// pkceVerifierLen is the RFC 7636 maximum (43..128); the reference uses 64.
	pkceVerifierLen = 64
)

// newPKCEVerifier returns a fresh RFC 7636 code verifier.
func newPKCEVerifier() (string, error) {
	buf := make([]byte, pkceVerifierLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("qwenwork: generating PKCE verifier: %w", err)
	}
	var b strings.Builder
	b.Grow(pkceVerifierLen)
	for _, v := range buf {
		// Modulo bias over a 64-character alphabet from 256 values is nil
		// (256 = 4*64), so this is uniform.
		b.WriteByte(pkceAlphabet[int(v)%len(pkceAlphabet)])
	}
	return b.String(), nil
}

// pkceChallengeS256 derives the S256 challenge: base64url(sha256(verifier))
// with no padding, exactly as RFC 7636 section 4.2 specifies.
func pkceChallengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// deviceAuthURL builds the URL the human opens.
func deviceAuthURL(base, challenge, nonce, machineID, clientID, redirectURI string) string {
	q := url.Values{
		"challenge":        {challenge},
		"challenge_method": {"S256"},
		"nonce":            {nonce},
		"machine_id":       {machineID},
		"client_id":        {clientID},
		"redirect_uri":     {redirectURI},
	}.Encode()
	return strings.TrimRight(base, "/") + deviceAuthPath + "?" + q
}

// loginRecord is an authorisation in progress.  It lives in Deps.DataDir so
// Status can tell an operator which URL to open, and so a restart does not lose
// a flow the human is halfway through.
type loginRecord struct {
	URL       string `json:"url"`
	Nonce     string `json:"nonce"`
	Verifier  string `json:"verifier"`
	StartedAt int64  `json:"started_at"`
	ExpiresAt int64  `json:"expires_at"`
}

// loginState returns the pending authorisation, or nil when there is none or it
// has expired.  It reads one small file: Status calls it, so it must be cheap.
func (c *Client) loginState() *loginRecord {
	if c.loginPath == "" {
		return nil
	}
	var rec loginRecord
	if err := core.ReadJSON(c.loginPath, &rec); err != nil {
		return nil
	}
	if rec.URL == "" || (rec.ExpiresAt > 0 && rec.ExpiresAt <= time.Now().Unix()) {
		return nil
	}
	return &rec
}

// RunDeviceFlow runs one complete PKCE authorisation: it prints the URL, polls
// until the human authorises, and stores the credential in Deps.DataDir.
//
// It is exported so a future CLI subcommand can call it; it never panics and
// returns an error rather than blocking forever.
func RunDeviceFlow(ctx context.Context, deps core.Deps, out io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if out == nil {
		out = os.Stdout
	}
	cfg, err := parseConfig(deps.Config)
	if err != nil {
		deps.Log("qwenwork: invalid config, using defaults: %v", err)
		cfg = config{}
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.loginTimeout())
		defer cancel()
	}

	c := &Client{deps: deps, cfg: cfg}
	c.setPaths()

	verifier, err := newPKCEVerifier()
	if err != nil {
		return err
	}
	nonce := newUUID()
	machineID := newUUID()
	authURL := deviceAuthURL(c.cfg.baseURL(), pkceChallengeS256(verifier), nonce, machineID, c.cfg.clientID(), c.cfg.redirectURI())

	rec := loginRecord{
		URL:       authURL,
		Nonce:     nonce,
		Verifier:  verifier,
		StartedAt: time.Now().Unix(),
		ExpiresAt: time.Now().Add(c.cfg.loginTimeout()).Unix(),
	}
	if c.loginPath != "" {
		if err := core.WriteJSONAtomic(c.loginPath, rec); err != nil {
			deps.Log("qwenwork: writing %s: %v", filepath.Base(c.loginPath), err)
		}
	}

	fmt.Fprintf(out, "qwenwork: open this URL in a browser and authorise access:\n  %s\n", authURL)
	fmt.Fprintf(out, "qwenwork: waiting for authorisation (polling every %s, giving up in %s)...\n",
		c.cfg.pollInterval(), c.cfg.loginTimeout())

	interval := c.cfg.pollInterval()
	for {
		grant, pending, err := c.pollGrant(ctx, nonce, verifier)
		if err != nil {
			// A poll failure is not fatal: the human may still be clicking.
			deps.Log("qwenwork: poll: %v", err)
		}
		if !pending && err == nil {
			acct := account{
				UID:          grant.UserID,
				Nickname:     grant.UserName,
				AccessToken:  grant.accessToken(),
				RefreshToken: grant.RefreshToken,
				ExpiresAt:    grant.expiryUnix(time.Now()),
				CreatedAt:    time.Now().Unix(),
			}
			if err := upsertStoredAccount(c.accountsPath, acct); err != nil {
				return fmt.Errorf("qwenwork: storing credential: %w", err)
			}
			if c.loginPath != "" {
				if err := os.Remove(c.loginPath); err != nil && !errors.Is(err, os.ErrNotExist) {
					deps.Log("qwenwork: removing %s: %v", filepath.Base(c.loginPath), err)
				}
			}
			fmt.Fprintf(out, "qwenwork: authorised %s; credential stored\n", acct.label())
			return nil
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("qwenwork: device authorisation was not completed in time: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

// upsertStoredAccount merges one credential into accounts.json, matching on the
// stable account id so a re-login refreshes rather than duplicates.  A previous
// "disabled" verdict is cleared: a fresh authorisation means a live credential.
func upsertStoredAccount(path string, a account) error {
	if path == "" {
		return nil
	}
	var list []account
	if err := core.ReadJSON(path, &list); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	key := a.id()
	for i := range list {
		if list[i].id() == key || (a.UID != "" && list[i].UID == a.UID) {
			merged := mergeAccount(list[i], a)
			merged.Disabled = false
			merged.CooldownUntil = 0
			merged.LastError = ""
			if merged.ID == "" {
				merged.ID = list[i].ID
			}
			if merged.CreatedAt == 0 {
				merged.CreatedAt = list[i].CreatedAt
			}
			list[i] = merged
			return core.WriteJSONAtomic(path, list)
		}
	}
	list = append(list, a)
	return core.WriteJSONAtomic(path, list)
}
