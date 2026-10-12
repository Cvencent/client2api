package doubaoime

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"

	"client2api/internal/core"
)

const maxAuthResponseBytes = 1 << 20

type cachedCredentials struct {
	CDID       string    `json:"cdid"`
	OpenUDID   string    `json:"openudid"`
	ClientUDID string    `json:"clientudid"`
	DeviceID   string    `json:"device_id"`
	AppKey     string    `json:"app_key"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (c cachedCredentials) valid() bool {
	return c.CDID != "" && c.OpenUDID != "" && c.ClientUDID != "" && c.DeviceID != "" && c.DeviceID != "0" && c.AppKey != ""
}

type deviceIdentity struct {
	CDID       string
	OpenUDID   string
	ClientUDID string
}

func newIdentity() (deviceIdentity, error) {
	cdid, err := randomUUID()
	if err != nil {
		return deviceIdentity{}, err
	}
	clientudid, err := randomUUID()
	if err != nil {
		return deviceIdentity{}, err
	}
	var open [8]byte
	if _, err := rand.Read(open[:]); err != nil {
		return deviceIdentity{}, err
	}
	return deviceIdentity{CDID: cdid, OpenUDID: hex.EncodeToString(open[:]), ClientUDID: clientudid}, nil
}

func randomUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func (c *Client) ensureCredentials(ctx context.Context, force bool) error {
	c.credMu.Lock()
	defer c.credMu.Unlock()
	if !force && c.credentials.valid() {
		return nil
	}
	identity := deviceIdentity{CDID: c.credentials.CDID, OpenUDID: c.credentials.OpenUDID, ClientUDID: c.credentials.ClientUDID}
	if identity.CDID == "" || identity.OpenUDID == "" || identity.ClientUDID == "" {
		var err error
		identity, err = newIdentity()
		if err != nil {
			return fmt.Errorf("doubaoime: generating device identity: %w", err)
		}
	}
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		session, err := c.newAuthSession()
		if err != nil {
			return err
		}
		creds, err := session.acquire(ctx, identity)
		if err == nil {
			creds.UpdatedAt = time.Now().UTC()
			c.credentials = creds
			if c.credentialsPath != "" {
				if err := core.WriteJSONAtomic(c.credentialsPath, creds); err != nil {
					return fmt.Errorf("doubaoime: saving credentials: %w", err)
				}
			}
			return nil
		}
		last = err
		if !isAuthFailure(err) {
			return err
		}
	}
	return last
}

type authSession struct {
	client    *http.Client
	cfg       config
	userAgent string
}

func (c *Client) newAuthSession() (*authSession, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("doubaoime: creating cookie jar: %w", err)
	}
	base := c.deps.HTTPClient
	if base == nil {
		base = &http.Client{}
	}
	httpClient := *base
	httpClient.Jar = jar
	return &authSession{client: &httpClient, cfg: c.cfg, userAgent: c.cfg.userAgent()}, nil
}

func (s *authSession) acquire(ctx context.Context, id deviceIdentity) (cachedCredentials, error) {
	deviceID, err := s.register(ctx, id)
	if err != nil {
		return cachedCredentials{}, err
	}
	appKey, err := s.token(ctx, id, deviceID)
	if err != nil {
		return cachedCredentials{}, err
	}
	return cachedCredentials{CDID: id.CDID, OpenUDID: id.OpenUDID, ClientUDID: id.ClientUDID, DeviceID: deviceID, AppKey: appKey}, nil
}

func (s *authSession) register(ctx context.Context, id deviceIdentity) (string, error) {
	nowMS := strconv.FormatInt(time.Now().UnixMilli(), 10)
	query := url.Values{
		"device_platform": {"android"}, "os": {"android"}, "ssmix": {"a"}, "_rticket": {nowMS},
		"cdid": {id.CDID}, "channel": {"official"}, "aid": {"401734"}, "app_name": {"oime"},
		"version_code": {"100102018"}, "version_name": {"1.1.2"}, "manifest_version_code": {"100102018"},
		"update_version_code": {"100102018"}, "resolution": {"1080*2400"}, "dpi": {"420"},
		"device_type": {"Pixel 7 Pro"}, "device_brand": {"google"}, "language": {"zh"},
		"os_api": {"34"}, "os_version": {"16"}, "ac": {"wifi"},
	}
	body := map[string]any{
		"magic_tag": "ss_app_log",
		"header": map[string]any{
			"device_id": 0, "install_id": 0, "aid": 401734, "app_name": "oime",
			"version_code": 100102018, "version_name": "1.1.2", "manifest_version_code": 100102018,
			"update_version_code": 100102018, "channel": "official", "package": "com.bytedance.android.doubaoime",
			"device_platform": "android", "os": "android", "os_api": "34", "os_version": "16",
			"device_type": "Pixel 7 Pro", "device_brand": "google", "device_model": "Pixel 7 Pro",
			"resolution": "1080*2400", "dpi": "420", "language": "zh", "timezone": 8, "access": "wifi",
			"rom": "UP1A.231005.007", "rom_version": "UP1A.231005.007", "openudid": id.OpenUDID,
			"clientudid": id.ClientUDID, "cdid": id.CDID, "region": "CN", "tz_name": "Asia/Shanghai",
			"tz_offset": 28800, "sim_region": "cn", "carrier_region": "cn", "cpu_abi": "arm64-v8a",
			"build_serial": "unknown", "not_request_sender": 0, "sig_hash": "", "google_aid": "",
			"mc": "", "serial_number": "",
		},
		"_gen_time": time.Now().UnixMilli(),
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(s.cfg.registerURL())
	if err != nil {
		return "", fmt.Errorf("doubaoime: invalid register URL: %w", err)
	}
	u.RawQuery = query.Encode()
	respBody, _, err := s.post(ctx, u.String(), "application/json", raw)
	if err != nil {
		return "", err
	}
	deviceID := findFirstString(respBody, "device_id")
	if deviceID == "" || deviceID == "0" {
		return "", core.Fail("doubaoime", "", core.FailureUpstream, http.StatusBadGateway,
			errors.New("doubaoime: device registration returned no device_id"))
	}
	return deviceID, nil
}

func (s *authSession) token(ctx context.Context, id deviceIdentity, deviceID string) (string, error) {
	query := url.Values{
		"device_platform": {"android"}, "os": {"android"}, "ssmix": {"a"}, "channel": {"official"},
		"aid": {"401734"}, "app_name": {"oime"}, "version_code": {"100102018"}, "version_name": {"1.1.2"},
		"device_id": {deviceID}, "cdid": {id.CDID},
	}
	u, err := url.Parse(s.cfg.settingsURL())
	if err != nil {
		return "", fmt.Errorf("doubaoime: invalid settings URL: %w", err)
	}
	u.RawQuery = query.Encode()
	body := []byte("body=null")
	sum := md5.Sum(body)
	headers := map[string]string{"x-ss-stub": strings.ToUpper(hex.EncodeToString(sum[:]))}
	respBody, _, err := s.postWithHeaders(ctx, u.String(), "application/x-www-form-urlencoded", body, headers)
	if err != nil {
		return "", err
	}
	appKey := findFirstString(respBody, "app_key")
	if appKey == "" {
		return "", core.Fail("doubaoime", "", core.FailureAuth, http.StatusBadGateway,
			errors.New("doubaoime: settings response returned no app_key"))
	}
	return appKey, nil
}

func (s *authSession) post(ctx context.Context, rawURL, contentType string, body []byte) ([]byte, int, error) {
	return s.postWithHeaders(ctx, rawURL, contentType, body, nil)
}

func (s *authSession) postWithHeaders(ctx context.Context, rawURL, contentType string, body []byte, headers map[string]string) ([]byte, int, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(time.Duration(200+100*attempt) * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, 0, ctx.Err()
			case <-timer.C:
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("User-Agent", s.userAgent)
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		resp, err := s.client.Do(req)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, 0, ctx.Err()
			}
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxAuthResponseBytes))
		resp.Body.Close()
		if readErr != nil {
			return nil, resp.StatusCode, readErr
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return data, resp.StatusCode, nil
		}
		err = classifyAuthHTTP(resp.StatusCode, data)
		if !retryableAuthStatus(resp.StatusCode) {
			return nil, resp.StatusCode, err
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("doubaoime: upstream request failed")
	}
	return nil, 0, lastErr
}

func classifyAuthHTTP(status int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 400 {
		msg = msg[:400]
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return core.Fail("doubaoime", "", core.FailureAuth, status, fmt.Errorf("doubaoime: upstream authentication failed: HTTP %d", status))
	case http.StatusTooManyRequests:
		return core.Fail("doubaoime", "", core.FailureRateLimited, status, fmt.Errorf("doubaoime: upstream rate limited: HTTP %d", status))
	default:
		return core.Fail("doubaoime", "", core.FailureUpstream, status, fmt.Errorf("doubaoime: upstream returned HTTP %d: %s", status, core.Redact(msg)))
	}
}

func retryableAuthStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooEarly || status == http.StatusTooManyRequests || status >= 500
}

func isAuthFailure(err error) bool {
	f, ok := core.AsFailure(err)
	return ok && f.Kind == core.FailureAuth
}

func findFirstString(raw []byte, key string) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return ""
	}
	return findKeyString(value, key)
}

func findKeyString(value any, key string) string {
	switch v := value.(type) {
	case map[string]any:
		if direct, ok := v[key]; ok {
			if s := scalarString(direct); s != "" && s != "0" {
				return s
			}
		}
		for _, child := range v {
			if s := findKeyString(child, key); s != "" {
				return s
			}
		}
	case []any:
		for _, child := range v {
			if s := findKeyString(child, key); s != "" {
				return s
			}
		}
	}
	return ""
}

func scalarString(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return v.String()
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	default:
		return ""
	}
}
