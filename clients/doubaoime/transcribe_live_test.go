package doubaoime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"client2api/internal/core"
)

// TestLiveTranscribe drives the real Doubao Input Method ASR service. It is
// skipped unless DOUBAOIME_LIVE=1, so `go test ./...` never touches the
// network. Point DOUBAOIME_LIVE_WAV at a 16-bit PCM WAV file; set
// DOUBAOIME_LIVE_DIR to reuse a device's stored credentials instead of
// registering a fresh device under a temp dir.
func TestLiveTranscribe(t *testing.T) {
	if os.Getenv("DOUBAOIME_LIVE") != "1" {
		t.Skip("set DOUBAOIME_LIVE=1 to run the live Doubao IME transcription test")
	}
	wavPath := strings.TrimSpace(os.Getenv("DOUBAOIME_LIVE_WAV"))
	if wavPath == "" {
		t.Fatal("DOUBAOIME_LIVE_WAV must point at a WAV file")
	}
	raw, err := os.ReadFile(wavPath)
	if err != nil {
		t.Fatal(err)
	}

	dir := strings.TrimSpace(os.Getenv("DOUBAOIME_LIVE_DIR"))
	if dir == "" {
		dir = t.TempDir()
	}
	client, err := New(core.Deps{
		DataDir: dir,
		Logf:    func(format string, args ...any) { t.Logf(format, args...) },
	})
	if err != nil {
		t.Fatal(err)
	}
	c := client.(*Client)
	if c.ffmpegPath == "" {
		t.Fatal("ffmpeg was not found; set ffmpeg_path or put ffmpeg on PATH")
	}

	res, err := c.Transcribe(t.Context(), &core.TranscriptionRequest{
		File:           raw,
		Filename:       filepath.Base(wavPath),
		ContentType:    "audio/wav",
		Model:          "whisper-1",
		Language:       strings.TrimSpace(os.Getenv("DOUBAOIME_LIVE_LANG")),
		ResponseFormat: "json",
	})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	t.Logf("transcript: %q", res.Text)
	if strings.TrimSpace(res.Text) == "" {
		t.Fatal("upstream returned an empty transcript")
	}
}
