package scheduler

import (
	"strings"
	"testing"
	"time"
)

func TestNextCronSupportsFiveFieldExpressions(t *testing.T) {
	now := time.Date(2026, 3, 4, 12, 34, 56, 0, CST)

	got, err := NextCron(now, "*/20 9-18 * * 1-5", CST)
	if err != nil {
		t.Fatalf("NextCron returned an error: %v", err)
	}
	want := time.Date(2026, 3, 4, 12, 40, 0, 0, CST)
	if !got.Equal(want) {
		t.Fatalf("NextCron = %s, want %s", got, want)
	}
}

func TestNextCronSupportsSixFieldExpressionsWithSeconds(t *testing.T) {
	now := time.Date(2026, 3, 4, 12, 34, 56, 500_000_000, CST)

	got, err := NextCron(now, "*/10 * * * * *", CST)
	if err != nil {
		t.Fatalf("NextCron returned an error: %v", err)
	}
	want := time.Date(2026, 3, 4, 12, 35, 0, 0, CST)
	if !got.Equal(want) {
		t.Fatalf("NextCron = %s, want %s", got, want)
	}
}

func TestNextCronCanCrossTheMonthBoundary(t *testing.T) {
	now := time.Date(2026, 3, 4, 12, 0, 0, 0, CST)

	got, err := NextCron(now, "0 9 1 * *", CST)
	if err != nil {
		t.Fatalf("NextCron returned an error: %v", err)
	}
	want := time.Date(2026, 4, 1, 9, 0, 0, 0, CST)
	if !got.Equal(want) {
		t.Fatalf("NextCron = %s, want %s", got, want)
	}
}

func TestNextCronUsesStandardDayOfMonthOrWeekdaySemantics(t *testing.T) {
	now := time.Date(2026, 3, 4, 12, 0, 0, 0, CST) // Wednesday

	got, err := NextCron(now, "0 9 1 * 1", CST)
	if err != nil {
		t.Fatalf("NextCron returned an error: %v", err)
	}
	want := time.Date(2026, 3, 9, 9, 0, 0, 0, CST) // the following Monday
	if !got.Equal(want) {
		t.Fatalf("NextCron = %s, want %s", got, want)
	}
}

func TestValidateCronRejectsMalformedExpressions(t *testing.T) {
	for _, expr := range []string{
		"",
		"* * * *",
		"61 * * * *",
		"* 24 * * *",
		"* * 0 * *",
		"* * * 13 *",
		"* * * * 8",
		"*/0 * * * *",
	} {
		t.Run(expr, func(t *testing.T) {
			err := ValidateCron(expr)
			if err == nil {
				t.Fatalf("ValidateCron(%q) = nil, want an error", expr)
			}
			if !strings.Contains(strings.ToLower(err.Error()), "cron") {
				t.Fatalf("ValidateCron(%q) error = %q, want it to mention cron", expr, err)
			}
		})
	}
}
