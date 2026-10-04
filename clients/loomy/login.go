package loomy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"client2api/internal/core"
)

// login.go is the one real consumer of the HMAC-SHA1 signature: the iFlytek
// CAccount login API, which is the only endpoint that authenticates with the
// product's AccessKey rather than with a user session.
//
// It implements the reference's two-step SMS flow:
//
//	POST {accountBase}/login/phone/sendMsgCode   -> data.msgid
//	POST {accountBase}/login/phone/checkCode     -> data.session, data.userid
//
// The `expire` parameter on the second call is where the session's life comes
// from: the vendor is asked for 14 days and never reports an expiry back, so the
// expiry this module stores is computed locally at login time -- exactly as the
// reference does.
//
// This flow IS reachable from the panel, but through core.AutoLoginProvider
// rather than core.LoginProvider.  AutoLoginProvider is a job the module runs
// itself, so the module can submit the code it fetched from the one-time-SMS
// platform; see autologin.go for the end-to-end run and sms.go for the
// platform side.  LoginProvider stays unimplemented on purpose: it is a
// URL-and-poll interface with no way to carry a code, and claiming it would
// put a "get an authorisation link" button in front of a flow that has no
// link.
//
// The WeChat flow is deliberately NOT implemented.  Its four bind endpoints are
// documented, but the QR code that starts it is obtained by a local browser
// flow in a reference file that could not be fetched, and a bind sequence with
// no way to obtain a `code` cannot be exercised at all.

// accountBase is the fixed preamble every CAccount request carries.  The `ua`
// value is hard-coded to macOS in the reference even when the client runs on
// Windows; it is copied verbatim rather than "corrected", because the vendor's
// own desktop client sends exactly this and the value is not worth differing on.
type accountBase struct {
	AppID   string `json:"appid"`
	ModelID string `json:"modelid"`
	Version string `json:"version"`
	DevID   string `json:"devid"`
	UA      string `json:"ua"`
	TraceID string `json:"traceid"`
}

type accountRequest struct {
	Base  accountBase `json:"base"`
	Param any         `json:"param"`
}

func newAccountBase(appID string) accountBase {
	return accountBase{
		AppID:   appID,
		ModelID: "Web",
		Version: "1.0.0",
		DevID:   "web",
		UA:      "Loomy|Desktop|Electron|macOS",
		TraceID: strings.ReplaceAll(newNonce(), "-", ""),
	}
}

// postAccount signs and sends one CAccount request.
//
// The body is serialised exactly once and that same byte string is what gets
// signed and what gets sent.  Marshalling twice would be a real bug: a second
// pass can reorder keys or change spacing, and any byte difference invalidates
// the HMAC.
func (u *upstream) postAccount(ctx context.Context, path string, param any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, u.cfg.requestTimeout())
	defer cancel()

	payload, err := json.Marshal(accountRequest{Base: newAccountBase(u.cfg.appID()), Param: param})
	if err != nil {
		return nil, fmt.Errorf("loomy: encoding %s: %w", path, err)
	}

	headers := authHeaders(signOptions{
		AccessKeyID:     u.cfg.accessKeyID(),
		AccessKeySecret: u.cfg.accessKeySecret(),
		Method:          http.MethodPost,
		Path:            path,
		Body:            string(payload),
		ContentType:     "application/json",
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.cfg.accountBase()+path, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("loomy: %s: %w", path, err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := u.json.Do(req)
	if err != nil {
		return nil, fmt.Errorf("loomy: %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("loomy: reading %s: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &apiError{Status: resp.StatusCode, Message: truncate(strings.TrimSpace(string(body)), 300)}
	}

	env := parseEnvelope(body)
	if !env.OK {
		return nil, &apiError{Code: env.Code, Status: resp.StatusCode, Message: env.Message}
	}
	return env.Data, nil
}

// sendSMSCode asks the vendor to text a login code to phone and returns the
// message id that the code is redeemed against.
func (u *upstream) sendSMSCode(ctx context.Context, phone string) (string, error) {
	data, err := u.postAccount(ctx, "/login/phone/sendMsgCode", map[string]any{
		"ccode":  "86",
		"phone":  phone,
		"expire": smsCodeTTLSeconds,
	})
	if err != nil {
		return "", err
	}

	var out struct {
		MsgID string `json:"msgid"`
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &out); err != nil {
			return "", fmt.Errorf("loomy: reading the SMS response: %w", err)
		}
	}
	// An empty message id is an error, not a value to carry forward: redeeming a
	// code against nothing would fail later with a much less useful message.
	if out.MsgID == "" {
		return "", fmt.Errorf("loomy: the SMS endpoint returned no message id")
	}
	return out.MsgID, nil
}

// loomySession is a freshly issued session.
type loomySession struct {
	AccessToken string
	UserID      string
	Phone       string
}

// loginWithSMSCode redeems a code for a session.
//
// `expire` is the requested session lifetime in seconds and is where Loomy's
// 14-day session comes from -- the vendor never returns the expiry it granted.
func (u *upstream) loginWithSMSCode(ctx context.Context, phone, code, msgid string) (loomySession, error) {
	data, err := u.postAccount(ctx, "/login/phone/checkCode", map[string]any{
		"ccode":  "86",
		"phone":  phone,
		"mcode":  code,
		"msgid":  msgid,
		"expire": sessionTTLSeconds,
	})
	if err != nil {
		return loomySession{}, err
	}

	var out struct {
		Session string `json:"session"`
		UserID  string `json:"userid"`
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &out); err != nil {
			return loomySession{}, fmt.Errorf("loomy: reading the login response: %w", err)
		}
	}
	if out.Session == "" || out.UserID == "" {
		return loomySession{}, fmt.Errorf("loomy: the login response carried no session or user id")
	}
	return loomySession{AccessToken: out.Session, UserID: out.UserID, Phone: phone}, nil
}

// importSession stores a session obtained by logging in.
//
// The expiry is computed here rather than read from the response, because the
// vendor does not send one: the lifetime was requested at login, so login time
// plus that lifetime is the only figure available.
func (c *Client) importSession(session loomySession, now time.Time) (core.AccountRecord, error) {
	acc := account{
		storedAccount: storedAccount{
			AccessToken: session.AccessToken,
			UserID:      session.UserID,
			Phone:       session.Phone,
			ExpiresAtMS: now.Add(time.Duration(sessionTTLSeconds) * time.Second).UnixMilli(),
			Enabled:     true,
		},
		origin: originStored,
	}
	acc.ID = defaultAccountID(acc)
	// The label is what the panel shows and what its "re-login this account"
	// button keys on, so a phone login labels the row with the number.  A
	// re-login can then re-issue the account's own number instead of renting a
	// new one, which would create a different Loomy account.
	if acc.Phone != "" {
		acc.Label = acc.Phone
	} else {
		acc.Label = acc.ID
	}
	if err := c.store.put(acc); err != nil {
		return core.AccountRecord{}, err
	}
	stored, _ := c.store.lookup(acc.ID)
	return accountRecord(&stored, now), nil
}
