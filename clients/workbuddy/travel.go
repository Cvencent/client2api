package workbuddy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// Growth-domain "buddy travels" endpoints: status / depart / claim / info, plus
// the consecutive-login read.
//
// Ported from the reference internal/upstream/travel.go (MIT).  Everything here
// goes to the chat host (copilot.tencent.com, no /v2 prefix) with the billing
// identity, and the envelope is judged like the reference's doJSON.
//
// buddyFirstPath and buddyAgreementPath are deliberately absent: this module
// already implements /activity/growth/buddy/first and /buddy/agreement in
// tasks.go (runFirstBuddy, gated by buddyGateClosed), and re-porting them would
// give one vendor route two owners.

const (
	travelStatusPath = "/activity/growth/buddy/travel/status"
	travelDepartPath = "/activity/growth/buddy/travel/depart"
	travelClaimPath  = "/activity/growth/buddy/travel/claim"
	buddyInfoPath    = "/activity/growth/buddy/info"

	// streakPath is read by both GrowthStreak and GrowthStreakFull.  It lives
	// here rather than in streak.go to mirror the reference file layout.
	streakPath = "/activity/growth/streak"
)

// Buddy is the account's current cat profile.  A nil Buddy means "no cat yet"
// (the vendor answers `buddy: null`).
type Buddy struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// TravelState is the cat-travel status block.  RecordID is what TravelClaim
// needs; DailyLimitReached resets at midnight CST.
type TravelState struct {
	State             string `json:"state"`               // idle / traveling / arrived
	DailyLimitReached bool   `json:"daily_limit_reached"` // already sent one out today
	RecordID          int64  `json:"record_id"`
	RewardCredit      int64  `json:"reward_credit"`
}

// TravelStatus reads the cat-travel status.
func (c *Client) TravelStatus(ctx context.Context, a *Auth) (*TravelState, error) {
	data, err := c.growthCall(ctx, a, http.MethodGet, travelStatusPath, nil, false)
	if err != nil {
		return nil, err
	}
	var st TravelState
	if err := decodePayload(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// TravelDepart sends the cat out.  locationID was measured to be 1..4 (the
// reward and duration bands are identical across the four).
func (c *Client) TravelDepart(ctx context.Context, a *Auth, locationID int) error {
	_, err := c.growthCall(ctx, a, http.MethodPost, travelDepartPath, map[string]any{
		"location_id": locationID,
	}, false)
	return err
}

// TravelClaim collects an arrived trip's reward and returns reward_credit.  A
// payload without the reward field is not treated as a failure: the caller logs
// a zero, exactly like the reference.
func (c *Client) TravelClaim(ctx context.Context, a *Auth, recordID int64) (int64, error) {
	data, err := c.growthCall(ctx, a, http.MethodPost, travelClaimPath, map[string]any{
		"record_id": recordID,
	}, false)
	if err != nil {
		return 0, err
	}
	var resp struct {
		RewardCredit int64 `json:"reward_credit"`
	}
	if err := decodePayload(data, &resp); err != nil {
		return 0, err
	}
	return resp.RewardCredit, nil
}

// BuddyInfo reads the cat profile.  (nil, nil) means "no cat yet"; a null, an
// absent field and an empty object all mean the same thing.
func (c *Client) BuddyInfo(ctx context.Context, a *Auth) (*Buddy, error) {
	data, err := c.growthCall(ctx, a, http.MethodGet, buddyInfoPath, nil, false)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Buddy json.RawMessage `json:"buddy"`
	}
	if err := decodePayload(data, &resp); err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(resp.Buddy))
	if trimmed == "" || trimmed == "null" || trimmed == "{}" {
		return nil, nil
	}
	var b Buddy
	if err := decodePayload(resp.Buddy, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// GrowthStreak reads the consecutive-login day count.  This is a read-only
// oracle: a missing streak/days field yields 0 rather than an error, because
// days==0 is the "reported 200 but the event was silently dropped" signal the
// activity check looks for.
func (c *Client) GrowthStreak(ctx context.Context, a *Auth) (int, error) {
	data, err := c.growthCall(ctx, a, http.MethodGet, streakPath, nil, false)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Streak struct {
			Days int `json:"days"`
		} `json:"streak"`
	}
	if err := decodePayload(data, &resp); err != nil {
		return 0, err
	}
	return resp.Streak.Days, nil
}

// buddyTravelLocked reports whether an error is the vendor's "the buddy chore
// has not been credited yet" answer.  It is the same shape as the adoption gate
// (buddyGateClosed), which this module already owns, so it simply delegates.
func buddyTravelLocked(err error) bool {
	if err == nil {
		return false
	}
	if buddyGateClosed(err) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), buddyGateMarker)
}
