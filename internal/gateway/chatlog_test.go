package gateway

import (
	"bytes"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// captureChatRows sends the per-request console rows into a buffer for the
// duration of one test and puts the previous destination back afterwards.
func captureChatRows(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevOn := chatLogOut, chatLogEnabled
	SetChatLogOutput(&buf)
	chatLogEnabled = true
	t.Cleanup(func() {
		SetChatLogOutput(prevOut)
		chatLogEnabled = prevOn
	})
	return &buf
}

func chatRows(buf *bytes.Buffer) []string {
	raw := strings.TrimRight(buf.String(), "\n")
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\n")
}

// TestChatRowRendersEveryColumn pins the console line byte for byte, because
// the column widths are what make the log readable when the columns are not
// aligned by a viewer.
func TestChatRowRendersEveryColumn(t *testing.T) {
	buf := captureChatRows(t)

	logChatRow(812*time.Millisecond, 7900*time.Millisecond,
		"workbuddy/glm-5.2", "stream", "5f4a1c2e77bb", "work", http.StatusOK, 486)

	lines := chatRows(buf)
	if len(lines) != 1 {
		t.Fatalf("wrote %d rows, want 1: %q", len(lines), buf.String())
	}
	// 486 tokens over 7.9s is 61.5 tok/s; each field is padded to its column
	// width (model 26, account 22, ttfb 8, tokens 6, rate 11) and the " | "
	// separator then adds one more space before the next pipe.
	want := regexp.MustCompile(`^\| #\d{3} \| \d{2}:\d{2}:\d{2} \| ` +
		`workbuddy/glm-5\.2 {10}\| stream \| 200 \| ` +
		`work\(5f4a1c2e\) {9}\| TTFB=812ms {4}\| tok=486 {4}\| ` +
		`61\.5tok/s {3}\| total=7\.9s \|$`)
	if !want.MatchString(lines[0]) {
		t.Errorf("row = %q\nwant match %s", lines[0], want)
	}
}

// TestChatRowSaysWhenTheUpstreamWasSilent covers toks == -1, which is how "the
// upstream never reported usage" is told apart from "the upstream reported 0".
func TestChatRowSaysWhenTheUpstreamWasSilent(t *testing.T) {
	buf := captureChatRows(t)

	logChatRow(time.Second, 2*time.Second, "t/m1", "sync", "abcdefgh1234", "", http.StatusOK, -1)

	line := chatRows(buf)[0]
	if !strings.Contains(line, "TTFB=1000ms") {
		t.Errorf("row = %q, want TTFB=1000ms", line)
	}
	if !strings.Contains(line, "| tok=-      | -           | total=") {
		t.Errorf("row = %q, want an empty token and rate field", line)
	}
	if strings.Contains(line, "tok/s") {
		t.Errorf("row = %q, want no rate for a request with no token count", line)
	}
}

// TestChatRowAlignsACJKNickName is the reason padding counts display columns:
// a byte-counted pad would leave this account column short of its neighbours.
func TestChatRowAlignsACJKNickName(t *testing.T) {
	buf := captureChatRows(t)

	logChatRow(10*time.Millisecond, time.Second, "t/m1", "sync", "5f4a1c2e77bb", "工作号", http.StatusOK, 3)

	line := chatRows(buf)[0]
	if !strings.Contains(line, "工作号(5f4a1c2e)       |") {
		t.Errorf("row = %q, want the CJK account padded to seven columns", line)
	}
}

func TestChatRowTruncatesALongModelName(t *testing.T) {
	buf := captureChatRows(t)

	long := "workbuddy/" + strings.Repeat("m", 40)
	logChatRow(time.Second, time.Second, long, "stream", "", "", http.StatusOK, 1)

	line := chatRows(buf)[0]
	fields := strings.Split(line, "|")
	if len(fields) < 4 {
		t.Fatalf("row = %q, want at least four pipe-separated fields", line)
	}
	model := strings.TrimSpace(fields[3])
	if len(model) != chatModelWidth {
		t.Errorf("model field = %q (%d chars), want %d", model, len(model), chatModelWidth)
	}
	if !strings.HasSuffix(model, "m") {
		t.Errorf("model field = %q, want the head of the original name", model)
	}
}

func TestChatStatDoneWritesOnceAndCarriesWhatItLearned(t *testing.T) {
	buf := captureChatRows(t)

	start := time.Now()
	stat := newChatStat(start, "t/m1", true)
	if stat.mode != "stream" {
		t.Errorf("mode = %q, want stream", stat.mode)
	}
	if stat.toks != -1 {
		t.Errorf("toks = %d, want -1 before the upstream reports anything", stat.toks)
	}
	// A second first-frame must not move the TTFB marker.
	stat.noteFirstFrame(start.Add(500 * time.Millisecond))
	stat.noteFirstFrame(start.Add(900 * time.Millisecond))
	stat.noteUsage(&core.Usage{PromptTokens: 4, CompletionTokens: 12, TotalTokens: 16})
	stat.uid = "abcdefgh1234"
	stat.status = http.StatusOK

	stat.done()
	stat.done()

	lines := chatRows(buf)
	if len(lines) != 1 {
		t.Fatalf("wrote %d rows, want exactly 1: %q", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], "TTFB=500ms") {
		t.Errorf("row = %q, want the first frame's TTFB", lines[0])
	}
	if !strings.Contains(lines[0], "tok=12") {
		t.Errorf("row = %q, want the completion token count", lines[0])
	}
	if !strings.Contains(lines[0], "| abcdefgh ") {
		t.Errorf("row = %q, want the account label", lines[0])
	}
}

// TestChatStatFallsBackToTheTotalTokenCount covers an upstream that reports only
// a total: the rate column is more useful with a number than with a dash.
func TestChatStatFallsBackToTheTotalTokenCount(t *testing.T) {
	stat := newChatStat(time.Now(), "t/m1", false)
	stat.noteUsage(&core.Usage{PromptTokens: 5, TotalTokens: 17})
	if stat.toks != 17 {
		t.Errorf("toks = %d, want 17 from the total", stat.toks)
	}
}

func TestChatStatIgnoresAMissingUsage(t *testing.T) {
	stat := newChatStat(time.Now(), "t/m1", false)
	stat.noteUsage(nil)
	if stat.toks != -1 {
		t.Errorf("toks = %d, want -1 when no usage was reported", stat.toks)
	}
}

func TestChatRowIsSilentWhenDisabled(t *testing.T) {
	buf := captureChatRows(t)
	chatLogEnabled = false

	logChatRow(time.Second, time.Second, "t/m1", "sync", "abcdefgh", "", http.StatusOK, 1)

	if buf.Len() != 0 {
		t.Errorf("wrote %q with the console rows disabled", buf.String())
	}
}

// TestChatWritesOneConsoleRowPerOutcome drives the real handler so that the
// status column is proven to be set on every exit, not just the happy one.
func TestChatWritesOneConsoleRowPerOutcome(t *testing.T) {
	buf := captureChatRows(t)
	stats := NewStats()
	usage := NewUsageStore(20)

	t.Run("success", func(t *testing.T) {
		buf.Reset()
		srv := newTestServer(t, &testClient{name: "t", events: usageEvents(10, 5)}, stats, usage)
		if rec := chat(t, srv, bufferedBody); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		lines := chatRows(buf)
		if len(lines) != 1 {
			t.Fatalf("wrote %d rows, want 1: %q", len(lines), buf.String())
		}
		line := lines[0]
		if !strings.Contains(line, "| t/m1 ") {
			t.Errorf("row = %q, want the resolved client and model", line)
		}
		if !strings.Contains(line, "| sync |") {
			t.Errorf("row = %q, want the sync mode for a buffered request", line)
		}
		if !strings.Contains(line, "| 200 |") {
			t.Errorf("row = %q, want status 200", line)
		}
		if !strings.Contains(line, "tok=5") {
			t.Errorf("row = %q, want the completion token count", line)
		}
	})

	t.Run("unknown model", func(t *testing.T) {
		buf.Reset()
		srv := newTestServer(t, &testClient{name: "t"}, stats, usage)
		body := `{"model":"nope","messages":[{"role":"user","content":"hi"}]}`
		if rec := chat(t, srv, body); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		line := chatRows(buf)[0]
		if !strings.Contains(line, "| nope ") {
			t.Errorf("row = %q, want the model the caller asked for", line)
		}
		if !strings.Contains(line, "| 404 |") {
			t.Errorf("row = %q, want status 404", line)
		}
	})

	t.Run("malformed body", func(t *testing.T) {
		buf.Reset()
		srv := newTestServer(t, &testClient{name: "t"}, stats, usage)
		if rec := chat(t, srv, "{not json"); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		line := chatRows(buf)[0]
		if !strings.Contains(line, "| 400 |") {
			t.Errorf("row = %q, want status 400", line)
		}
	})

	t.Run("module failure names its account", func(t *testing.T) {
		buf.Reset()
		boom := core.Fail("t", "acct-1234567890", core.FailureQuota, http.StatusPaymentRequired, errors.New("quota gone"))
		srv := newTestServer(t, &testClient{name: "t", chatErr: boom}, stats, usage)
		if rec := chat(t, srv, bufferedBody); rec.Code != http.StatusPaymentRequired {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		line := chatRows(buf)[0]
		if !strings.Contains(line, "| acct-123") {
			t.Errorf("row = %q, want the failing account's short id", line)
		}
		if !strings.Contains(line, "| 402 |") {
			t.Errorf("row = %q, want the failure's own status", line)
		}
	})
}
