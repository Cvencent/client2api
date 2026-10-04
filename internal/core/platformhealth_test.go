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
