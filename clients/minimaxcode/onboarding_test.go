package minimaxcode

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"client2api/internal/core"
)

func TestLoginCompletesAccountOnboarding(t *testing.T) {
	c, ft := newTestClient(t, loginConfig(), func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/oauth2/device/code":
			return jsonResponse(http.StatusOK,
				`{"user_code":"ABCD-EFGH","device_code":"dev-onboarding",`+
					`"verification_uri":"https://account.minimax.cn/device",`+
					`"expires_in":600,"interval":1}`), nil
		case "/oauth2/token":
			return jsonResponse(http.StatusOK,
				`{"access_token":"mmoat_onboarding","refresh_token":"mmrt_onboarding",`+
					`"token_type":"Bearer","expires_in":3600}`), nil
		case "/v1/api/user/info":
			return jsonResponse(http.StatusOK,
				`{"data":{"userInfo":{"userID":"user-onboarding","realUserID":"9009",`+
					`"subUserName":"MiniMaxOnboarding"}},"statusInfo":{"code":0}}`), nil
		case onboardingPathTest:
			return jsonResponse(http.StatusOK,
				`{"onboarding":{"completed":true,"category_ids":["vibe_coding"]},`+
					`"base_resp":{"status_code":0,"status_msg":"ok"}}`), nil
		default:
			return nil, fmt.Errorf("unexpected request to %s", req.URL.Path)
		}
	})

	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	forcePoll(t, c, st.SessionID)
	done, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if done.State != core.LoginSuccess {
		t.Fatalf("poll state = %q (%s), want success", done.State, done.Message)
	}

	userInfoAt, onboardingAt := -1, -1
	for i := 0; i < ft.count(); i++ {
		switch ft.requestAt(i).URL.Path {
		case "/v1/api/user/info":
			userInfoAt = i
		case onboardingPathTest:
			onboardingAt = i
		}
	}
	if onboardingAt < 0 {
		t.Fatal("the sign-in did not complete the account onboarding")
	}
	if userInfoAt < 0 || onboardingAt < userInfoAt {
		t.Fatalf("onboarding request %d ran before user-info request %d", onboardingAt, userInfoAt)
	}

	req := ft.requestAt(onboardingAt)
	if req.Method != http.MethodPost {
		t.Errorf("onboarding method = %s, want POST", req.Method)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer mmoat_onboarding" {
		t.Errorf("onboarding Authorization = %q, want the signed-in bearer token", got)
	}
	if got := ft.bodyAt(onboardingAt); got != onboardingBodyTest {
		t.Errorf("onboarding body = %q, want %q", got, onboardingBodyTest)
	}
}

func TestCheckinCompletesAccountOnboardingOnce(t *testing.T) {
	onboardingCalls := 0
	statusCalls := 0
	s := &scriptedSignin{
		statusBody: signinPanelJSON(t, signinDayClaimed, 800, true),
	}
	base := s.handler()
	c, ft := newTestClient(t, signinConfig(), func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case onboardingPathTest:
			onboardingCalls++
			return jsonResponse(http.StatusOK,
				`{"onboarding":{"completed":true,"category_ids":["vibe_coding"]},`+
					`"base_resp":{"status_code":0,"status_msg":"ok"}}`), nil
		case signinStatusPathTest:
			statusCalls++
		}
		return base(req)
	})

	if res := checkin(t, c, signinActionDaily); !res.OK {
		t.Fatalf("first Checkin was not successful: %+v", res)
	}
	if res := checkin(t, c, signinActionDaily); !res.OK {
		t.Fatalf("second Checkin was not successful: %+v", res)
	}

	if statusCalls != 2 {
		t.Errorf("status calls = %d, want 2", statusCalls)
	}
	if onboardingCalls != 1 {
		t.Errorf("onboarding calls = %d, want 1", onboardingCalls)
	}
	if ft.count() < 3 {
		t.Fatalf("made %d requests, want onboarding then two status reads", ft.count())
	}
	if got := ft.requestAt(0).URL.Path; got != onboardingPathTest {
		t.Errorf("first request went to %s, want onboarding before sign-in", got)
	}
	if got := ft.bodyAt(0); got != onboardingBodyTest {
		t.Errorf("onboarding body = %q, want %q", got, onboardingBodyTest)
	}
	if got := ft.requestAt(1).URL.Path; got != signinStatusPathTest {
		t.Errorf("second request went to %s, want the sign-in status", got)
	}
}
