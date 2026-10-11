package scheduler

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// cronSpec is one parsed cron expression. It accepts the common five-field
// form (minute through weekday) and the six-field form whose first field is
// seconds. The scheduler evaluates it in the configured location, which is
// CST for this application.
type cronSpec struct {
	seconds  []int
	minutes  []int
	hours    []int
	days     []int
	months   []int
	weekdays []int

	dayWildcard bool
	dowWildcard bool
	hasSeconds  bool
}

// ValidateCron reports whether expr is a supported cron expression.
func ValidateCron(expr string) error {
	_, err := parseCron(expr)
	return err
}

// NextCron returns the first cron activation strictly after now in loc.
// A valid expression with no activation in the next ten years returns the
// zero time, which callers treat as "do not schedule".
func NextCron(now time.Time, expr string, loc *time.Location) (time.Time, error) {
	spec, err := parseCron(expr)
	if err != nil {
		return time.Time{}, err
	}
	if loc == nil {
		loc = CST
	}
	return spec.next(now.In(loc)), nil
}

func parseCron(expr string) (cronSpec, error) {
	fields := strings.Fields(strings.TrimSpace(expr))
	var spec cronSpec
	switch len(fields) {
	case 5:
		spec.seconds = []int{0}
	case 6:
		seconds, _, err := parseCronField(fields[0], 0, 59, nil, false)
		if err != nil {
			return cronSpec{}, fmt.Errorf("cron: seconds: %w", err)
		}
		spec.seconds = seconds
		spec.hasSeconds = true
		fields = fields[1:]
	default:
		return cronSpec{}, fmt.Errorf("cron: expected 5 or 6 fields, got %d", len(fields))
	}

	minutes, _, err := parseCronField(fields[0], 0, 59, nil, false)
	if err != nil {
		return cronSpec{}, fmt.Errorf("cron: minutes: %w", err)
	}
	hours, _, err := parseCronField(fields[1], 0, 23, nil, false)
	if err != nil {
		return cronSpec{}, fmt.Errorf("cron: hours: %w", err)
	}
	days, dayWildcard, err := parseCronField(fields[2], 1, 31, nil, true)
	if err != nil {
		return cronSpec{}, fmt.Errorf("cron: day of month: %w", err)
	}
	months, _, err := parseCronField(fields[3], 1, 12, monthNames(), false)
	if err != nil {
		return cronSpec{}, fmt.Errorf("cron: month: %w", err)
	}
	weekdays, dowWildcard, err := parseCronField(fields[4], 0, 7, weekdayNames(), true)
	if err != nil {
		return cronSpec{}, fmt.Errorf("cron: weekday: %w", err)
	}
	weekdays = normalizeCronWeekdays(weekdays)

	spec.minutes = minutes
	spec.hours = hours
	spec.days = days
	spec.months = months
	spec.weekdays = weekdays
	spec.dayWildcard = dayWildcard
	spec.dowWildcard = dowWildcard
	return spec, nil
}

func parseCronField(raw string, min, max int, names map[string]int, allowQuestion bool) ([]int, bool, error) {
	field := strings.TrimSpace(strings.ToLower(raw))
	if field == "" {
		return nil, false, fmt.Errorf("empty field")
	}
	wildcard := field == "*" || (allowQuestion && field == "?")
	values := map[int]struct{}{}
	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, false, fmt.Errorf("empty list item in %q", raw)
		}
		base, step, hasStep, err := splitCronStep(part)
		if err != nil {
			return nil, false, err
		}
		start, end := 0, 0
		switch {
		case base == "*" || (allowQuestion && base == "?"):
			start, end = min, max
		case strings.Contains(base, "-"):
			bounds := strings.Split(base, "-")
			if len(bounds) != 2 {
				return nil, false, fmt.Errorf("invalid range %q", base)
			}
			start, err = parseCronValue(bounds[0], names)
			if err != nil {
				return nil, false, err
			}
			end, err = parseCronValue(bounds[1], names)
			if err != nil {
				return nil, false, err
			}
		default:
			value, err := parseCronValue(base, names)
			if err != nil {
				return nil, false, err
			}
			if !hasStep {
				if err := addCronValue(values, value, min, max); err != nil {
					return nil, false, err
				}
				continue
			}
			start, end = value, max
		}
		if start < min || end > max || start > end {
			return nil, false, fmt.Errorf("%q is outside %d-%d", part, min, max)
		}
		for value := start; value <= end; value += step {
			if err := addCronValue(values, value, min, max); err != nil {
				return nil, false, err
			}
		}
	}
	if len(values) == 0 {
		return nil, false, fmt.Errorf("%q selects no values", raw)
	}
	out := make([]int, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Ints(out)
	return out, wildcard, nil
}

func splitCronStep(part string) (string, int, bool, error) {
	if !strings.Contains(part, "/") {
		return part, 1, false, nil
	}
	pieces := strings.Split(part, "/")
	if len(pieces) != 2 || strings.TrimSpace(pieces[0]) == "" {
		return "", 0, false, fmt.Errorf("invalid step %q", part)
	}
	step, err := strconv.Atoi(strings.TrimSpace(pieces[1]))
	if err != nil || step <= 0 {
		return "", 0, false, fmt.Errorf("step in %q must be a positive whole number", part)
	}
	return strings.TrimSpace(pieces[0]), step, true, nil
}

func parseCronValue(raw string, names map[string]int) (int, error) {
	text := strings.TrimSpace(strings.ToLower(raw))
	if names != nil {
		if value, ok := names[text]; ok {
			return value, nil
		}
		if len(text) >= 3 {
			if value, ok := names[text[:3]]; ok {
				return value, nil
			}
		}
	}
	value, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", raw)
	}
	return value, nil
}

func addCronValue(values map[int]struct{}, value, min, max int) error {
	if value < min || value > max {
		return fmt.Errorf("%d is outside %d-%d", value, min, max)
	}
	values[value] = struct{}{}
	return nil
}

func normalizeCronWeekdays(values []int) []int {
	out := make([]int, 0, len(values))
	seen := map[int]struct{}{}
	for _, value := range values {
		if value == 7 {
			value = 0
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Ints(out)
	return out
}

func monthNames() map[string]int {
	return map[string]int{
		"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
	}
}

func weekdayNames() map[string]int {
	return map[string]int{
		"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
	}
}

func (s cronSpec) next(now time.Time) time.Time {
	earliest := now.Truncate(time.Second)
	if s.hasSeconds {
		earliest = earliest.Add(time.Second)
	} else {
		earliest = now.Truncate(time.Minute).Add(time.Minute)
	}
	loc := now.Location()
	startDay := time.Date(earliest.Year(), earliest.Month(), earliest.Day(), 0, 0, 0, 0, loc)
	const horizonDays = 366 * 10
	for dayOffset := 0; dayOffset < horizonDays; dayOffset++ {
		day := startDay.AddDate(0, 0, dayOffset)
		if !s.matchesDay(day) {
			continue
		}
		for _, hour := range s.hours {
			if sameCronDay(day, earliest) && hour < earliest.Hour() {
				continue
			}
			for _, minute := range s.minutes {
				if sameCronDay(day, earliest) && hour == earliest.Hour() && minute < earliest.Minute() {
					continue
				}
				for _, second := range s.seconds {
					if sameCronDay(day, earliest) && hour == earliest.Hour() && minute == earliest.Minute() && second < earliest.Second() {
						continue
					}
					candidate := time.Date(day.Year(), day.Month(), day.Day(), hour, minute, second, 0, loc)
					if candidate.After(now) {
						return candidate
					}
				}
			}
		}
	}
	return time.Time{}
}

func (s cronSpec) matchesDay(day time.Time) bool {
	if !containsCronValue(s.months, int(day.Month())) {
		return false
	}
	dayMatch := containsCronValue(s.days, day.Day())
	weekdayMatch := containsCronValue(s.weekdays, int(day.Weekday()))
	if !s.dayWildcard && !s.dowWildcard {
		return dayMatch || weekdayMatch
	}
	return dayMatch && weekdayMatch
}

func containsCronValue(values []int, value int) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func sameCronDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}
