package zcode

import (
	"context"
	"net/http"
	"testing"
)

// TestClaimUsesTheAPIKeySiblingWhenTheJWTChannelHasNoPlan covers the shape the
// operator actually has: the panel groups a plan JWT and a coding-plan API key
// under one Zhipu account, but the claim endpoint is reachable through the
// sibling API key as well.  The old code rejected any non-JWT row outright.
func TestClaimUsesTheAPIKeySiblingWhenTheJWTChannelHasNoPlan(t *testing.T) {
	const userID = "61161790588087632"
	env := newPanelEnv(t, `{"auto_discover":false}`)
	var claimed bool
	c := env.client(t, routeTransport(t, captchaRoutes(captchaConfigOff, map[string]func(*http.Request) (*http.Response, error){
		monitorQuotaPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, monitorQuotaFixture), nil
		},
		subscriptionPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, subscriptionFixture), nil
		},
		planPreviewPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, claimPreviewFixture), nil
		},
		planClaimPath: func(*http.Request) (*http.Response, error) {
			claimed = true
			return jsonResponse(http.StatusOK, `{"code":0,"msg":"","data":{"plan":{"starts_at":1,"ends_at":2}}}`), nil
		},
	})))
	jwtID := addJWTAccountWithUser(t, c, userID)
	addBigmodelAPIKey(t, c, userID)
	installSolver(t, c, "token-from-solver")

	res, err := c.Checkin(context.Background(), jwtID, claimAction)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("result = %+v, want the sibling API key to make the claim", res)
	}
	if !claimed {
		t.Fatal("the claim endpoint was never called through the sibling channel")
	}
}

// TestClaimOnAnAPIKeyAccountFindsItsSiblingJWT is the mirror case: the operator
// clicked the row the panel shows for the API-key channel, but the claim
// endpoint is the JWT channel's.  The module must resolve the sibling instead
// of answering "plan billing needs a JWT".
func TestClaimOnAnAPIKeyAccountFindsItsSiblingJWT(t *testing.T) {
	const userID = "61161790588087632"
	env := newPanelEnv(t, `{"auto_discover":false}`)
	var claimed bool
	c := env.client(t, routeTransport(t, captchaRoutes(captchaConfigOff, map[string]func(*http.Request) (*http.Response, error){
		planPreviewPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, claimPreviewFixture), nil
		},
		planClaimPath: func(*http.Request) (*http.Response, error) {
			claimed = true
			return jsonResponse(http.StatusOK, `{"code":0,"msg":"","data":{"plan":{"starts_at":1,"ends_at":2}}}`), nil
		},
	})))
	addJWTAccountWithUser(t, c, userID)
	keyID := addBigmodelAPIKey(t, c, userID)
	installSolver(t, c, "token-from-solver")

	res, err := c.Checkin(context.Background(), keyID, claimAction)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK || !claimed {
		t.Fatalf("result = %+v claimed=%v, want the sibling JWT to make the claim", res, claimed)
	}
}
