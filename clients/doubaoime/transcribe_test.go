package doubaoime

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
	"golang.org/x/net/websocket"
)

func TestTranscribeRunsDoubaoIMESequence(t *testing.T) {
	var (
		mu        sync.Mutex
		frameData [][]byte
		states    []int
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"device_id":"90001"}}`)
	})
	mux.HandleFunc("/settings", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"settings":{"asr_config":{"app_key":"app-key"}}}}`)
	})
	mux.Handle("/ws", websocket.Handler(func(ws *websocket.Conn) {
		if got := ws.Request().Header.Get("proto-version"); got != "v2" {
			t.Errorf("proto-version = %q", got)
			return
		}
		if got := ws.Request().Header.Get("x-custom-keepalive"); got != "true" {
			t.Errorf("keepalive = %q", got)
			return
		}
		for {
			var raw []byte
			if err := websocket.Message.Receive(ws, &raw); err != nil {
				return
			}
			msg, err := parseMessage(raw)
			if err != nil {
				t.Errorf("parse request: %v", err)
				return
			}
			if msg.Token != "app-key" || msg.ServiceName != "ASR" || msg.RequestID == "" {
				t.Errorf("request fields = %+v", msg)
				return
			}
			switch msg.MethodName {
			case "StartTask":
				_ = websocket.Message.Send(ws, marshalMessage(wsMessage{RequestID: msg.RequestID, MessageType: "TaskStarted", StatusCode: 20000000}))
			case "StartSession":
				_ = websocket.Message.Send(ws, marshalMessage(wsMessage{RequestID: msg.RequestID, MessageType: "SessionStarted", StatusCode: 20000000}))
			case "TaskRequest":
				mu.Lock()
				frameData = append(frameData, append([]byte(nil), msg.AudioData...))
				states = append(states, msg.FrameState)
				mu.Unlock()
			case "FinishSession":
				_ = websocket.Message.Send(ws, marshalMessage(wsMessage{
					RequestID:   msg.RequestID,
					MessageType: "SessionFinished",
					StatusCode:  20000000,
					ResultJSON:  `{"results":[{"text":"请帮我把首页","is_interim":false},{"text":"的任务打开。","is_interim":false,"is_vad_finished":true}]}`,
				}))
				return
			}
		}
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	cfg := fmt.Sprintf(`{"register_url":%q,"settings_url":%q,"websocket_url":%q,"request_timeout":"2s","start_timeout":"2s","result_timeout":"2s"}`,
		srv.URL+"/register", srv.URL+"/settings", wsURL)
	c := newTestClient(t, cfg)
	c.encode = func(context.Context, []int16) ([][]byte, error) {
		return [][]byte{{1, 2}, {3, 4}}, nil
	}

	wav := makeTestWAV(16000, 1, 16, []int16{200, 400, 200, -200, -400})
	got, err := c.Transcribe(t.Context(), &core.TranscriptionRequest{File: wav, Model: "whisper-1", Language: "zh"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "请帮我把首页的任务打开。" {
		t.Fatalf("text = %q", got.Text)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(frameData) != 2 || !bytes.Equal(frameData[0], []byte{1, 2}) || !bytes.Equal(frameData[1], []byte{3, 4}) {
		t.Fatalf("frames = %v", frameData)
	}
	if len(states) != 2 || states[0] != 1 || states[1] != 9 {
		t.Fatalf("states = %v", states)
	}
}

func TestTranscribeRejectsInvalidWAVBeforeNetwork(t *testing.T) {
	c := newTestClient(t, `{"ffmpeg_path":"missing"}`)
	_, err := c.Transcribe(t.Context(), &core.TranscriptionRequest{File: []byte("bad")})
	if err == nil || !strings.Contains(err.Error(), "invalid audio") {
		t.Fatalf("error = %v", err)
	}
}

func TestTranscribeRejectsPromptBeforeNetwork(t *testing.T) {
	c := newTestClient(t, `{"ffmpeg_path":"missing"}`)
	_, err := c.Transcribe(t.Context(), &core.TranscriptionRequest{
		File:   makeTestWAV(16000, 1, 16, []int16{200, 400, 200}),
		Prompt: "OpenAI, GPT",
	})
	if err == nil || !strings.Contains(err.Error(), "prompt is not supported") {
		t.Fatalf("error = %v", err)
	}
}

func TestDecodeAudioUsesFFmpegForNonWAV(t *testing.T) {
	ffmpeg := findFFmpeg("")
	if ffmpeg == "" {
		t.Skip("ffmpeg is not installed")
	}
	pcm := make([]int16, 16000)
	for i := range pcm {
		pcm[i] = int16(600 * math.Sin(float64(i)/8))
	}
	dir := t.TempDir()
	wavPath := filepath.Join(dir, "audio.wav")
	if err := os.WriteFile(wavPath, makeTestWAV(16000, 1, 16, pcm), 0o600); err != nil {
		t.Fatal(err)
	}
	oggPath := filepath.Join(dir, "audio.ogg")
	cmd := exec.CommandContext(t.Context(), ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", wavPath, "-c:a", "libopus", oggPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create ogg fixture: %v: %s", err, out)
	}
	raw, err := os.ReadFile(oggPath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeAudio(t.Context(), ffmpeg, raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.samples) == 0 || got.sampleRate != 16000 || got.channels != 1 {
		t.Fatalf("decoded audio = %+v", got)
	}
}

func TestVendoredFFmpegDecodesCommonUploadFormats(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the vendored ffmpeg is a Windows binary")
	}
	ffmpeg := filepath.Join("..", "..", "third_party", "ffmpeg", "ffmpeg.exe")
	if _, err := os.Stat(ffmpeg); err != nil {
		t.Skipf("vendored ffmpeg is not present: %v", err)
	}
	systemFFmpeg := findFFmpeg("")
	if systemFFmpeg == "" || samePath(systemFFmpeg, ffmpeg) {
		t.Skip("a full ffmpeg is needed to create the format fixtures")
	}

	pcm := make([]int16, 16000)
	for i := range pcm {
		pcm[i] = int16(6000 * math.Sin(float64(i)/8))
	}
	dir := t.TempDir()
	wavPath := filepath.Join(dir, "source.wav")
	if err := os.WriteFile(wavPath, makeTestWAV(16000, 1, 16, pcm), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		ext string
		enc []string
	}{
		{"mp3", []string{"-c:a", "libmp3lame"}},
		{"m4a", []string{"-c:a", "aac"}},
		{"ogg", []string{"-c:a", "libvorbis"}},
		{"flac", []string{"-c:a", "flac"}},
		{"webm", []string{"-c:a", "libopus"}},
	}
	for _, tc := range cases {
		t.Run(tc.ext, func(t *testing.T) {
			path := filepath.Join(dir, "sample."+tc.ext)
			args := []string{"-hide_banner", "-loglevel", "error", "-y", "-i", wavPath}
			args = append(args, tc.enc...)
			args = append(args, path)
			if out, err := exec.CommandContext(t.Context(), systemFFmpeg, args...).CombinedOutput(); err != nil {
				t.Skipf("the system ffmpeg cannot create a %s fixture: %v: %s", tc.ext, err, out)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			got, err := decodeAudio(t.Context(), ffmpeg, raw)
			if err != nil {
				t.Fatalf("decode %s: %v", tc.ext, err)
			}
			if len(got.samples) < 1000 || got.sampleRate != 16000 || got.channels != 1 {
				t.Fatalf("decoded %s audio = %d samples, %d Hz, %d channels", tc.ext, len(got.samples), got.sampleRate, got.channels)
			}
		})
	}
}

func samePath(a, b string) bool {
	absoluteA, errA := filepath.Abs(a)
	absoluteB, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return strings.EqualFold(filepath.Clean(absoluteA), filepath.Clean(absoluteB))
}

func TestTranscribeSurfacesLifecycleErrorStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"device_id":"90001"}}`)
	})
	mux.HandleFunc("/settings", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"app_key":"app-key"}}`)
	})
	mux.Handle("/ws", websocket.Handler(func(ws *websocket.Conn) {
		var raw []byte
		if err := websocket.Message.Receive(ws, &raw); err != nil {
			return
		}
		msg, _ := parseMessage(raw)
		_ = websocket.Message.Send(ws, marshalMessage(wsMessage{
			RequestID: msg.RequestID, MessageType: "TaskStarted", StatusCode: 40100000, StatusMessage: "unauthorized",
		}))
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	cfg := fmt.Sprintf(`{"register_url":%q,"settings_url":%q,"websocket_url":%q}`, srv.URL+"/register", srv.URL+"/settings", wsURL)
	c := newTestClient(t, cfg)
	c.encode = func(context.Context, []int16) ([][]byte, error) { return [][]byte{{1, 2}}, nil }

	wav := makeTestWAV(16000, 1, 16, []int16{200, 400, 200, -200, -400})
	_, err := c.Transcribe(t.Context(), &core.TranscriptionRequest{File: wav, Model: "whisper-1"})
	if err == nil || !strings.Contains(err.Error(), "40100000") {
		t.Fatalf("error = %v", err)
	}
}

func newTestClient(t *testing.T, rawConfig string) *Client {
	t.Helper()
	client, err := New(core.Deps{DataDir: t.TempDir(), Config: []byte(rawConfig)})
	if err != nil {
		t.Fatal(err)
	}
	c := client.(*Client)
	c.cfg.RequestTimeout = durationField(time.Second.String())
	c.cfg.StartTimeout = durationField(time.Second.String())
	c.cfg.ResultTimeout = durationField(time.Second.String())
	return c
}
