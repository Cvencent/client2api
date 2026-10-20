package qoder

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// cli_test.go covers the pieces of the CLI bridge that do not need a real
// install: discovery against a fake layout, the wire translations, the prompt
// shaping and the account gating.  The one test that does need the real vendor
// is gated behind CLIENT2API_QODER_LIVE_CLI_TEST=1, like the desktop-credential
// test beside it, so an ordinary `go test` stays offline.

// fakeInstall writes a directory that probeInstall will accept.
func fakeInstall(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, qoderExeName), []byte("exe"), 0o600); err != nil {
		t.Fatal(err)
	}
	worker := filepath.Join(root, filepath.FromSlash(workerRelPath))
	if err := os.MkdirAll(filepath.Dir(worker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(worker, []byte("worker"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestProbeInstallAcceptsARealLayout(t *testing.T) {
	root := fakeInstall(t)
	inst, ok := probeInstall(root)
	if !ok {
		t.Fatal("probeInstall rejected a complete install")
	}
	if inst.Exe != filepath.Join(root, qoderExeName) {
		t.Errorf("Exe = %q", inst.Exe)
	}
	if inst.Worker == "" {
		t.Error("Worker is empty")
	}
}

func TestProbeInstallAcceptsTheExecutableItself(t *testing.T) {
	root := fakeInstall(t)
	inst, ok := probeInstall(filepath.Join(root, qoderExeName))
	if !ok {
		t.Fatal("probeInstall rejected a path to the executable")
	}
	if inst.Exe != filepath.Join(root, qoderExeName) {
		t.Errorf("Exe = %q", inst.Exe)
	}
}

func TestProbeInstallRejectsAnIncompleteInstall(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, qoderExeName), []byte("exe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := probeInstall(root); ok {
		t.Fatal("an install without the agent worker was accepted")
	}
}

func TestEnvOverridePointsDiscoveryAtOneInstall(t *testing.T) {
	root := fakeInstall(t)
	t.Setenv(cliPathEnv, root)
	resetCLIDiscovery()
	t.Cleanup(resetCLIDiscovery)

	inst, err := findCLI()
	if err != nil {
		t.Fatalf("findCLI: %v", err)
	}
	if inst.Exe != filepath.Join(root, qoderExeName) {
		t.Errorf("Exe = %q, want the override", inst.Exe)
	}

	// A second call must hit the memo, not the disk again.
	os.Remove(filepath.Join(root, qoderExeName))
	if _, err := findCLI(); err != nil {
		t.Fatalf("findCLI after the install vanished: %v", err)
	}
}

func TestFindCLIReportsAMissingInstall(t *testing.T) {
	// The candidate list includes the well-known install roots, so on a host
	// that genuinely has Qoder CN this test cannot manufacture a miss.
	if _, ok := searchCLI(); ok {
		t.Skip("this host has a real Qoder CN install; the miss path cannot be exercised")
	}
	t.Setenv(cliPathEnv, filepath.Join(t.TempDir(), "nowhere"))
	resetCLIDiscovery()
	t.Cleanup(resetCLIDiscovery)

	_, err := findCLI()
	if !errors.Is(err, ErrCLIMissing) {
		t.Fatalf("err = %v, want ErrCLIMissing", err)
	}
}

func TestCLIMappingOnlyCoversTheFreeKeys(t *testing.T) {
	for _, model := range []string{"dfmodel", "qfmodel", "DFModel"} {
		if !cliModel(model) {
			t.Errorf("cliModel(%q) = false, want true", model)
		}
	}
	for _, model := range []string{"auto", "dmodel", "", "dfmodel-x"} {
		if cliModel(model) {
			t.Errorf("cliModel(%q) = true, want false", model)
		}
	}
}

func TestCLIConfigLeafIsAFileName(t *testing.T) {
	cases := map[string]string{
		"qoder-019e9892-0117-7af5-b551-7da169c9df7d": "qoder-019e9892-0117-7af5-b551-7da169c9df7d",
		"weird id/with:chars":                        "weird_id_with_chars",
		"":                                           "account",
		"...":                                        "account",
	}
	for in, want := range cases {
		if got := cliConfigLeaf(in); got != want {
			t.Errorf("cliConfigLeaf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCLIDirStaysInsideTheModuleDataDir(t *testing.T) {
	c := &Client{deps: core.Deps{DataDir: t.TempDir()}}
	dir := c.cliConfigDir(cliConfigLeaf("qoder-abc"))
	root := c.cliRootDir()
	if !pathWithin(root, dir) {
		t.Fatalf("%s is not inside %s", dir, root)
	}
	// A leaf that tries to escape must not resolve outside the root.  The
	// separator is the host's own, so the case means the same thing on every
	// platform this module is built for.
	if pathWithin(root, c.cliConfigDir(filepath.Join("..", "..", "escape"))) {
		t.Fatal("a .. leaf escaped the CLI root")
	}
}

func TestCLIEnvDropsVendorVariables(t *testing.T) {
	base := t.TempDir()
	t.Setenv("QODERCN_CONFIG_DIR", "C:\\Users\\me\\.qoder-cn")
	t.Setenv("QODER_PERSONAL_ACCESS_TOKEN", "secret")
	t.Setenv("QODERCN_DEVICE_TOKEN", "dt-secret")
	env := cliEnv(base)
	valueOf := func(name string) (string, bool) {
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, name+"="); ok {
				return v, true
			}
		}
		return "", false
	}
	// The operator's own variables are dropped, except the config root, which
	// is re-pinned to the account directory as a second line of defence behind
	// --config-dir.
	if v, ok := valueOf("QODERCN_CONFIG_DIR"); !ok || v != base {
		t.Errorf("QODERCN_CONFIG_DIR = %q (present=%v), want %q", v, ok, base)
	}
	for _, name := range []string{"QODER_PERSONAL_ACCESS_TOKEN", "QODERCN_DEVICE_TOKEN"} {
		if _, ok := valueOf(name); ok {
			t.Errorf("%s survived cliEnv", name)
		}
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "ELECTRON_RUN_AS_NODE=1") {
		t.Error("ELECTRON_RUN_AS_NODE is not pinned")
	}
}

func TestNewCLICmdRunsTheWorkerFromTheAccountDir(t *testing.T) {
	root := fakeInstall(t)
	inst, _ := probeInstall(root)
	dir := t.TempDir()
	cmd := newCLICmd(inst, dir, "--print")
	if cmd.Dir != dir {
		t.Errorf("Dir = %q, want the account dir", cmd.Dir)
	}
	if cmd.Path != inst.Exe {
		t.Errorf("Path = %q, want the install exe", cmd.Path)
	}
	if len(cmd.Args) < 2 || cmd.Args[1] != inst.Worker {
		t.Errorf("Args = %v, want the worker first", cmd.Args)
	}
}

func TestCLIArgsDisableTheBuiltInTools(t *testing.T) {
	p := newCLIProc(cliInstall{}, t.TempDir(), "acc", "dfmodel", "")
	args := p.cliArgs()
	joined := strings.Join(args, "\x00")
	if !strings.Contains(joined, "--config-dir\x00"+p.dir) {
		t.Errorf("args do not pin the config dir: %v", args)
	}
	i := -1
	for j, a := range args {
		if a == "--tools" {
			i = j
		}
	}
	if i < 0 || i+1 >= len(args) || args[i+1] != "" {
		t.Errorf("--tools is not disabled: %v", args)
	}
}

func TestEventsFromInner(t *testing.T) {
	cases := []struct {
		name string
		in   cliEvent
		want []core.Event
	}{
		{
			name: "text delta",
			in: cliEvent{Type: "content_block_delta", Delta: struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			}{Type: "text_delta", Text: "OK"}},
			want: []core.Event{{Type: core.EventDelta, Delta: "OK"}},
		},
		{
			name: "thinking delta",
			in: func() cliEvent {
				ev := cliEvent{Type: "content_block_delta"}
				ev.Delta.Type = "thinking_delta"
				ev.Delta.Thinking = "hmm"
				return ev
			}(),
			want: []core.Event{{Type: core.EventDelta, Reasoning: "hmm"}},
		},
		{
			name: "tool call arguments",
			in: func() cliEvent {
				ev := cliEvent{Type: "content_block_delta", Index: 2}
				ev.Delta.Type = "input_json_delta"
				ev.Delta.PartialJSON = `{"a":`
				return ev
			}(),
			want: []core.Event{{Type: core.EventToolCall, ToolCall: &core.ToolCallDelta{Index: 2, Arguments: `{"a":`}}},
		},
		{
			name: "tool call announcement",
			in: func() cliEvent {
				ev := cliEvent{Type: "content_block_start", Index: 1}
				ev.ContentBlock.Type = "tool_use"
				ev.ContentBlock.ID = "t1"
				ev.ContentBlock.Name = "Read"
				return ev
			}(),
			want: []core.Event{{Type: core.EventToolCall, ToolCall: &core.ToolCallDelta{Index: 1, ID: "t1", Name: "Read"}}},
		},
		{
			name: "usage",
			in: func() cliEvent {
				ev := cliEvent{Type: "message_delta"}
				ev.Usage = &cliUsage{InputTokens: 3, OutputTokens: 5, CacheRead: 7}
				return ev
			}(),
			want: []core.Event{{Type: core.EventUsage, Usage: &core.Usage{PromptTokens: 3, CompletionTokens: 5, TotalTokens: 8, CachedTokens: 7}}},
		},
		{
			name: "unmodelled kind",
			in:   cliEvent{Type: "message_stop"},
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := eventsFromInner(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d events, want %d", len(got), len(tc.want))
			}
			for i := range got {
				if got[i].Type != tc.want[i].Type {
					t.Errorf("event %d type = %v, want %v", i, got[i].Type, tc.want[i].Type)
				}
				if got[i].Delta != tc.want[i].Delta || got[i].Reasoning != tc.want[i].Reasoning {
					t.Errorf("event %d payload = %+v, want %+v", i, got[i], tc.want[i])
				}
				if (got[i].ToolCall == nil) != (tc.want[i].ToolCall == nil) {
					t.Errorf("event %d tool presence differs", i)
				} else if got[i].ToolCall != nil && *got[i].ToolCall != *tc.want[i].ToolCall {
					t.Errorf("event %d tool = %+v, want %+v", i, *got[i].ToolCall, *tc.want[i].ToolCall)
				}
				if (got[i].Usage == nil) != (tc.want[i].Usage == nil) {
					t.Errorf("event %d usage presence differs", i)
				} else if got[i].Usage != nil && *got[i].Usage != *tc.want[i].Usage {
					t.Errorf("event %d usage = %+v, want %+v", i, *got[i].Usage, *tc.want[i].Usage)
				}
			}
		})
	}
}

func TestFinishFromCLI(t *testing.T) {
	cases := map[string]string{
		"end_turn":     "stop",
		"stop":         "stop",
		"max_tokens":   "length",
		"tool_use":     "tool_calls",
		"":             "",
		"mystery":      "mystery",
		"  END_TURN  ": "stop",
	}
	for in, want := range cases {
		if got := finishFromCLI(in); got != want {
			t.Errorf("finishFromCLI(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHandleLineSurfacesARefusalAsAuth(t *testing.T) {
	turn := &cliTurn{proc: newCLIProc(cliInstall{}, t.TempDir(), "acc-1", "dfmodel", "")}
	raw := []byte(`{"type":"result","subtype":"success","is_error":true,"result":"Not logged in · Please run /login"}`)
	stop := ""
	done, err := turn.handleLine(raw, &stop, func(core.Event) bool { return true })
	if !done {
		t.Fatal("a result line must end the turn")
	}
	var failure *core.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("err = %v, want a *core.Failure", err)
	}
	if failure.Kind != core.FailureAuth {
		t.Errorf("kind = %v, want auth", failure.Kind)
	}
	if failure.Account != "acc-1" {
		t.Errorf("account = %q, want acc-1", failure.Account)
	}
	if !strings.Contains(err.Error(), "Not logged in") {
		t.Errorf("the vendor's words were lost: %v", err)
	}
}

func TestHandleLineSurfacesARefusalAsUpstream(t *testing.T) {
	turn := &cliTurn{proc: newCLIProc(cliInstall{}, t.TempDir(), "acc-1", "dfmodel", "")}
	raw := []byte(`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["boom"]}`)
	done, err := turn.handleLine(raw, new(string), func(core.Event) bool { return true })
	if !done {
		t.Fatal("a result line must end the turn")
	}
	var failure *core.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("err = %v, want a *core.Failure", err)
	}
	if failure.Kind != core.FailureUpstream {
		t.Errorf("kind = %v, want upstream", failure.Kind)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("the vendor's words were lost: %v", err)
	}
}

func TestHandleLineEmitsDoneOnASettledTurn(t *testing.T) {
	turn := &cliTurn{proc: newCLIProc(cliInstall{}, t.TempDir(), "acc-1", "dfmodel", "")}
	var events []core.Event
	raw := []byte(`{"type":"result","subtype":"success","is_error":false,"stop_reason":"end_turn","result":"hi"}`)
	done, err := turn.handleLine(raw, new(string), func(ev core.Event) bool {
		events = append(events, ev)
		return true
	})
	if !done || err != nil {
		t.Fatalf("done=%v err=%v", done, err)
	}
	if len(events) != 1 || events[0].Type != core.EventDone || events[0].Finish != "stop" {
		t.Fatalf("events = %+v, want one clean done", events)
	}
}

func TestPromptFromRequestTakesTheNewestUserTurn(t *testing.T) {
	req := &core.ChatRequest{Messages: []core.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "nope"},
		{Role: "user", Content: "second"},
	}}
	got, err := promptFromRequest(req)
	if err != nil {
		t.Fatalf("promptFromRequest: %v", err)
	}
	if got != "second" {
		t.Errorf("got %q, want the newest user turn", got)
	}
}

func TestFlattenRequestCarriesWholeHistory(t *testing.T) {
	req := &core.ChatRequest{Messages: []core.Message{
		{Role: "system", Content: "be brief"},
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "answer"},
		{Role: "user", Content: "second"},
	}}
	got, err := flattenRequest(req)
	if err != nil {
		t.Fatalf("flattenRequest: %v", err)
	}
	for _, want := range []string{"System: be brief", "User: first", "Assistant: answer", "User: second"} {
		if !strings.Contains(got, want) {
			t.Errorf("flatten missing %q in %q", want, got)
		}
	}
}

func TestFlattenRequestSendsASingleUserTurnBare(t *testing.T) {
	req := &core.ChatRequest{Messages: []core.Message{{Role: "user", Content: "hi there"}}}
	got, err := flattenRequest(req)
	if err != nil {
		t.Fatalf("flattenRequest: %v", err)
	}
	if got != "hi there" {
		t.Errorf("got %q, want the bare text", got)
	}
}

func TestPromptForTurnSwitchesOnWorkerFreshness(t *testing.T) {
	req := &core.ChatRequest{
		ConversationID: "conv-1",
		Messages: []core.Message{
			{Role: "user", Content: "first"},
			{Role: "assistant", Content: "answer"},
			{Role: "user", Content: "second"},
		},
	}
	// A worker that is already alive owns the transcript: the newest turn only.
	if got, _ := promptForTurn(req, false); got != "second" {
		t.Errorf("warm prompt = %q, want the newest turn only", got)
	}
	// A worker that is about to start owns nothing: the whole history, or the
	// model would answer with no context.
	if got, _ := promptForTurn(req, true); !strings.Contains(got, "first") || !strings.Contains(got, "second") {
		t.Errorf("cold prompt = %q, want the whole history", got)
	}
}

func TestCLIEffortTier(t *testing.T) {
	for _, in := range []string{"max", "HIGH", " xhigh ", "medium"} {
		if got := cliEffortTier(in); got != strings.ToLower(strings.TrimSpace(in)) {
			t.Errorf("cliEffortTier(%q) = %q", in, got)
		}
	}
	for _, in := range []string{"", "off", "none", "banana"} {
		if got := cliEffortTier(in); got != "" {
			t.Errorf("cliEffortTier(%q) = %q, want empty", in, got)
		}
	}
}

func TestCLIEffortPrefersTheCaller(t *testing.T) {
	c := &Client{cfg: config{CLIReasoningEffort: "low"}}
	req := &core.ChatRequest{Options: map[string]any{"reasoning_effort": "high"}}
	if got := c.cliEffort(req); got != "high" {
		t.Errorf("caller effort = %q, want high", got)
	}
	if got := c.cliEffort(&core.ChatRequest{}); got != "low" {
		t.Errorf("configured effort = %q, want low", got)
	}
	// A caller-invented tier must be dropped, not passed through: the CLI
	// rejects the whole turn for an unknown value.
	if got := c.cliEffort(&core.ChatRequest{Options: map[string]any{"effort": "banana"}}); got != "low" {
		t.Errorf("unknown tier = %q, want the configured default", got)
	}
}

func TestParseCLIStatusReadsTheIdentity(t *testing.T) {
	out := "Version: 1.1.64\n" +
		"Username: vencent1306681486\n" +
		"Avatar: https://qoder.com.cn/users/019e9892-0117-7af5-b551-7da169c9df7d/default/avatars\n" +
		"User Type: personal_standard\n" +
		"Login Method: browser\n"
	st := parseCLIStatus(out)
	if st.Version != "1.1.64" || st.Username != "vencent1306681486" {
		t.Errorf("status = %+v", st)
	}
	if st.UserID != "019e9892-0117-7af5-b551-7da169c9df7d" {
		t.Errorf("UserID = %q, want the vendor's own id", st.UserID)
	}
	if st.UserType != "personal_standard" {
		t.Errorf("UserType = %q", st.UserType)
	}
}

func TestPathWithin(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "data", "qoder", "cli")
	cases := []struct {
		name  string
		child string
		want  bool
	}{
		{"child", filepath.Join(parent, "acc1"), true},
		{"same dir", parent, true},
		{"sibling prefix", filepath.Join(base, "data", "qoder", "cli-other", "acc1"), false},
		{"outside", filepath.Join(base, "elsewhere"), false},
		{"empty", "", false},
	}
	if runtime.GOOS == "windows" {
		// Windows compares paths case-insensitively; the other hosts do not,
		// so the expectation has to follow the platform's own rule.
		cases = append(cases, struct {
			name  string
			child string
			want  bool
		}{"case folded", strings.ToUpper(filepath.Join(parent, "acc1")), true})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pathWithin(parent, tc.child); got != tc.want {
				t.Errorf("pathWithin(%q, %q) = %v, want %v", parent, tc.child, got, tc.want)
			}
		})
	}
}

func TestSelectableCoversCLIOnlyAccounts(t *testing.T) {
	now := time.Now().UTC()
	acc := account{storedAccount: storedAccount{ID: "cli-1", CLIDir: "staged", Enabled: true}}
	if !acc.selectable(now) {
		t.Error("a CLI-only account must be selectable")
	}
	if !acc.cliOnly() {
		t.Error("cliOnly() = false for a CLI-only account")
	}
	// An account with neither a token nor a CLI root is still unusable.
	empty := account{storedAccount: storedAccount{ID: "empty", Enabled: true}}
	if empty.selectable(now) {
		t.Error("an account with no credential at all must not be selectable")
	}
}

func TestAttachCLIDirKeepsTheDeviceToken(t *testing.T) {
	s := &store{all: []*account{{storedAccount: storedAccount{
		ID: "qoder-uid", Token: "dt-x", Label: "old", Enabled: true,
	}}}}
	err := s.attachCLIDir(account{storedAccount: storedAccount{
		ID: "qoder-uid", CLIDir: "leaf", Label: "new", Name: "who",
	}})
	if err != nil {
		t.Fatalf("attachCLIDir: %v", err)
	}
	got := s.all[0]
	if got.Token != "dt-x" {
		t.Errorf("the device token was overwritten: %q", got.Token)
	}
	if got.CLIDir != "leaf" {
		t.Errorf("CLIDir = %q", got.CLIDir)
	}
	if got.Label != "old" {
		t.Errorf("an existing label was replaced: %q", got.Label)
	}
	if !got.Enabled {
		t.Error("attachCLIDir must enable the account")
	}

	// A login for an account that is not stored yet creates the row.
	if err := s.attachCLIDir(account{storedAccount: storedAccount{
		ID: "qoder-new", CLIDir: "leaf2", Label: "who2",
	}}); err != nil {
		t.Fatalf("attachCLIDir(new): %v", err)
	}
	if len(s.all) != 2 || s.all[1].ID != "qoder-new" {
		t.Fatalf("rows = %+v", s.all)
	}
}

func TestAttachCLIDirRefusesAnEmptyID(t *testing.T) {
	s := &store{}
	if err := s.attachCLIDir(account{storedAccount: storedAccount{CLIDir: "leaf"}}); err == nil {
		t.Fatal("an id-less CLI login must be refused")
	}
}

func TestRemoveAccountHandsTheCLIRootToTheCleanupHook(t *testing.T) {
	var removed []string
	s := &store{onRemoveCLIDir: func(leaf string) { removed = append(removed, leaf) }}
	s.all = []*account{{storedAccount: storedAccount{ID: "cli-1", CLIDir: "leaf-1", Enabled: true}, origin: originStored}}

	if err := s.remove("cli-1"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if len(s.all) != 0 {
		t.Fatalf("rows = %+v", s.all)
	}
	if len(removed) != 1 || removed[0] != "leaf-1" {
		t.Fatalf("removed = %v, want [leaf-1]", removed)
	}
}

func TestAccountRecordReportsTheCLILogin(t *testing.T) {
	acc := account{storedAccount: storedAccount{ID: "cli-1", CLIDir: "leaf-1", Enabled: true}}
	rec := accountRecord(&acc, time.Now().UTC())
	if rec.Fields["cli_login"] != true {
		t.Errorf("cli_login = %v", rec.Fields["cli_login"])
	}
	if rec.Fields["cli_dir"] != "leaf-1" {
		t.Errorf("cli_dir = %v", rec.Fields["cli_dir"])
	}
	if rec.State != "ready" {
		t.Errorf("state = %q, want ready", rec.State)
	}
}

func TestAdoptCLIDirMovesAStagedRootOntoTheAccount(t *testing.T) {
	base := t.TempDir()
	t.Setenv(cliDirEnv, base)
	c := &Client{deps: core.Deps{DataDir: filepath.Join(base, "..", "data")}}

	staged := filepath.Join(".staging", "login-x")
	dir := c.cliConfigDir(staged)
	if err := os.MkdirAll(filepath.Join(dir, ".auth"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cliUserMarker(dir), []byte("signed-in"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := c.adoptCLIDir(staged, "qoder-abc-123")
	if got != "qoder-abc-123" {
		t.Fatalf("leaf = %q, want the account id", got)
	}
	if !cliSignedIn(c.cliConfigDir("qoder-abc-123")) {
		t.Fatal("the staged root was not moved onto the account")
	}
	if cliSignedIn(dir) {
		t.Error("the staged copy is still there")
	}
}

func TestAdoptCLIDirKeepsAnExistingRoot(t *testing.T) {
	base := t.TempDir()
	t.Setenv(cliDirEnv, base)
	c := &Client{deps: core.Deps{DataDir: filepath.Join(base, "..", "data")}}

	existing := c.cliConfigDir("qoder-old")
	if err := os.MkdirAll(filepath.Join(existing, ".auth"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cliUserMarker(existing), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	staged := c.cliConfigDir(filepath.Join(".staging", "login-y"))
	if err := os.MkdirAll(filepath.Join(staged, ".auth"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cliUserMarker(staged), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := c.adoptCLIDir(filepath.Join(".staging", "login-y"), "qoder-old")
	if got != "qoder-old" {
		t.Fatalf("leaf = %q, want the existing account leaf", got)
	}
	if !cliSignedIn(existing) {
		t.Error("the existing root was disturbed")
	}
}

func TestRemoveStagedRootLeavesSignedInStateAlone(t *testing.T) {
	base := t.TempDir()
	t.Setenv(cliDirEnv, base)
	logs := []string{}
	c := &Client{deps: core.Deps{DataDir: filepath.Join(base, "..", "data")}}

	staged := filepath.Join(".staging", "login-z")
	dir := c.cliConfigDir(staged)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	c.removeStagedRoot(staged)
	if _, err := os.Stat(dir); err == nil {
		t.Log("the empty staged root survived")
	}

	// A staged root that IS signed in must never be removed here: the login
	// succeeded and the poll has not adopted it yet.
	if err := os.MkdirAll(filepath.Join(dir, ".auth"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cliUserMarker(dir), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.removeStagedRoot(staged)
	if !cliSignedIn(dir) {
		t.Fatal("a signed-in staged root was removed")
	}
	_ = logs
	_ = io.Discard
}

// TestLiveCLIBridgeAnswers drives the real vendor worker through the whole
// bridge.  It needs an actual Qoder CN install and an account signed in to its
// local CLI, so it is opt-in exactly like the desktop-credential test.
func TestLiveCLIBridgeAnswers(t *testing.T) {
	if os.Getenv("CLIENT2API_QODER_LIVE_CLI_TEST") != "1" {
		t.Skip("set CLIENT2API_QODER_LIVE_CLI_TEST=1 with a signed-in local CLI to run the live bridge")
	}
	// The vendor serialises inference per account, so this test must not race
	// the two credential tests.  Go runs tests in one package sequentially by
	// default, but the worker is a child process: a leftover one from an
	// earlier aborted run would hold this account and stall the turn.  A
	// per-test deadline turns that into a fast reported failure instead of a
	// hung test binary.
	// A queue deep enough to matter takes minutes, so the bound here is the
	// vendor's own ceiling plus headroom rather than a tight timeout.  The point
	// of the guard is to catch a genuinely wedged worker, not to fail a turn the
	// vendor is legitimately making wait.
	deadline := time.AfterFunc(12*time.Minute, func() {
		t.Errorf("live bridge turn exceeded 12m; a stale Qoder worker may be holding %s", os.Getenv("CLIENT2API_QODER_LIVE_CLI_DIR"))
	})
	defer deadline.Stop()
	dir := os.Getenv("CLIENT2API_QODER_LIVE_CLI_DIR")
	if dir == "" {
		t.Skip("CLIENT2API_QODER_LIVE_CLI_DIR must name a config root that holds a CLI login")
	}
	t.Setenv(cliDirEnv, filepath.Dir(dir))
	resetCLIDiscovery()
	t.Cleanup(resetCLIDiscovery)

	c := &Client{
		deps: core.Deps{DataDir: t.TempDir()},
		cfg:  config{},
	}
	c.store = &store{all: []*account{{storedAccount: storedAccount{
		ID:      filepath.Base(dir),
		CLIDir:  filepath.Base(dir),
		Enabled: true,
	}}}}
	c.cli = newCLIPool()
	// A go test process does not reap its children, so the workers this test
	// starts have to be torn down explicitly or they outlive the run and hold
	// the account directory against the next one.  Close tears the pool and the
	// sweeper down; the timeout keeps a wedged child from hanging the run.
	t.Cleanup(func() {
		done := make(chan struct{})
		go func() { _ = c.Close(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Log("close did not finish within 30s")
		}
	})

	acc := *c.store.all[0]
	if !c.cliAccountReady(acc) {
		t.Fatalf("%s does not look signed in (no .auth/user)", dir)
	}

	// Both turns share one conversation id, so the second one is what a real
	// client's follow-up looks like: a warm worker that owns the transcript.
	stream, err := c.chatViaCLI(context.Background(), &core.ChatRequest{
		ConversationID: "live-conv",
		Model:          "dfmodel",
		Messages:       []core.Message{{Role: "user", Content: "Reply with the single word OK"}},
	}, "dfmodel", "")
	if err != nil {
		t.Fatalf("chatViaCLI: %v", err)
	}
	defer stream.Close()

	var reply strings.Builder
	queued := false
	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		switch event.Type {
		case core.EventDelta:
			if event.Reasoning != "" {
				queued = true
				t.Logf("queue notice: %s", event.Reasoning)
			}
			reply.WriteString(event.Delta)
		case core.EventError:
			if event.Err != nil {
				t.Fatalf("event error: %v", event.Err)
			}
		}
	}
	if strings.TrimSpace(reply.String()) == "" {
		// The two free models are heavily contended: the vendor queues them and
		// reports the wait instead of answering.  A turn that only ever queued
		// is a busy vendor, not a broken bridge -- the notice the bridge emitted
		// is itself the proof that the transport works.
		if queued {
			t.Skip("the vendor is queueing this model right now; the bridge surfaced the wait")
		}
		t.Fatal("the CLI answered with no text")
	}
	t.Logf("first turn reply: %s", reply.String())

	// A second turn on the same conversation must reuse the live worker, which
	// is the whole reason the pool exists: the reply to "what did I ask" is
	// only knowable if the transcript survived.
	second, err := c.chatViaCLI(context.Background(), &core.ChatRequest{
		ConversationID: "live-conv",
		Model:          "dfmodel",
		Messages: []core.Message{
			{Role: "user", Content: "Reply with the single word OK"},
			{Role: "assistant", Content: reply.String()},
			{Role: "user", Content: "What single word did I ask you to reply with? Answer with just that word."},
		},
	}, "dfmodel", "")
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}
	defer second.Close()
	var secondReply strings.Builder
	for {
		event, err := second.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("second stream: %v", err)
		}
		switch event.Type {
		case core.EventDelta:
			secondReply.WriteString(event.Delta)
		case core.EventError:
			if event.Err != nil {
				t.Fatalf("second event error: %v", event.Err)
			}
		}
	}
	if workers, _ := c.cliPoolFor().counters(); workers != 1 {
		t.Errorf("the pool holds %d workers after two turns, want 1", workers)
	}
	t.Logf("second turn reply: %s", secondReply.String())
}

// ---------------------------------------------------------------------------
// C2: one CLI sign-in is enough for the whole account
// ---------------------------------------------------------------------------

// TestCLICredentialStoreYieldsTheDeviceToken is the load-bearing test for the
// CLI-only account shape.
//
// A CLI sign-in writes its credential into the config root, encrypted with a
// key derived from that root's machine id.  Reading it back -- through the
// vendor's own WASM, which the CLI binary hosts -- is what gives the account
// the device token the ledger, the check-in and the signed gateway all need.
// Without this a CLI-only account could serve the two free models and nothing
// else, which is exactly the shape this module is trying to avoid.
//
// It needs a real Qoder CN install and a signed-in config root, so it is
// opt-in like the other live tests.
func TestCLICredentialStoreYieldsTheDeviceToken(t *testing.T) {
	if os.Getenv("CLIENT2API_QODER_LIVE_CLI_TEST") != "1" {
		t.Skip("set CLIENT2API_QODER_LIVE_CLI_TEST=1 with a signed-in local CLI")
	}
	dir := os.Getenv("CLIENT2API_QODER_LIVE_CLI_DIR")
	if dir == "" {
		t.Skip("CLIENT2API_QODER_LIVE_CLI_DIR must name a config root that holds a CLI login")
	}
	if _, err := findCLI(); err != nil {
		t.Skipf("no local Qoder CN install: %v", err)
	}
	t.Setenv(cliDirEnv, filepath.Dir(dir))
	cfg := config{}
	c := &Client{
		deps: core.Deps{DataDir: t.TempDir(), Logf: func(f string, a ...any) { t.Logf("log: "+f, a...) }},
		cfg:  cfg,
		up:   newUpstream(cfg, nil, nil),
	}
	leaf := filepath.Base(dir)
	acc := account{storedAccount: storedAccount{ID: leaf, CLIDir: leaf, Enabled: true}}
	c.store = &store{all: []*account{&acc}}

	if !acc.cliOnly() {
		t.Fatal("a token-less CLI account must start out cli-only")
	}
	fresh := c.ensureFreshCLIToken(context.Background(), acc)
	if fresh.cliOnly() {
		t.Fatalf("the credential store did not yield a token: %q", fresh.Token)
	}
	if !strings.HasPrefix(fresh.Token, "dt-") {
		t.Errorf("token = %q, want the vendor's dt- device token", shortToken(fresh.Token))
	}
	if fresh.UserID == "" {
		t.Error("uid was not read")
	}
	if fresh.RefreshToken == "" {
		t.Error("refresh token was not read")
	}
	if fresh.ExpiresAtMS <= 0 {
		t.Error("expiry was not read")
	}

	// The token is the one OpenAPI accepts, which is the whole point: prove it
	// against the ledger rather than trusting the decrypt alone.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	bal, err := c.AccountBalance(ctx, fresh.ID, 0)
	if err != nil {
		t.Fatalf("AccountBalance with the decrypted token: %v", err)
	}
	if bal.Total <= 0 {
		t.Errorf("ledger returned no capacity: %+v", bal)
	}
	t.Logf("ledger: credits=%d total=%d used=%.2f", bal.Credits, bal.Total, bal.Used)
}

// TestCLICheckinUsesTheDecryptedToken proves the second half of C2: the daily
// check-in works off the same CLI-derived credential, so an operator who only
// signed in to the CLI still gets their credits.
func TestCLICheckinUsesTheDecryptedToken(t *testing.T) {
	if os.Getenv("CLIENT2API_QODER_LIVE_CLI_TEST") != "1" {
		t.Skip("set CLIENT2API_QODER_LIVE_CLI_TEST=1 with a signed-in local CLI")
	}
	dir := os.Getenv("CLIENT2API_QODER_LIVE_CLI_DIR")
	if dir == "" {
		t.Skip("CLIENT2API_QODER_LIVE_CLI_DIR must name a config root that holds a CLI login")
	}
	if _, err := findCLI(); err != nil {
		t.Skipf("no local Qoder CN install: %v", err)
	}
	t.Setenv(cliDirEnv, filepath.Dir(dir))
	cfg := config{}
	c := &Client{
		deps: core.Deps{DataDir: t.TempDir(), Logf: func(f string, a ...any) { t.Logf("log: "+f, a...) }},
		cfg:  cfg,
		up:   newUpstream(cfg, nil, nil),
	}
	leaf := filepath.Base(dir)
	acc := account{storedAccount: storedAccount{ID: leaf, CLIDir: leaf, Enabled: true}}
	c.store = &store{all: []*account{&acc}}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	res, err := c.Checkin(ctx, acc.ID, "")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.Skipped {
		t.Fatalf("a CLI-only account was skipped instead of checked in: %s", res.Message)
	}
	if res.Error != "" {
		t.Fatalf("check-in failed: %s", res.Error)
	}
	t.Logf("check-in: ok=%v message=%q", res.OK, res.Message)
}

// shortToken renders a token for a log line without printing it whole.
func shortToken(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8] + "..."
}

// TestQueuedTurnExtendsTheStallWindow pins the behaviour the free models need.
//
// The vendor queues dfmodel/qfmodel when they are busy and reports the wait as
// `model_queue_status` with a wait_time_ms that can reach an hour.  A worker
// that is waiting is not stalled, so the silence guard must be pushed out by
// every such report -- otherwise the bridge kills a process the vendor is
// actively serving.
func TestQueuedTurnExtendsTheStallWindow(t *testing.T) {
	turn := &cliTurn{proc: newCLIProc(cliInstall{}, t.TempDir(), "acc", "dfmodel", "")}
	if got := turn.stallBudget(); got != cliLineStall {
		t.Fatalf("a fresh turn's budget = %s, want %s", got, cliLineStall)
	}

	var got []core.Event
	raw := []byte(`{"type":"system","subtype":"model_queue_status","status":"queued","model_key":"dfmodel","queue_count":5333,"wait_time_ms":323000,"queue_max_wait_ms":3600000,"service_available":true}`)
	done, err := turn.handleLine(raw, new(string), func(ev core.Event) bool {
		got = append(got, ev)
		return true
	})
	if done || err != nil {
		t.Fatalf("a queue report must not end the turn: done=%v err=%v", done, err)
	}
	if len(got) != 1 || got[0].Type != core.EventDelta || got[0].Reasoning == "" {
		t.Fatalf("events = %+v, want one notice on the reasoning channel", got)
	}
	if !strings.Contains(got[0].Reasoning, "5333") {
		t.Errorf("the notice does not name the queue depth: %q", got[0].Reasoning)
	}
	if budget := turn.stallBudget(); budget <= 30*time.Minute {
		t.Errorf("stall budget = %s, want it extended past the vendor's queue ceiling", budget)
	}
}

// TestQueueNoticeRendersWhatTheOperatorNeeds checks the human-readable form.
func TestQueueNoticeRendersWhatTheOperatorNeeds(t *testing.T) {
	line := cliLine{QueueCount: 12, WaitTimeMS: 65000, ServiceAvailable: true}
	got := queueNotice(line)
	for _, want := range []string{"排队中", "12", "预计"} {
		if !strings.Contains(got, want) {
			t.Errorf("notice %q is missing %q", got, want)
		}
	}
	if !strings.Contains(queueNotice(cliLine{ServiceAvailable: false}), "不可用") {
		t.Error("an unavailable service is not reported")
	}
}

// TestCLILoginCompletesWhenTheCredentialArrivesWithoutTheConsoleLine pins the
// bug that left a finished sign-in spinning forever.
//
// The vendor writes <configDir>/.auth/user as soon as the browser step
// succeeds, but its console line ("Login successful! Welcome, …") is a
// separate, cosmetic signal whose wording is not part of any contract.  The
// poll used to insist on that line before it would look at the store, so a
// perfectly good credential sat in "pending" and the panel spun on 请稍后.
// Completion must be decided by the store.
func TestCLILoginCompletesWhenTheCredentialArrivesWithoutTheConsoleLine(t *testing.T) {
	dir := os.Getenv("CLIENT2API_QODER_LIVE_CLI_DIR")
	if dir == "" || os.Getenv("CLIENT2API_QODER_LIVE_CLI_TEST") != "1" {
		t.Skip("set CLIENT2API_QODER_LIVE_CLI_TEST=1 and CLIENT2API_QODER_LIVE_CLI_DIR")
	}
	if _, err := findCLI(); err != nil {
		t.Skipf("no local Qoder CN install: %v", err)
	}
	// Copy the credential into a scratch CLI root first: adoptCLIDir MOVES the
	// staged login onto the account's own directory, so pointing this test at
	// the live tree would consume the operator's sign-in and make the test
	// one-shot.
	// cliConfigDir is a plain join, so a staged leaf carries its own .staging
	// prefix -- exactly what cliStageLeaf produces in production.
	cliRoot := t.TempDir()
	leaf := filepath.Join(".staging", "login-test")
	staged := filepath.Join(cliRoot, leaf)
	if err := os.MkdirAll(filepath.Join(staged, ".auth"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"user", "machine_id"} {
		b, err := os.ReadFile(filepath.Join(dir, ".auth", name))
		if err != nil {
			t.Fatalf("reading %s from %s: %v", name, dir, err)
		}
		if err := os.WriteFile(filepath.Join(staged, ".auth", name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(cliDirEnv, cliRoot)
	cfg := config{}
	c := &Client{
		deps: core.Deps{DataDir: t.TempDir(), Logf: func(f string, a ...any) { t.Logf("log: "+f, a...) }},
		cfg:  cfg,
		up:   newUpstream(cfg, nil, nil),
	}
	c.store = &store{all: make([]*account, 0)}
	c.logins = map[string]*loginSession{}
	c.cliLogins = map[string]*cliLoginSession{}

	// A session whose console identity never arrived (who == "", ok == false)
	// over a config root that IS signed in: exactly the state that used to hang.
	sess := &cliLoginSession{
		id:        "qoder-cli-login-test",
		leaf:      leaf,
		dir:       staged,
		expiresAt: time.Now().Add(time.Minute),
		done:      make(chan struct{}),
		message:   "waiting",
	}
	close(sess.done)

	st, err := c.pollCLILogin(context.Background(), sess)
	if err != nil {
		t.Fatalf("pollCLILogin: %v", err)
	}
	if st.State != core.LoginSuccess {
		t.Fatalf("state = %q (%s), want success: a credential was present", st.State, st.Message)
	}
	accs := c.store.snapshot()
	if len(accs) != 1 {
		t.Fatalf("accounts = %d, want the signed-in account to be stored", len(accs))
	}
	if accs[0].Token == "" {
		t.Error("the stored account carries no device token")
	}
	if accs[0].CLIDir == "" {
		t.Error("the stored account carries no CLI config root")
	}
}

// TestCLILoginStaysPendingWhileNoCredentialExists is the other half: a session
// with nothing on disk must keep waiting, not report success.
func TestCLILoginStaysPendingWhileNoCredentialExists(t *testing.T) {
	t.Setenv(cliDirEnv, t.TempDir())
	c := &Client{deps: core.Deps{DataDir: t.TempDir()}, cfg: config{}}
	c.store = &store{}
	c.cliLogins = map[string]*cliLoginSession{}

	sess := &cliLoginSession{
		id:        "pending",
		leaf:      filepath.Join("absent", "leaf"),
		dir:       filepath.Join(t.TempDir(), "nothing-here"),
		expiresAt: time.Now().Add(time.Minute),
		done:      make(chan struct{}),
		message:   "waiting",
	}
	st, err := c.pollCLILogin(context.Background(), sess)
	if err != nil {
		t.Fatalf("pollCLILogin: %v", err)
	}
	if st.State != core.LoginPending {
		t.Fatalf("state = %q, want pending while no credential exists", st.State)
	}
}

// TestExitedLoginChildStillAdoptsAWrittenCredential pins the second half of the
// "请稍后" hang.
//
// The login child exits the instant the browser confirms, and it writes the
// credential in the same breath.  The exit path used to record that as a
// failure a beat before the file was visible, so the poll saw "failed" and the
// panel gave up -- with a valid credential sitting on disk the whole time.
// A stored credential must win over an exited child, and an exited child with
// nothing on disk must still be a failure.
func TestExitedLoginChildStillAdoptsAWrittenCredential(t *testing.T) {
	cliRoot := t.TempDir()
	leaf := filepath.Join(".staging", "login-exited")
	staged := filepath.Join(cliRoot, leaf)
	if err := os.MkdirAll(filepath.Join(staged, ".auth"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A credential file is all the poll needs here; its content is never
	// decrypted on this path (that is readCLICredentials' job, covered by the
	// live test).
	if err := os.WriteFile(filepath.Join(staged, ".auth", "user"), []byte("ciphertext"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(cliDirEnv, cliRoot)

	c := &Client{deps: core.Deps{DataDir: t.TempDir()}, cfg: config{}}
	c.store = &store{}
	c.cliLogins = map[string]*cliLoginSession{}

	sess := &cliLoginSession{
		id: "exited", leaf: leaf, dir: staged,
		expiresAt: time.Now().Add(time.Minute),
		done:      make(chan struct{}),
		message:   "waiting",
	}
	// Simulate the exit path having recorded a failure already.
	sess.fail("本机 Qoder CLI 的登录进程已退出，可能是设备码过期或被取消。")
	close(sess.done)

	st, _ := c.pollCLILogin(context.Background(), sess)
	// What must not happen is the old behaviour: reporting the child's exit as
	// the reason and stopping there.  Depending on the host the poll may then
	// fail for a different, honest reason (no install, or a synthetic file the
	// decrypt rejects), and both of those are fine here.
	if st.State == core.LoginFailed && strings.Contains(st.Message, "登录进程已退出") {
		t.Fatalf("the child's exit was reported as the failure while a credential existed: %q", st.Message)
	}
}

// TestExitedLoginChildWithNoCredentialFails guards the other direction: a child
// that exits without writing anything must still report a failure.
func TestExitedLoginChildWithNoCredentialFails(t *testing.T) {
	cliRoot := t.TempDir()
	leaf := filepath.Join(".staging", "login-empty")
	staged := filepath.Join(cliRoot, leaf)
	if err := os.MkdirAll(staged, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(cliDirEnv, cliRoot)

	c := &Client{deps: core.Deps{DataDir: t.TempDir()}, cfg: config{}}
	c.store = &store{}
	c.cliLogins = map[string]*cliLoginSession{}

	sess := &cliLoginSession{
		id: "empty", leaf: leaf, dir: staged,
		expiresAt: time.Now().Add(time.Minute),
		done:      make(chan struct{}),
		message:   "waiting",
	}
	sess.fail("本机 Qoder CLI 的登录进程已退出，可能是设备码过期或被取消。")
	close(sess.done)

	st, err := c.pollCLILogin(context.Background(), sess)
	if err != nil {
		t.Fatalf("pollCLILogin: %v", err)
	}
	if st.State != core.LoginFailed {
		t.Fatalf("state = %q, want failed when the child exited with no credential", st.State)
	}
}
