package codearts

import (
	"testing"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// #255：81001 双义——主对话路径的超窗语义（"input token limit is 1048576"）
// 必须归为 FailureContextWindow（不 rotate，gateway 学窗/trim-retry 才能看到）；
// fold 路径的「messages 参数非法」（#240 实验语义）不得被误吞。
// ---------------------------------------------------------------------------

func TestAOverWindowBodyMapsToContextWindow(t *testing.T) {
	if got := classifyStatus(400, `InferHub.ModelArts.81001.400: input token limit is 1048576`); got != kindContextWindow {
		t.Fatalf("classifyStatus = %v, want kindContextWindow (issue #255)", got)
	}
	if got := failureKind(kindContextWindow); got != core.FailureContextWindow {
		t.Fatalf("failureKind = %v, want core.FailureContextWindow (issue #255)", got)
	}
}

func TestBFoldWordingIsNotSwallowed(t *testing.T) {
	// fold 路径的 81001 文本（#240 实验语义）不含超窗词——不得被新正则误吞。
	if got := classifyStatus(400, `InferHub.ModelArts.81001.400: messages 参数非法`); got == kindContextWindow {
		t.Fatal("the fold wording was classified as over-window (issue #255 反向钉)")
	}
}

func TestCEstablishedBranchesKeepTheirOrder(t *testing.T) {
	// auth/queue/quota 分支在超窗分支之前保持现序（issue 反向钉）。
	if got := classifyStatus(400, "invalid token"); got != kindAuth {
		t.Errorf("auth body = %v, want kindAuth", got)
	}
	if got := classifyStatus(400, "TM.00001041"); got != kindQueue {
		t.Errorf("queue body = %v, want kindQueue", got)
	}
	if got := classifyStatus(400, "积分已耗尽"); got != kindQuota {
		t.Errorf("quota body = %v, want kindQuota", got)
	}
}
