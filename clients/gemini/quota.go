package gemini

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// quotaWindow is one rolling window the vendor reports.
type quotaWindow struct {
	BucketID          string
	Window            string
	ResetTime         string
	RemainingFraction float64
}

// fetchQuotaWindows reads the two Gemini buckets from
// retrieveUserQuotaSummary.  Any hard constraint that fails drops the bucket
// rather than inventing a value.
func (c *Client) fetchQuotaWindows(ctx context.Context, acct *accountRecord) (fiveHour, weekly *quotaWindow, err error) {
	project := firstNonEmpty(acct.Project, c.cfg.Project, defaultProject)
	body := map[string]any{"project": project}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.quotaTimeout())
	defer cancel()
	status, raw, derr := c.doJSON(ctx, endpointSandbox, quotaPath, acct.AccessToken, body, false)
	if derr != nil {
		return nil, nil, derr
	}
	if status < 200 || status >= 300 {
		return nil, nil, quotaRefusal(status, raw)
	}
	var payload struct {
		Groups []struct {
			Buckets []json.RawMessage `json:"buckets"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, nil, err
	}
	for _, g := range payload.Groups {
		for _, b := range g.Buckets {
			w := parseQuotaBucket(b)
			if w == nil {
				continue
			}
			switch w.BucketID {
			case "gemini-5h":
				if fiveHour == nil {
					two := *w
					fiveHour = &two
				}
			case "gemini-weekly":
				if weekly == nil {
					two := *w
					weekly = &two
				}
			}
		}
	}
	return fiveHour, weekly, nil
}

// parseQuotaBucket decodes one bucket.  An unparseable reset time drops the
// whole bucket: a window with no reset time would render as never expiring.
func parseQuotaBucket(raw json.RawMessage) *quotaWindow {
	var b struct {
		BucketID          string   `json:"bucketId"`
		Window            string   `json:"window"`
		ResetTime         string   `json:"resetTime"`
		RemainingFraction *float64 `json:"remainingFraction"`
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil
	}
	if b.BucketID == "" || b.RemainingFraction == nil {
		return nil
	}
	if b.ResetTime == "" {
		return nil
	}
	if _, err := time.Parse(time.RFC3339, b.ResetTime); err != nil {
		return nil
	}
	return &quotaWindow{
		BucketID:          b.BucketID,
		Window:            b.Window,
		ResetTime:         b.ResetTime,
		RemainingFraction: *b.RemainingFraction,
	}
}

// quotaRefusal turns a non-2xx quota response into a message that names the
// vendor's reason (a missing 5-hour bucket answers 403 SUBSCRIPTION_REQUIRED
// while the credential is perfectly valid).
func quotaRefusal(status int, body []byte) error {
	reason := ""
	var env struct {
		Error struct {
			Message string `json:"message"`
			Details []struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil {
		for _, d := range env.Error.Details {
			if d.Reason != "" {
				reason = d.Reason
				break
			}
		}
	}
	if status == 401 || status == 403 {
		msg := "quota query rejected"
		if reason != "" {
			msg += " (" + reason + ")"
		}
		return &statusError{msg: msg}
	}
	return &statusError{msg: describeBody(status, body)}
}

type statusError struct{ msg string }

func (e *statusError) Error() string { return e.msg }

// --- discovery --------------------------------------------------------------

// discoverCredentialPaths lists Google OAuth token files the Cloud Code tools
// leave on this machine.
func discoverCredentialPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var candidates []string
	switch runtime.GOOS {
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			candidates = append(candidates,
				filepath.Join(appData, "Antigravity", "oauth_creds.json"),
				filepath.Join(appData, "Antigravity", "tokens.json"),
				filepath.Join(appData, "Code", "User", "globalStorage", "oauth_creds.json"),
			)
		}
	}
	candidates = append(candidates,
		filepath.Join(home, ".config", "antigravity", "oauth_creds.json"),
		filepath.Join(home, ".gemini", "oauth_creds.json"),
		filepath.Join(home, ".cloudcode", "credentials.json"),
	)
	out := make([]string, 0, len(candidates))
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			out = append(out, p)
		}
	}
	return out
}

// readFileLimited reads a credential file, capped.
func readFileLimited(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) > 1<<20 {
		return nil, &statusError{msg: "credential file is implausibly large"}
	}
	return b, nil
}
