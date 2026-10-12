package doubaoime

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
