package workbuddy

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// countriesFixture is the reference's double-nested shape: the payload carries
// another {data:{list:[...]}} layer inside env.Data.
const countriesFixture = `{"data":{"list":[
  {"EnName":"Hong Kong","Name":"中国香港","IOS2":"HK","IOS3":"HKG","Code":"852"},
  {"EnName":"Singapore","Name":"新加坡","IOS2":"SG","IOS3":"SGP","Code":"65"},
  {"EnName":"United States","Name":"美国","IOS2":"US","IOS3":"USA","Code":"1"},
  {"EnName":"Thailand","Name":"泰国","IOS2":"TH","IOS3":"THA","Code":"66"}
]}}`

func TestWorkbuddyGlobalFetchCountries(t *testing.T) {
	t.Run("intl only keeps the whitelist in order", func(t *testing.T) {
		var seen *http.Request
		rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
			seen = req
			return jsonResponse(200, wbEnvelope(countriesFixture)), nil
		}}
		c, _ := panelClient(t, rt, intlAccountFiles())
		a := wbIntlAuth(t, c)

		got, err := c.GlobalFetchCountries(context.Background(), a, true)
		if err != nil {
			t.Fatalf("GlobalFetchCountries: %v", err)
		}
		// The register chain carries a Bearer token and the web UA, but no
		// X-User-Id: only the status call adds that one.
		if h, want := seen.Header.Get("Authorization"), "Bearer "+a.AccessTokenValue(); h != want {
			t.Fatalf("Authorization = %q, want %q", h, want)
		}
		if h := seen.Header.Get("X-User-Id"); h != "" {
			t.Fatalf("X-User-Id = %q, but this chain does not send it", h)
		}
		if h := seen.Header.Get("User-Agent"); h != globalWebUA {
			t.Fatalf("User-Agent = %q, want the web fingerprint", h)
		}
		req := seen
		if req.Method != http.MethodPost || req.URL.Path != globalRegisterCountriesPath {
			t.Fatalf("%s %s, want POST %s", req.Method, req.URL.Path, globalRegisterCountriesPath)
		}
		body := wbBody(t, req)
		if body["filterForbidden"] != float64(1) {
			t.Fatalf("body = %v, want filterForbidden=1", body)
		}
		if req.Header.Get("Origin") == "" || req.Header.Get("Referer") == "" {
			t.Fatalf("Origin/Referer missing: %v", req.Header)
		}
		// Italy/US are dropped; the whitelist order (HK, SG, TH) is preserved.
		var codes []string
		for _, cc := range got {
			codes = append(codes, cc.IOS2)
		}
		if strings.Join(codes, ",") != "HK,SG,TH" {
			t.Fatalf("IOS2 list = %v, want [HK SG TH]", codes)
		}
		if got[0].Name != "中国香港" || got[0].Code != "852" || got[0].EnName != "Hong Kong" {
			t.Fatalf("first country = %+v, want the full record", got[0])
		}
	})

	t.Run("without the filter every country survives", func(t *testing.T) {
		rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
			return jsonResponse(200, wbEnvelope(countriesFixture)), nil
		}}
		c, _ := panelClient(t, rt, intlAccountFiles())

		got, err := c.GlobalFetchCountries(context.Background(), wbIntlAuth(t, c), false)
		if err != nil {
			t.Fatalf("GlobalFetchCountries: %v", err)
		}
		if len(got) != 4 {
			t.Fatalf("got %d countries, want all 4: %+v", len(got), got)
		}
	})

	t.Run("a string-wrapped payload is unwrapped", func(t *testing.T) {
		wrapped := `"{\"data\":{\"list\":[{\"EnName\":\"Macao\",\"Name\":\"中国澳门\",\"IOS2\":\"MO\",\"IOS3\":\"MAC\",\"Code\":\"853\"}]}}"`
		rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
			return jsonResponse(200, wbEnvelope(wrapped)), nil
		}}
		c, _ := panelClient(t, rt, intlAccountFiles())

		got, err := c.GlobalFetchCountries(context.Background(), wbIntlAuth(t, c), true)
		if err != nil {
			t.Fatalf("GlobalFetchCountries: %v", err)
		}
		if len(got) != 1 || got[0].IOS2 != "MO" {
			t.Fatalf("got %+v, want the single MO record", got)
		}
	})

	t.Run("a business refusal is an error naming the code", func(t *testing.T) {
		rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
			return jsonResponse(200, wbRefusal(11001, "forbidden")), nil
		}}
		c, _ := panelClient(t, rt, intlAccountFiles())

		_, err := c.GlobalFetchCountries(context.Background(), wbIntlAuth(t, c), true)
		if err == nil {
			t.Fatal("a refused country list returned no error")
		}
		if !strings.Contains(err.Error(), "get-country-code") || !strings.Contains(err.Error(), "11001") {
			t.Fatalf("error = %q, want it to name the call and the code", err)
		}
	})

	t.Run("the cn realm is refused without a request", func(t *testing.T) {
		rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
			t.Fatalf("a CN account reached %s", req.URL.Path)
			return nil, nil
		}}
		c, _ := panelClient(t, rt, cnAccountFiles())

		_, err := c.GlobalFetchCountries(context.Background(), wbCNAuth(t, c), true)
		if err == nil || err.Error() != "fetch countries: only global accounts" {
			t.Fatalf("error = %v, want the global-only guard", err)
		}
		if n := len(wbPaths(rt)); n != 0 {
			t.Fatalf("%d requests were sent, want 0", n)
		}
	})
}

func TestWorkbuddyGlobalRegisterStatus(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		activated     bool
		needsRegion   bool
		wantMsgSubstr string
	}{
		{"activated", `{"code":200,"msg":"register success"}`, true, false, "register success"},
		{"region required by code", `{"code":500,"msg":"need a region"}`, false, true, "need a region"},
		{"region required by wording", `{"code":1,"msg":"Region Required"}`, false, true, "Region Required"},
		{"anything else", `{"code":1,"msg":"still pending"}`, false, false, "still pending"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var seen *http.Request
			rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
				seen = req
				return jsonResponse(200, tc.body), nil
			}}
			c, _ := panelClient(t, rt, intlAccountFiles())
			a := wbIntlAuth(t, c)

			activated, needsRegion, msg, err := c.GlobalRegisterStatus(context.Background(), a)
			if err != nil {
				t.Fatalf("GlobalRegisterStatus: %v", err)
			}
			if activated != tc.activated || needsRegion != tc.needsRegion {
				t.Fatalf("activated=%v needsRegion=%v, want %v/%v",
					activated, needsRegion, tc.activated, tc.needsRegion)
			}
			if !strings.Contains(msg, tc.wantMsgSubstr) {
				t.Fatalf("msg = %q, want it to contain %q", msg, tc.wantMsgSubstr)
			}
			req := wbIdentity(t, seen, a)
			if req.Method != http.MethodGet || req.URL.Path != globalRegisterStatusPath {
				t.Fatalf("%s %s, want GET %s", req.Method, req.URL.Path, globalRegisterStatusPath)
			}
			if !strings.Contains(req.URL.RawQuery, "userId="+a.UIDValue()) {
				t.Fatalf("query = %q, want userId=%s", req.URL.RawQuery, a.UIDValue())
			}
		})
	}

	t.Run("the cn realm is refused without a request", func(t *testing.T) {
		rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
			t.Fatalf("a CN account reached %s", req.URL.Path)
			return nil, nil
		}}
		c, _ := panelClient(t, rt, cnAccountFiles())

		_, _, _, err := c.GlobalRegisterStatus(context.Background(), wbCNAuth(t, c))
		if err == nil || err.Error() != "register status: only global accounts" {
			t.Fatalf("error = %v, want the global-only guard", err)
		}
		if n := len(wbPaths(rt)); n != 0 {
			t.Fatalf("%d requests were sent, want 0", n)
		}
	})
}

func TestWorkbuddyGlobalSubmitRegionUsesOneElementArrays(t *testing.T) {
	var seen *http.Request
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		seen = req
		return jsonResponse(200, wbEnvelope(`{}`)), nil
	}}
	c, _ := panelClient(t, rt, intlAccountFiles())
	a := wbIntlAuth(t, c)

	country := GlobalCountry{EnName: "Hong Kong", Name: "中国香港", IOS2: "HK", IOS3: "HKG", Code: "852"}
	if err := c.GlobalSubmitRegion(context.Background(), a, country); err != nil {
		t.Fatalf("GlobalSubmitRegion: %v", err)
	}
	if h, want := seen.Header.Get("Authorization"), "Bearer "+a.AccessTokenValue(); h != want {
		t.Fatalf("Authorization = %q, want %q", h, want)
	}
	if h := seen.Header.Get("X-User-Id"); h != "" {
		t.Fatalf("X-User-Id = %q, but this chain does not send it", h)
	}
	req := seen
	if req.Method != http.MethodPost || req.URL.Path != globalSubmitRegionPath {
		t.Fatalf("%s %s, want POST %s", req.Method, req.URL.Path, globalSubmitRegionPath)
	}
	body := wbBody(t, req)
	attrs, ok := body["attributes"].(map[string]any)
	if !ok {
		t.Fatalf("body = %v, want an attributes object", body)
	}
	// The vendor wants every attribute as a one-element array.
	for key, want := range map[string]string{
		"countryCode":     "852",
		"countryFullName": "Hong Kong",
		"countryName":     "HK",
	} {
		arr, ok := attrs[key].([]any)
		if !ok || len(arr) != 1 || arr[0] != want {
			t.Fatalf("attributes[%s] = %v, want [%s]", key, attrs[key], want)
		}
	}
}

func TestWorkbuddyGlobalSubmitRegionSurfacesARefusal(t *testing.T) {
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		return jsonResponse(200, wbRefusal(11007, "the region is not supported")), nil
	}}
	c, _ := panelClient(t, rt, intlAccountFiles())

	err := c.GlobalSubmitRegion(context.Background(), wbIntlAuth(t, c), GlobalCountry{IOS2: "HK", Code: "852"})
	if err == nil {
		t.Fatal("a refused region submit returned no error")
	}
	if !strings.Contains(err.Error(), "11007") {
		t.Fatalf("error = %q, want it to name the business code", err)
	}
}

func TestWorkbuddyGlobalCompleteRegistration(t *testing.T) {
	t.Run("repairs an account that needs a region", func(t *testing.T) {
		var status, submit, countries int
		rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
			switch {
			case req.URL.Path == globalRegisterStatusPath:
				status++
				if status == 1 {
					return jsonResponse(200, `{"code":500,"msg":"region required"}`), nil
				}
				return jsonResponse(200, `{"code":200,"msg":"register success"}`), nil
			case req.URL.Path == globalRegisterCountriesPath:
				countries++
				return jsonResponse(200, wbEnvelope(countriesFixture)), nil
			case req.URL.Path == globalSubmitRegionPath:
				submit++
				return jsonResponse(200, wbEnvelope(`{}`)), nil
			default:
				t.Fatalf("unexpected request %s %s", req.Method, req.URL.Path)
				return nil, nil
			}
		}}
		c, _ := panelClient(t, rt, intlAccountFiles())

		activated, err := c.GlobalCompleteRegistration(context.Background(), wbIntlAuth(t, c))
		if err != nil {
			t.Fatalf("GlobalCompleteRegistration: %v", err)
		}
		if !activated {
			t.Fatal("activated = false after a successful repair")
		}
		if status != 2 || countries != 1 || submit != 1 {
			t.Fatalf("status=%d countries=%d submit=%d, want 2/1/1", status, countries, submit)
		}
	})

	t.Run("an already activated account makes no repair call", func(t *testing.T) {
		rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
			if req.URL.Path != globalRegisterStatusPath {
				t.Fatalf("unexpected request %s %s", req.Method, req.URL.Path)
			}
			return jsonResponse(200, `{"code":200,"msg":"register success"}`), nil
		}}
		c, _ := panelClient(t, rt, intlAccountFiles())

		activated, err := c.GlobalCompleteRegistration(context.Background(), wbIntlAuth(t, c))
		if err != nil {
			t.Fatalf("GlobalCompleteRegistration: %v", err)
		}
		if !activated {
			t.Fatal("activated = false for an activated account")
		}
		if n := wbCalls(rt, globalRegisterStatusPath); n != 1 {
			t.Fatalf("%d status calls, want exactly 1", n)
		}
	})

	t.Run("a pending account that is not about a region is an error", func(t *testing.T) {
		rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
			return jsonResponse(200, `{"code":1,"msg":"awaiting review"}`), nil
		}}
		c, _ := panelClient(t, rt, intlAccountFiles())

		activated, err := c.GlobalCompleteRegistration(context.Background(), wbIntlAuth(t, c))
		if err == nil {
			t.Fatalf("returned activated=%v and no error for a pending account", activated)
		}
		if !strings.Contains(err.Error(), "awaiting review") {
			t.Fatalf("error = %q, want it to carry the upstream message", err)
		}
	})
}
