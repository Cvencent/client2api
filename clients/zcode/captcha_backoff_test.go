package zcode

import (
	"context"
	"errors"
	"testing"
	"time"

	"client2api/internal/core"
)

func TestCaptchaBackoffTripsAtThresholdAndDoubles(t *testing.T) {
	b := newCaptchaBackoff()
	now := time.Unix(0, 0)

	// Under the threshold there is no cooldown.
	for i := 0; i < captchaBackoffThreshold-1; i++ {
		if got := b.noteFailure(now); got != 0 {
			t.Fatalf("failure %d produced a cooldown %v, want none before the threshold", i+1, got)
		}
		if got := b.remaining(now); got != 0 {
			t.Fatalf("failure %d closed the gate early: %v", i+1, got)
		}
	}
	// The threshold failure opens the gate for exactly the base interval.
	if got := b.noteFailure(now); got != captchaBackoffBase {
		t.Fatalf("threshold cooldown = %v, want %v", got, captchaBackoffBase)
	}
	if got := b.remaining(now); got != captchaBackoffBase {
		t.Fatalf("remaining = %v, want %v", got, captchaBackoffBase)
	}
	// The next failure doubles, not starts over.
	if got := b.noteFailure(now); got != 2*captchaBackoffBase {
		t.Fatalf("second cooldown = %v, want %v", got, 2*captchaBackoffBase)
	}
}

func TestCaptchaBackoffCapsAtMax(t *testing.T) {
	b := newCaptchaBackoff()
	now := time.Unix(0, 0)
	for i := 0; i < 40; i++ {
		b.noteFailure(now)
	}
	if got := b.remaining(now); got != captchaBackoffMax {
		t.Fatalf("remaining = %v, want the %v ceiling", got, captchaBackoffMax)
	}
}

func TestCaptchaBackoffClearsOnSuccessButKeepsStreakOnLapse(t *testing.T) {
	b := newCaptchaBackoff()
	now := time.Unix(0, 0)
	for i := 0; i < captchaBackoffThreshold; i++ {
		b.noteFailure(now)
	}
	// Letting the clock pass the deadline must clear the gate but keep the
	// streak, so the next failure keeps escalating.
	later := now.Add(captchaBackoffBase + time.Second)
	if got := b.remaining(later); got != 0 {
		t.Fatalf("remaining after the deadline = %v, want 0", got)
	}
	if got := b.noteFailure(later); got != 2*captchaBackoffBase {
		t.Fatalf("post-lapse cooldown = %v, want the escalated %v", got, 2*captchaBackoffBase)
	}
	b.noteSuccess()
	if got := b.remaining(later); got != 0 {
		t.Fatalf("success must clear the gate, got %v", got)
	}
	// After a success the streak restarts, so the threshold must be reached
	// again from scratch before the gate closes.
	for i := 0; i < captchaBackoffThreshold-1; i++ {
		if got := b.noteFailure(later); got != 0 {
			t.Fatalf("post-success failure %d closed the gate early: %v", i+1, got)
		}
	}
	if got := b.noteFailure(later); got != captchaBackoffBase {
		t.Fatalf("streak after success = %v, want a fresh %v", got, captchaBackoffBase)
	}
}

func TestSolveCaptchaBacksOffAfterRepeatedMintFailures(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false,"captcha_browser":true}`)
	ft := routeTransport(t, captchaRoutes(captchaConfigFixture, nil))
	c := env.client(t, ft)

	var calls int
	c.mintBrowser = func(context.Context, regionInfo) (string, error) {
		calls++
		return "", errors.New("browser produced no parameter")
	}

	for i := 0; i < captchaBackoffThreshold; i++ {
		if _, _, err := c.solveCaptcha(context.Background()); err == nil {
			t.Fatalf("attempt %d succeeded, want a mint failure", i+1)
		}
	}
	if calls != captchaBackoffThreshold {
		t.Fatalf("mint calls = %d, want %d", calls, captchaBackoffThreshold)
	}
	// Once the breaker is open the solver must not even be consulted.
	_, _, err := c.solveCaptcha(context.Background())
	if err == nil {
		t.Fatal("expected the gate to refuse the mint")
	}
	if !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("gate error = %v, want ErrNotConfigured", err)
	}
	if calls != captchaBackoffThreshold {
		t.Fatalf("mint calls = %d, the gate must not call the solver while open", calls)
	}
}
