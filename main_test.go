package main

import (
	"testing"
	"time"
)

func TestParseClock(t *testing.T) {
	hour, minute, ok := parseClock("09:30")
	if !ok {
		t.Fatalf("expected parseClock to succeed")
	}
	if hour != 9 || minute != 30 {
		t.Fatalf("expected 09:30, got %02d:%02d", hour, minute)
	}

	_, _, ok = parseClock("09-30")
	if ok {
		t.Fatalf("expected parseClock to fail on invalid format")
	}
}

func TestParseWeeklySchedule(t *testing.T) {
	weekday, hour, minute, ok := parseWeeklySchedule("monday 09:00")
	if !ok {
		t.Fatalf("expected parseWeeklySchedule to succeed")
	}
	if weekday != time.Monday || hour != 9 || minute != 0 {
		t.Fatalf("unexpected weekly schedule parsed")
	}

	_, _, _, ok = parseWeeklySchedule("monday")
	if ok {
		t.Fatalf("expected parseWeeklySchedule to fail on missing time")
	}
}

func TestNextDailyRun(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 1, 22, 8, 0, 0, 0, loc)
	next := nextDailyRun(now, 9, 0)
	if next.Day() != now.Day() || next.Hour() != 9 || next.Minute() != 0 {
		t.Fatalf("expected same-day 09:00, got %v", next)
	}

	now = time.Date(2026, 1, 22, 10, 0, 0, 0, loc)
	next = nextDailyRun(now, 9, 0)
	if next.Day() == now.Day() {
		t.Fatalf("expected next day run, got %v", next)
	}
}

func TestNextWeeklyRun(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 1, 19, 8, 0, 0, 0, loc) // Monday
	next := nextWeeklyRun(now, time.Monday, 9, 0)
	if next.Weekday() != time.Monday || next.Hour() != 9 || next.Minute() != 0 || next.Day() != now.Day() {
		t.Fatalf("expected same-day Monday 09:00, got %v", next)
	}

	now = time.Date(2026, 1, 19, 10, 0, 0, 0, loc)
	next = nextWeeklyRun(now, time.Monday, 9, 0)
	if next.Weekday() != time.Monday || next.Day() == now.Day() {
		t.Fatalf("expected next Monday run, got %v", next)
	}
}
