package core

import (
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
)

// tokenWithPayload builds a three-part token whose payload is exactly the bytes
// given.  Nothing here signs anything: the module never verifies a signature,
// and neither does this test.
func tokenWithPayload(payload string) string {
	return "header." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"
}

func TestJWTClaimReadsAStringClaim(t *testing.T) {
	tok := tokenWithPayload(`{"user_id":"61161790588087632","sub":"61161790588087632"}`)
	if got := JWTClaim(tok, "user_id"); got != "61161790588087632" {
		t.Fatalf("JWTClaim(user_id) = %q, want %q", got, "61161790588087632")
	}
	if got := JWTClaim(tok, "sub"); got != "61161790588087632" {
		t.Fatalf("JWTClaim(sub) = %q, want %q", got, "61161790588087632")
	}
}

// TestJWTClaimReadsANumericClaimWithoutRounding 是这一族里最要紧的一条。厂商
// 把账号 id 当数字发出来（Zhipu 就是），而 61161790588087632 远大于 2^53：
// 用 float64 解一遍会得到 61161790588087632 -> 61161790588087630 这种值，
// 也就是"把两个不同账号的 id 悄悄合成一个"。所以解码必须走 json.Number。
func TestJWTClaimReadsANumericClaimWithoutRounding(t *testing.T) {
	const want = "61161790588087632"
	tok := tokenWithPayload(`{"user_id":` + want + `}`)

	if got := JWTClaim(tok, "user_id"); got != want {
		t.Fatalf("JWTClaim(user_id) = %q, want %q：数字型 claim 被浮点化就会认错账号", got, want)
	}
	// 这条断言不是空的：同样的字节走 float64 会变成另一个数。下面把它算出来，
	// 万一将来这个常数换了、恰好能精确表示，这条测试自己会提醒。
	asFloat := strconv.FormatFloat(61161790588087632, 'f', -1, 64)
	if asFloat == want {
		t.Fatalf("这个常数用 float64 也能精确表示（%s），所以本测试挡不住浮点化：换一个更大的 id", asFloat)
	}
}

func TestJWTClaimTakesTheFirstNameThatIsPresent(t *testing.T) {
	tok := tokenWithPayload(`{"uid":"u-1","sub":"s-1"}`)
	if got := JWTClaim(tok, "user_id", "uid", "sub"); got != "u-1" {
		t.Fatalf("JWTClaim = %q, want u-1：名字是按顺序试的，不是按 JSON 里的顺序", got)
	}
	// 先试的名字存在但是空串，要落到下一个有值的名字上。
	blank := tokenWithPayload(`{"user_id":"   ","sub":"s-2"}`)
	if got := JWTClaim(blank, "user_id", "sub"); got != "s-2" {
		t.Fatalf("JWTClaim = %q, want s-2：空白 claim 不算数", got)
	}
}

func TestJWTClaimRejectsWhatItCannotRead(t *testing.T) {
	for _, c := range []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"one part", "not-a-token"},
		{"two parts", "header.payload"},
		{"four parts", "a.b.c.d"},
		{"payload is not base64", "header.!!!!.sig"},
		{"payload is not JSON", tokenWithPayload(`not json`)},
		{"payload is a JSON array", tokenWithPayload(`["user_id"]`)},
		{"payload is a JSON string", tokenWithPayload(`"user_id"`)},
		{"claim absent", tokenWithPayload(`{"other":"x"}`)},
		{"claim is null", tokenWithPayload(`{"user_id":null}`)},
		{"claim is an object", tokenWithPayload(`{"user_id":{"id":"x"}}`)},
		{"claim is a bool", tokenWithPayload(`{"user_id":true}`)},
		{"claim is empty", tokenWithPayload(`{"user_id":""}`)},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := JWTClaim(c.token, "user_id", "sub"); got != "" {
				t.Fatalf("JWTClaim(%q) = %q, want \"\"", c.token, got)
			}
		})
	}
}

// TestJWTClaimAcceptsAPaddedPayload 覆盖另一种 base64 方言：有的实现把 "=" 补齐
// 再发出来，有的不补。两种都要能读，因为读不出来就等于"这个账号认不出来"，
// 而那在面板上表现为凭空多出一个账号。
func TestJWTClaimAcceptsAPaddedPayload(t *testing.T) {
	padded := "header." + base64.URLEncoding.EncodeToString([]byte(`{"user_id":"u-9"}`)) + ".sig"
	if got := JWTClaim(padded, "user_id"); got != "u-9" {
		t.Fatalf("JWTClaim(padded) = %q, want u-9", got)
	}
}

func TestJWTIdentityUsesTheVendorClaimNames(t *testing.T) {
	for _, c := range []struct {
		payload string
		want    string
	}{
		{`{"user_id":"a"}`, "a"},
		{`{"userId":"b"}`, "b"},
		{`{"uid":"c"}`, "c"},
		{`{"sub":"d"}`, "d"},
		{`{"user_id":"a","sub":"d"}`, "a"},
		{`{"sub":"d","uid":"c"}`, "c"},
		{`{"name":"n"}`, ""},
	} {
		if got := JWTIdentity(tokenWithPayload(c.payload)); got != c.want {
			t.Errorf("JWTIdentity(%s) = %q, want %q", c.payload, got, c.want)
		}
	}
}

func TestJWTExpiryReadsUnixSeconds(t *testing.T) {
	if got := JWTExpiry(tokenWithPayload(`{"exp":1790588088}`)); got != 1790588088 {
		t.Fatalf("JWTExpiry = %d, want 1790588088", got)
	}
	// 字符串形态的 exp 也照读：它和数字形态说的是同一件事，而这里读的只是一个
	// 显示用的过期时间，不是授权决定。
	if got := JWTExpiry(tokenWithPayload(`{"exp":"1790588088"}`)); got != 1790588088 {
		t.Fatalf("JWTExpiry(string exp) = %d, want 1790588088", got)
	}
	for _, c := range []struct{ name, payload string }{
		{"absent", `{"sub":"d"}`},
		{"fractional", `{"exp":1790588088.5}`},
		{"not a number", `{"exp":"soon"}`},
		{"zero", `{"exp":0}`},
		{"negative", `{"exp":-1}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := JWTExpiry(tokenWithPayload(c.payload)); got != 0 {
				t.Fatalf("JWTExpiry(%s) = %d, want 0", c.payload, got)
			}
		})
	}
}

// TestJWTClaimNeverReturnsTheWholeToken 是一条不变量：这个函数的返回值会被写进
// 面板的 identity 字段，而面板只是把 identity 当标签用。它绝不能把 token 本身
// 当结果交出去——那会把一个凭据放进一个每张表都在渲染的字段里。
func TestJWTClaimNeverReturnsTheWholeToken(t *testing.T) {
	tok := tokenWithPayload(`{"user_id":"u-1"}`)
	for _, name := range []string{"user_id", "sub", "exp", "payload", "signature"} {
		if got := JWTClaim(tok, name); got != "" && strings.Contains(tok, got) && strings.Count(got, ".") == 2 {
			t.Fatalf("JWTClaim(%s) returned something token-shaped: %q", name, got)
		}
	}
}
