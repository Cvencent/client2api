package browser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 钉住持久 profile 的契约（issue #183）：LaunchOpts.Profile 直接作
// --user-data-dir 且 Close 不删；只传 ProfileRoot 的旧路径行为不变
// （MkdirTemp + Close 删除）。#183 的根因是 ProfileRoot 只固定了一次性
// profile 的父目录——「固定目录」并未让历史积累发生过。
func TestPersistentProfileIsUsedVerbatimAndSurvivesClose(t *testing.T) {
	raw, err := os.ReadFile("cdp.go")
	if err != nil {
		t.Fatalf("read cdp.go: %v", err)
	}
	src := string(raw)

	// Launch 必须先看 Profile 再看 ProfileRoot。
	optsAt := strings.Index(src, "var profile string")
	if optsAt < 0 {
		t.Fatal("Launch 的 profile 分支不见了")
	}
	branch := src[optsAt : optsAt+900]
	if !strings.Contains(branch, "opts.Profile") {
		t.Fatal("Launch 不再消费 opts.Profile：持久 profile 失效（issue #183）")
	}
	// Close 必须在 persistent 分支提前返回，不得删除目录。
	closeAt := strings.Index(src, "func (b *Browser) Close()")
	if closeAt < 0 {
		t.Fatal("Close 不见了")
	}
	closeBody := src[closeAt : closeAt+800]
	if !strings.Contains(closeBody, "if b.persistent {") || !strings.Contains(closeBody, "return") {
		t.Fatal("Close 没有为持久 profile 跳过删除：历史积累会被清掉（issue #183）")
	}
	// 反向钉子：一次性路径的 MkdirTemp + RemoveAll 必须仍在（旧调用方行为不变）。
	if !strings.Contains(src, "os.MkdirTemp(root,") || !strings.Contains(src, "os.RemoveAll(b.profile)") {
		t.Error("一次性 profile 的创建/清理语义被动过：只传 ProfileRoot 的调用方行为必须保持（issue #183）")
	}
}

// 本机真浏览器双 Launch：第二次能读到第一次留下的文件，Close 后目录仍在。
// 需要 C2A_LIVE_CDP=1（与 TestLiveLaunch 同一开关）。
func TestLivePersistentProfileKeepsState(t *testing.T) {
	if os.Getenv("C2A_LIVE_CDP") == "" {
		t.Skip("set C2A_LIVE_CDP=1 to run the live browser test")
	}
	profile := filepath.Join(t.TempDir(), "persistent-profile")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	marker := "persisted-" + filepath.Base(profile)

	b1, err := Launch(ctx, LaunchOpts{Profile: profile, Headless: true, StartURL: "about:blank", Timeout: 45 * time.Second})
	if err != nil {
		t.Fatalf("launch 1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(profile, "marker.txt"), []byte(marker), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	b1.Close()
	if _, err := os.Stat(filepath.Join(profile, "marker.txt")); err != nil {
		t.Fatalf("Close removed the persistent profile: %v", err)
	}

	b2, err := Launch(ctx, LaunchOpts{Profile: profile, Headless: true, StartURL: "about:blank", Timeout: 45 * time.Second})
	if err != nil {
		t.Fatalf("launch 2: %v", err)
	}
	defer b2.Close()
	raw, err := os.ReadFile(filepath.Join(profile, "marker.txt"))
	if err != nil || string(raw) != marker {
		t.Fatalf("the second launch did not see the first launch's file: %q, %v", raw, err)
	}
}
