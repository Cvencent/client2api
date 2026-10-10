package qoder

import (
	"context"
	"strings"
	"testing"

	"client2api/internal/core"
)

func TestAliyunFrameMatchTargetsTheSMSLoginForm(t *testing.T) {
	if !strings.Contains(aliyunFrameMatch, "appEntrance=qoder_sms") {
		t.Fatalf("aliyunFrameMatch = %q, want the SMS-login frame marker", aliyunFrameMatch)
	}
}

func TestCancelAutoLoginMarksARunningJobCancelled(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	job := &autoJob{
		id:     "qoder-auto-test",
		state:  core.AutoLoginRunning,
		step:   "browser",
		cancel: cancel,
	}
	c := &Client{}
	c.putAutoJob(job)

	if err := c.CancelAutoLogin(context.Background(), job.id); err != nil {
		t.Fatalf("CancelAutoLogin: %v", err)
	}
	got, err := c.PollAutoLogin(context.Background(), job.id)
	if err != nil {
		t.Fatalf("PollAutoLogin: %v", err)
	}
	if got.State != core.AutoLoginCancelled {
		t.Fatalf("state = %q, want %q", got.State, core.AutoLoginCancelled)
	}
}
