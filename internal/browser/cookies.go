package browser

import (
	"context"
	"encoding/json"
	"fmt"
)

// Cookie is one browser cookie as the DevTools Protocol reports it.  Only the
// fields a login hand-off needs are kept: the name/value pair is the credential,
// the domain and path say where it applies, and the flags let a caller keep an
// HttpOnly session cookie apart from a junk tracker.
type Cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"`
	HTTPOnly bool    `json:"httpOnly"`
	Secure   bool    `json:"secure"`
}

// Cookies reads the browser's cookie jar over CDP.
//
// It exists because a vendor login (Doubao today) mints an HttpOnly session
// cookie that no page script can see and the vendor never hands out over an API,
// so a browser driven by this package is the only place the credential can be
// read back from.
//
// urls is the set of origins to read: Network.getCookies returns the jar that
// would be sent to those origins, which is what keeps an unrelated domain's
// cookies out of the result.  An empty urls asks Storage.getCookies for the
// whole jar instead, which is the escape hatch for a caller that does not yet
// know the origin.
//
// The values this returns are credentials.  Callers must not log them or put
// them in a Status response.
func (p *Page) Cookies(ctx context.Context, urls []string) ([]Cookie, error) {
	method := "Network.getCookies"
	var params any
	if len(urls) > 0 {
		params = map[string]any{"urls": urls}
	} else {
		// Network.getCookies requires urls; Storage.getCookies returns
		// everything in the profile.
		method = "Storage.getCookies"
	}
	res, err := p.call(ctx, method, params)
	if err != nil {
		return nil, fmt.Errorf("read the browser cookies: %w", err)
	}
	var out struct {
		Cookies []Cookie `json:"cookies"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, fmt.Errorf("decode the browser cookies: %w", err)
	}
	return out.Cookies, nil
}
