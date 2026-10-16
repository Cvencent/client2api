package qwenwork

import (
	"math"
	"strconv"
	"strings"
)

// rate.go reads the vendor's own `credits` multiplier.
//
// The field arrives as text, not a number: "1", "0", "x0.15", or a literal
// 免费 / free.  Only a parsed zero counts as free -- an unreadable or negative
// value is not free, because guessing would send traffic the wallet cannot pay
// for.

// creditsAreFree reports whether the credits multiplier bills nothing.
func creditsAreFree(raw string) bool {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return false
	}
	if s == "免费" || s == "free" || s == "free_unlimited" {
		return true
	}
	s = strings.TrimSpace(strings.TrimPrefix(s, "x"))
	s = strings.TrimSpace(strings.TrimSuffix(s, "x"))
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return false
	}
	return v == 0
}
