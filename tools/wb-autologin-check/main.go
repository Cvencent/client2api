//go:build ignore

// Command wb-autologin-check runs the WorkBuddy CN browser auto-login end to
// end against the real vendor, using a rented SMS number, so the login flow can
// be verified before a release.  It is a developer harness, not a shipped
// binary, so it carries the `//go:build ignore` constraint below.  That keeps
// it out of `go vet ./...`, `go build ./...` and `go test ./...` while `go run`
// still compiles and runs it on demand.  See tools/README.md.
//
// It needs a live SMS-platform credential and the number to reuse:
//
//	set WB_REAL_PHONE=13800000000
//	set WB_REAL_TOKEN=<sms-platform-token>
//	go run ./tools/wb-autologin-check/main.go
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"client2api/clients/workbuddy"
	"client2api/internal/core"
)

func main() {
	phone := strings.TrimSpace(os.Getenv("WB_REAL_PHONE"))
	token := strings.TrimSpace(os.Getenv("WB_REAL_TOKEN"))
	if phone == "" || token == "" {
		fmt.Fprintln(os.Stderr, "WB_REAL_PHONE and WB_REAL_TOKEN are required")
		os.Exit(2)
	}

	dir, err := os.MkdirTemp("", "client2api-wb-realcheck-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "temp dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	raw, err := workbuddy.New(core.Deps{
		DataDir: dir,
		Config:  []byte(`{"login_realm":"cn","browser_headless":true,"sms_interval_seconds":5,"sms_polls":12}`),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "New: %v\n", err)
		os.Exit(1)
	}
	provider, ok := raw.(core.AutoLoginProvider)
	if !ok {
		fmt.Fprintln(os.Stderr, "workbuddy client does not implement AutoLoginProvider")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()
	job, err := provider.StartAutoLogin(ctx, core.AutoLoginRequest{
		Realm: "cn",
		Phone: phone,
		Token: token,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "StartAutoLogin: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("job started: %s\n", job.ID)

	seen := 0
	deadline := time.Now().Add(6 * time.Minute)
	for {
		snap, err := provider.PollAutoLogin(ctx, job.ID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "PollAutoLogin: %v\n", err)
			os.Exit(1)
		}
		for ; seen < len(snap.Log); seen++ {
			fmt.Printf("%s %s\n", snap.Log[seen].At, snap.Log[seen].Text)
		}
		if snap.State != core.AutoLoginRunning {
			fmt.Printf("final state=%s message=%s account_id=%s\n", snap.State, snap.Message, snap.AccountID)
			if snap.State != core.AutoLoginSuccess {
				os.Exit(1)
			}
			return
		}
		if time.Now().After(deadline) {
			_ = provider.CancelAutoLogin(context.Background(), job.ID)
			fmt.Fprintln(os.Stderr, "live CN auto-login did not finish within 6 minutes")
			os.Exit(1)
		}
		time.Sleep(2 * time.Second)
	}
}
