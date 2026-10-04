package core

import (
	"testing"
	"time"
)

func TestPlatformHealthAlertsOnceAndSuppresses(t *testing.T) {
	h := NewPlatformHealth()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.Local)

	for i := 1; i <= 2; i++ {
		ev := h.NoteFailure("tabbit", "deepseek-v4.1-flash", now.Add(time.Duration(i)*time.Minute))
		if ev.Failures != i || ev.Alert || ev.Suppressed {
			t.Fatalf("failure %d event = %+v, want Failures=%d without alert", i, ev, i)
		}
		if h.Suppressed("tabbit", "deepseek-v4.1-flash", now.Add(time.Duration(i)*time.Minute)) {
			t.Fatal("platform was suppressed before the threshold")
		}
	}

	tripped := h.NoteFailure("tabbit", "deepseek-v4.1-flash", now.Add(3*time.Minute))
	if tripped.Failures != 3 || !tripped.Alert || !tripped.Suppressed {
		t.Fatalf("third failure = %+v, want an alert and suppression", tripped)
	}
	if !h.Suppressed("tabbit", "deepseek-v4.1-flash", now.Add(9*time.Minute)) {
		t.Fatal("platform stopped being suppressed before the cooldown elapsed")
	}
	if again := h.NoteFailure("tabbit", "deepseek-v4.1-flash", now.Add(4*time.Minute)); again.Alert {
		t.Fatalf("alert repeated while already suppressed: %+v", again)
	}
	if h.Suppressed("tabbit", "deepseek-v4.1-flash", now.Add(13*time.Minute+time.Second)) {
		t.Fatal("platform remained suppressed after the cooldown elapsed")
	}
}

// A platform that is merely unable to serve right now -- every account busy or
// cooling, no usable account at all -- is not a failed platform.  It must be
// demoted for a short window without raising the failure alert, and a success
// must clear that demotion immediately.
func TestPlatformHealthDemotesUnavailableWithoutAlerting(t *testing.T) {
	h := NewPlatformHealth()
	now := time.Date(2026, 10, 4, 23, 36, 0, 0, time.Local)

	h.NoteUnavailable("workbuddy", "cn:deepseek-v4.1-flash", now)
	if h.Suppressed("workbuddy", "cn:deepseek-v4.1-flash", now.Add(time.Second)) {
		t.Fatal("an unavailable platform must not enter the failure cooldown")
	}
	if !h.Degraded("workbuddy", "cn:deepseek-v4.1-flash", now.Add(time.Second)) {
		t.Fatal("an unavailable platform was not demoted")
	}
	if h.Degraded("workbuddy", "cn:deepseek-v4.1-flash", now.Add(PlatformUnavailableCooldown+time.Second)) {
		t.Fatal("the demotion outlived its short window")
	}

	h.NoteUnavailable("workbuddy", "cn:deepseek-v4.1-flash", now)
	h.NoteSuccess("workbuddy", "cn:deepseek-v4.1-flash", now.Add(time.Second))
	if h.Degraded("workbuddy", "cn:deepseek-v4.1-flash", now.Add(2*time.Second)) {
		t.Fatal("a success must clear the demotion")
	}
}

// A real failure is stronger evidence than a busy signal: it must take over the
// entry and start the full failure cooldown rather than inheriting a demotion.
func TestPlatformHealthFailureSupersedesUnavailable(t *testing.T) {
	h := NewPlatformHealth()
	now := time.Date(2026, 10, 4, 23, 36, 0, 0, time.Local)

	h.NoteUnavailable("tabbit", "m", now)
	h.NoteFailure("tabbit", "m", now.Add(time.Second))
	h.NoteFailure("tabbit", "m", now.Add(2*time.Second))
	tripped := h.NoteFailure("tabbit", "m", now.Add(3*time.Second))
	if !tripped.Alert || !tripped.Suppressed {
		t.Fatalf("third real failure = %+v, want an alert and suppression", tripped)
	}
}

func TestPlatformHealthWindowExpiresAndSuccessClears(t *testing.T) {
	h := NewPlatformHealth()
	start := time.Date(2026, 10, 3, 12, 0, 0, 0, time.Local)
	h.NoteFailure("alpha", "m", start)
	ev := h.NoteFailure("alpha", "m", start.Add(11*time.Minute))
	if ev.Failures != 1 {
		t.Fatalf("failure after the window = %+v, want a fresh count of 1", ev)
	}

	h.NoteSuccess("alpha", "m", start.Add(12*time.Minute))
	ev = h.NoteFailure("alpha", "m", start.Add(13*time.Minute))
	if ev.Failures != 1 || ev.Alert || ev.Suppressed {
		t.Fatalf("failure after success = %+v, want a fresh healthy state", ev)
	}
}
