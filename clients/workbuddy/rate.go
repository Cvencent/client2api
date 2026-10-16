package workbuddy

import (
	"math"
	"strconv"
	"strings"
)

// Model pricing.  WorkBuddy quotes a model's price as a text multiplier --
// "x0.15 credits", "x0.29", "x0.00 credits" -- and quotes the campaign that is
// in force right now in a second field.  The panel already normalises the
// spellings into one number; the router needs the same answer, because a
// multiplier of zero is the one pricing fact with a routing consequence: a
// model the vendor charges nothing for does not depend on the account's
// balance, so an account parked for an empty wallet may still serve it.

// freeRateText lists the literal spellings a vendor uses instead of a zero.
// They are compared case-insensitively after trimming, because the field is
// user-visible free text rather than a number.
func freeRateText(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "免费", "free", "free_unlimited":
		return true
	}
	return false
}

// modelRate parses one multiplier field.  ok is false for an empty or
// unparseable value, which callers must read as "unknown": treating a label
// like "高" as zero would publish a paid model as free and let it bypass the
// account's balance.
//
// The accepted shapes mirror the panel's rateNumber(): an optional leading or
// trailing "x"/"X"/"×", an optional "credit(s)" unit, and a plain number.
func modelRate(raw string) (float64, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, false
	}
	if freeRateText(s) {
		return 0, true
	}
	s = strings.TrimSpace(strings.TrimSuffix(s, "credits"))
	s = strings.TrimSpace(strings.TrimSuffix(s, "credit"))
	s = strings.Trim(strings.TrimSpace(s), "xX×")
	s = strings.TrimSpace(s)
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, false
	}
	return v, true
}

// effectiveModelRate reports the multiplier actually in force for one model.
//
// The promotion wins when the catalogue carried one: applyModelPromotions only
// attaches promo_* to a campaign that is inside its validity range and its
// daily window, so a "free at night" discount never marks the model free at
// noon.  An unparseable promotion is reported as unknown rather than falling
// back to the list price, matching the panel, which shows the promotion text
// instead of the list price once it is present.
func effectiveModelRate(mi ModelInfo) (float64, bool) {
	if strings.TrimSpace(mi.PromoCredits) != "" {
		return modelRate(mi.PromoCredits)
	}
	return modelRate(mi.Credits)
}

// modelIsFree reports whether the vendor currently charges nothing for this
// model.  Only a parsed zero counts: an unknown price is not free.
func modelIsFree(mi ModelInfo) bool {
	v, ok := effectiveModelRate(mi)
	return ok && v == 0
}
