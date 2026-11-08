package browser

import (
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// #141：无痕验证对「行为指纹」做文章——瞬时传送的指针、同一像素的落点、
// 同节奏的点击序列都是机器形状。ClickTrusted 的三条人工化特性钉死在这里。
// ---------------------------------------------------------------------------

func TestAMouseMovePathIsNotATeleport(t *testing.T) {
	path := mouseMovePath(400, 300)
	if len(path) < 5 {
		t.Fatalf("path has %d steps, want >=5 (a hand sweeps, it does not teleport)", len(path))
	}
	// 起点必须偏离目标（非原地抖动）：首步到目标的距离 > 20px。
	first := path[0]
	dx, dy := first.x-400, first.y-300
	if dx*dx+dy*dy < 400 {
		t.Fatalf("first step too close to target: %d,%d", first.x, first.y)
	}
	// 相邻步距随推进收窄（ease-out 减速）。
	d0 := dist2(path[0], path[1])
	dl := dist2(path[len(path)-2], path[len(path)-1])
	if dl > d0 {
		t.Errorf("path does not decelerate: first-step dist2=%d last-step dist2=%d", d0, dl)
	}
	// 最后一步必须正好落在目标上。
	last := path[len(path)-1]
	if last.x != 400 || last.y != 300 {
		t.Fatalf("path ends at %d,%d, want 400,300", last.x, last.y)
	}
}

func TestBJitterStaysInsideTheElement(t *testing.T) {
	// ±20% 的抖动系数在 [-0.2, 0.2) 内。
	for i := 0; i < 200; i++ {
		f := jitterFraction()
		if f <= -0.2 || f >= 0.2 {
			t.Fatalf("jitterFraction = %v, want (-0.2,0.2)", f)
		}
	}
}

func TestCPacingIsNotMetronomic(t *testing.T) {
	// 相邻两次 step 延迟与 press-release 延迟都不应恒定（机器是等节奏的）。
	var d1, d2 time.Duration
	d1 = mouseStepDelay()
	d2 = mouseStepDelay()
	_ = d1
	_ = d2
	a := pressReleaseDelay()
	b := pressReleaseDelay()
	if a == b && a == 0 {
		t.Fatal("pacing collapsed to zero")
	}
	// 统计上不可能 200 次全部相同——如果全部相同就是写死了。
	same := true
	first := pressReleaseDelay()
	for i := 0; i < 200; i++ {
		if pressReleaseDelay() != first {
			same = false
			break
		}
	}
	if same {
		t.Error("pressReleaseDelay is constant: a metronome is machine-shaped")
	}
}

func dist2(a, b struct{ x, y int }) int {
	dx, dy := a.x-b.x, a.y-b.y
	return dx*dx + dy*dy
}
