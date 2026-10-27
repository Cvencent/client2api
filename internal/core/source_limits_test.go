package core

import (
	"strings"
	"testing"
	"time"
)

func TestParseSourcesAcceptsRelayLimits(t *testing.T) {
	sources, err := ParseSources([]byte(`{
		"relay": {
			"base_url": "https://relay.example/v1",
			"requests_per_minute": 120,
			"requests_per_minute_per_account": 30,
			"max_in_flight": 8,
			"max_in_flight_per_account": 1,
			"cooldown": "15s",
			"rate_limit_cooldown": "30s",
			"quota_cooldown": "5m",
			"auth_cooldown": "6m",
			"upstream_cooldown": "10s"
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg := sources["relay"]
	if cfg.RequestsPerMinute != 120 || cfg.RequestsPerMinutePerAccount != 30 {
		t.Fatalf("RPM limits = (%d, %d), want (120, 30)", cfg.RequestsPerMinute, cfg.RequestsPerMinutePerAccount)
	}
	if cfg.MaxInFlight == nil || *cfg.MaxInFlight != 8 {
		t.Fatalf("max_in_flight = %v, want 8", cfg.MaxInFlight)
	}
	if cfg.MaxInFlightPerAccount == nil || *cfg.MaxInFlightPerAccount != 1 {
		t.Fatalf("max_in_flight_per_account = %v, want 1", cfg.MaxInFlightPerAccount)
	}
	for _, tc := range []struct {
		kind FailureKind
		want time.Duration
	}{
		{FailureRateLimited, 30 * time.Second},
		{FailureQuota, 5 * time.Minute},
		{FailureAuth, 6 * time.Minute},
		{FailureUpstream, 10 * time.Second},
	} {
		if got := cfg.CooldownFor(tc.kind, time.Hour); got != tc.want {
			t.Errorf("%s cooldown = %s, want %s", tc.kind, got, tc.want)
		}
	}
}

func TestParseSourcesRejectsInvalidRelayLimits(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"negative RPM", `{"relay":{"base_url":"https://relay.example/v1","requests_per_minute":-1}}`, "requests_per_minute"},
		{"negative account RPM", `{"relay":{"base_url":"https://relay.example/v1","requests_per_minute_per_account":-1}}`, "requests_per_minute_per_account"},
		{"negative in-flight", `{"relay":{"base_url":"https://relay.example/v1","max_in_flight":-1}}`, "max_in_flight"},
		{"negative account in-flight", `{"relay":{"base_url":"https://relay.example/v1","max_in_flight_per_account":-1}}`, "max_in_flight_per_account"},
		{"bad cooldown", `{"relay":{"base_url":"https://relay.example/v1","cooldown":"later"}}`, "cooldown"},
		{"bad rate cooldown", `{"relay":{"base_url":"https://relay.example/v1","rate_limit_cooldown":"later"}}`, "rate_limit_cooldown"},
		{"bad quota cooldown", `{"relay":{"base_url":"https://relay.example/v1","quota_cooldown":"later"}}`, "quota_cooldown"},
		{"bad auth cooldown", `{"relay":{"base_url":"https://relay.example/v1","auth_cooldown":"later"}}`, "auth_cooldown"},
		{"bad upstream cooldown", `{"relay":{"base_url":"https://relay.example/v1","upstream_cooldown":"later"}}`, "upstream_cooldown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseSources([]byte(tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

func TestSourceCooldownFallsBackToGeneralAndExplicitZeroDisables(t *testing.T) {
	cfg := SourceConfig{Cooldown: "7s"}
	if got := cfg.CooldownFor(FailureRateLimited, time.Minute); got != 7*time.Second {
		t.Fatalf("general fallback = %s, want 7s", got)
	}
	cfg.RateLimitCooldown = "9s"
	if got := cfg.CooldownFor(FailureRateLimited, time.Minute); got != 9*time.Second {
		t.Fatalf("specific override = %s, want 9s", got)
	}
	cfg.RateLimitCooldown = "0s"
	if got := cfg.CooldownFor(FailureRateLimited, time.Minute); got != 0 {
		t.Fatalf("explicit zero = %s, want 0", got)
	}
}

func TestParseSourcesAcceptsRequestLimits(t *testing.T) {
	sources, err := ParseSources([]byte(`{
		"relay": {
			"base_url": "https://relay.example/v1",
			"request_limits": [
				{"scope": "account", "limit": 100, "window": "24h", "reset": "rolling"},
				{"scope": "source", "limit": 500, "window": "24h", "reset": "calendar", "timezone": "Asia/Shanghai", "reset_at": "08:00"}
			]
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	limits := sources["relay"].RequestLimits
	if len(limits) != 2 {
		t.Fatalf("request_limits length = %d, want 2", len(limits))
	}
	if limits[0].Scope != "account" || limits[0].Limit != 100 || limits[0].Window != "24h" || limits[0].Reset != "rolling" {
		t.Fatalf("account rule = %+v", limits[0])
	}
	if limits[1].Scope != "source" || limits[1].Timezone != "Asia/Shanghai" || limits[1].ResetAt != "08:00" {
		t.Fatalf("source rule = %+v", limits[1])
	}
	a := SourceRequestLimit{Scope: "account", Limit: 100, Window: "24h", Reset: "rolling"}
	b := a
	b.Limit = 50
	if a.Signature() != b.Signature() {
		t.Fatalf("limit changed the rule signature: %q != %q", a.Signature(), b.Signature())
	}
	c := a
	c.Window = "1440m"
	if a.Signature() != c.Signature() {
		t.Fatalf("equivalent windows have different signatures: %q != %q", a.Signature(), c.Signature())
	}
	emptyTZ := SourceRequestLimit{Scope: "account", Limit: 1, Window: "1h", Reset: "calendar"}
	explicitLocal := emptyTZ
	explicitLocal.Timezone = "Local"
	if emptyTZ.Signature() != explicitLocal.Signature() {
		t.Fatalf("empty timezone and Local have different signatures: %q != %q", emptyTZ.Signature(), explicitLocal.Signature())
	}
}

func TestEffectiveRequestLimitsFromLegacy(t *testing.T) {
	cfg := SourceConfig{RequestsPerMinute: 120, RequestsPerMinutePerAccount: 30}
	got := cfg.EffectiveRequestLimits()
	if len(got) != 2 {
		t.Fatalf("legacy rules = %+v, want 2", got)
	}
	if got[0].Scope != "source" || got[0].Limit != 120 || got[0].Window != "1m" || got[0].Reset != "first" {
		t.Fatalf("source legacy rule = %+v", got[0])
	}
	if got[1].Scope != "account" || got[1].Limit != 30 || got[1].Window != "1m" || got[1].Reset != "first" {
		t.Fatalf("account legacy rule = %+v", got[1])
	}
	cfg.RequestLimits = []SourceRequestLimit{{Scope: "account", Limit: 7, Window: "1h"}}
	got = cfg.EffectiveRequestLimits()
	if len(got) != 1 || got[0].Limit != 7 {
		t.Fatalf("new rules should win over legacy: %+v", got)
	}
	if got := (SourceConfig{}).EffectiveRequestLimits(); len(got) != 0 {
		t.Fatalf("zero legacy fields produced rules: %+v", got)
	}
}

func TestParseSourcesRejectsInvalidRequestLimits(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"bad scope", `{"relay":{"base_url":"https://relay.example/v1","request_limits":[{"scope":"key","limit":1,"window":"1m"}]}}`, "scope"},
		{"zero limit", `{"relay":{"base_url":"https://relay.example/v1","request_limits":[{"scope":"account","limit":0,"window":"1m"}]}}`, "limit"},
		{"bad window", `{"relay":{"base_url":"https://relay.example/v1","request_limits":[{"scope":"account","limit":1,"window":"later"}]}}`, "window"},
		{"sub-second window", `{"relay":{"base_url":"https://relay.example/v1","request_limits":[{"scope":"account","limit":1,"window":"500ms"}]}}`, "window"},
		{"bad reset", `{"relay":{"base_url":"https://relay.example/v1","request_limits":[{"scope":"account","limit":1,"window":"1m","reset":"sometimes"}]}}`, "reset"},
		{"bad timezone", `{"relay":{"base_url":"https://relay.example/v1","request_limits":[{"scope":"account","limit":1,"window":"1h","reset":"calendar","timezone":"Mars/Olympus"}]}}`, "timezone"},
		{"bad reset_at", `{"relay":{"base_url":"https://relay.example/v1","request_limits":[{"scope":"account","limit":1,"window":"1h","reset":"calendar","reset_at":"25:00"}]}}`, "reset_at"},
		{"calendar window", `{"relay":{"base_url":"https://relay.example/v1","request_limits":[{"scope":"account","limit":1,"window":"7h","reset":"calendar"}]}}`, "calendar"},
		{"duplicate", `{"relay":{"base_url":"https://relay.example/v1","request_limits":[{"scope":"account","limit":1,"window":"1h"},{"scope":"account","limit":2,"window":"60m"}]}}`, "duplicate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseSources([]byte(tc.body))
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), tc.want) {
				t.Fatalf("error = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}
