package panel

import (
	"os"
	"strings"
	"testing"
)

// clientsSource 读取 clients/ 下一个模块的源码，供「上游必须真的上报这个
// 状态字面量」的看守用。读不到就跳过该条的定位并报错，测试也不该静默通过。
func clientsSource(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile("../../clients/" + rel)
	if err != nil {
		t.Fatalf("cannot read clients/%s: %v", rel, err)
	}
	src := string(b)
	src = strings.ReplaceAll(src, "\r\n", "\n")
	src = strings.ReplaceAll(src, "\r", "\n")
	return src
}

// ---------------------------------------------------------------------------
// low_credit 不再混进「冷却中」桶（issue #24）的模块侧三拼图。
//
// stateCategory 面板侧（7752d3d）早就拆出独立档，但当初按钮上「积分 0 的
// 账号显示 冷却中」的真因是**没有任何模块上报 state="low_credit"**：loomy 的
// lowCredit 分支、officeace 的零积分停车、workbuddy 的余额护栏停车都在数据
// 层写着 "cooling"。看守把三处全部钉住：状态字面量必须真的被上报。
// ---------------------------------------------------------------------------

// TestLoomyReportsLowCredit 钉 loomy 的 lowCredit 分支：它上报的 state 必须是
// "low_credit"（经济态独立档），而不是 "cooling"。
func TestLoomyReportsLowCredit(t *testing.T) {
	src := clientsSource(t, "loomy/accounts.go")

	i := strings.Index(src, "case a.lowCredit:")
	if i < 0 {
		t.Fatal("loomy 的 lowCredit 分支不在：看守失去锚点（issue #24）")
	}
	// 只看这个 case 自己的范围（到下一个 case 为止），别把后面限流分支的
	// cooling 误算进来。
	nxt := strings.Index(src[i:], "\n\tcase ")
	seg := src[i:]
	if nxt >= 0 {
		seg = src[i : i+nxt]
	}
	if !strings.Contains(seg, `record.State = "low_credit"`) {
		t.Error(`loomy 的 lowCredit 分支上报的 state 不是 "low_credit"：积分 0 的账号永远落不进 面板的 积分不足 桶，仍显示 冷却中（issue #24）`)
	}
	if strings.Contains(seg, `record.State = "cooling"`) {
		t.Error(`loomy 的 lowCredit 分支回到 "cooling"：经济态又和分钟级限流混计（issue #24）`)
	}
	// 路由层不能跟着变：lowCredit 的账号仍然不参与选路（usable 的第一行就拦住）。
	j := strings.Index(src, "if !a.Enabled || a.dead || a.abnormal || a.lowCredit {")
	if j < 0 {
		t.Error("loomy 的 usable 不再挡 lowCredit：拆桶 changed 为纯展示，选路行为不能跟着改（issue #24）")
	}
}

// TestWorkbuddyReserveParkReportsLowCredit 钉 workbuddy 的余额护栏停车：护栏
// 自己创建（note == reserveCreditNote）的那种停车必须上报 "low_credit"，
// 利率限/熔断/凭据的 own 停车不受影响。
func TestWorkbuddyReserveParkReportsLowCredit(t *testing.T) {
	src := clientsSource(t, "workbuddy/pool.go")

	i := strings.Index(src, "func (p *Pool) Snapshot()")
	if i < 0 {
		t.Fatal("workbuddy 的 Snapshot 不在：看守失去锚点（issue #24）")
	}
	body := src[i : i+2600]
	if !strings.Contains(body, "stateExhausted && e.note == reserveCreditNote") {
		t.Error("workbuddy 的余额护栏停车没有单独识别：护栏车与限流车不可拆分（issue #24）")
	}
	if !strings.Contains(body, `state = "low_credit"`) {
		t.Error(`workbuddy 识别到的护栏停车没有上报 "low_credit"：面板积分不足 桶收不到 row（issue #24）`)
	}
	// 护栏的常量必须还在（这是护栏自己创建的那种停车的唯一可再生判据）。
	if !strings.Contains(src, `const reserveCreditNote = "low_credit"`) {
		t.Error("reserveCreditNote 常量不在：护栏停车的判据丢失（issue #24）")
	}
}

// TestOfficeAceZeroPointParkingReportsLowCredit 钉 officeace 的零积分停车：
// 模块读数为 0 时（pointsAt 有 read）上报 "low_credit"；-1（未泄）与正读数
// 不改判。
func TestOfficeAceZeroPointParkingReportsLowCredit(t *testing.T) {
	src := clientsSource(t, "openaicompat/pool.go")
	office := clientsSource(t, "openaicompat/officeace.go")

	// 池子必须真的问 hook。
	if !strings.Contains(src, "stateHook func(id string) string") {
		t.Error("providerPool 的 stateHook 槽位不在：办公室模块没有通道上报自己的经济态判决（issue #24）")
	}
	if !strings.Contains(src, "p.stateHook(a.ID)") {
		t.Error("stateAt 不咨询 stateHook：挂钩挂了也不生效（issue #24）")
	}
	// 模块侧：绑定 hook 的函数在文件里。
	if !strings.Contains(office, "func (o *officeAceSource) bindStateHook()") {
		t.Error("officeace 的 bindStateHook 不在：零积分读数无法被归类为经济态（issue #24）")
	}
	i := strings.Index(office, `if o.points[id] == 0 && !o.pointsAt[id].IsZero() {`)
	if i < 0 {
		t.Fatal("officeace 的零读数判据不在：看守失去锚点（issue #24）")
	}
	seg := office[i : i+160]
	if !strings.Contains(seg, `"low_credit"`) {
		t.Error("officeace 的零读数没有上报 low_credit：经济态判决落不进面板（issue #24）")
	}
	// -1（"not drained"）与正读数必须不改判——判据必须是精确的 == 0。
	neg := strings.Index(office, `if o.points[id] == 0`)
	if neg > 0 && strings.Contains(office[neg:neg+120], "points[id] != 0") {
		t.Error("判据写反：-1 与正读数也被拉进经济态（issue #24）")
	}
}
