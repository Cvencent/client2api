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
