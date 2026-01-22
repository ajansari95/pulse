package main

import (
	"net/http/httptest"
	"strings"
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

func TestWebhookSendersSkipWhenUnset(t *testing.T) {
	monitor := NewMonitor(Config{})
	if err := monitor.sendSlackAlert("test"); err != nil {
		t.Fatalf("expected slack sender to skip, got %v", err)
	}
	if err := monitor.sendDiscordAlert("test"); err != nil {
		t.Fatalf("expected discord sender to skip, got %v", err)
	}
	if err := monitor.sendPagerDutyAlert(AlertDetails{Kind: "down", PlainMessage: "test"}); err != nil {
		t.Fatalf("expected pagerduty sender to skip, got %v", err)
	}
}

func TestPrometheusLabelValue(t *testing.T) {
	value := "api\"prod\\test\n"
	if got := prometheusLabelValue(value); got != "api\\\"prod\\\\test\\n" {
		t.Fatalf("unexpected label escape: %s", got)
	}
}

func TestMetricsHandler(t *testing.T) {
	config := Config{Endpoints: []Endpoint{{Name: "api\"prod"}}}
	monitor := NewMonitor(config)
	monitor.statuses["api\"prod"] = &EndpointStatus{IsUp: true, ResponseTime: 150 * time.Millisecond}
	monitor.totalChecks["api\"prod"] = 3
	monitor.totalFailures["api\"prod"] = 1

	req := httptest.NewRequest("GET", "/metrics", nil)
	recorder := httptest.NewRecorder()
	monitor.MetricsHandler().ServeHTTP(recorder, req)

	body := recorder.Body.String()
	if !strings.Contains(body, "pulse_endpoint_up{name=\"api\\\"prod\"} 1") {
		t.Fatalf("expected up metric, got %s", body)
	}
	if !strings.Contains(body, "pulse_endpoint_total_checks{name=\"api\\\"prod\"} 3") {
		t.Fatalf("expected checks metric, got %s", body)
	}
}

func TestParseAlertDetails(t *testing.T) {
	message := "🔴 <b>API</b> is DOWN\n\nEndpoint: <code>https://example.com</code>\nError: timeout"
	details := parseAlertDetails(message)
	if details.Kind != "down" {
		t.Fatalf("expected down kind, got %s", details.Kind)
	}
	if details.EndpointName != "API" {
		t.Fatalf("expected endpoint API, got %s", details.EndpointName)
	}
	if strings.Contains(details.PlainMessage, "<b>") || strings.Contains(details.PlainMessage, "<code>") {
		t.Fatalf("expected plain message to strip HTML tags")
	}

	summary := parseAlertDetails("📊 <b>Daily Summary</b>\n")
	if summary.Kind != "summary" {
		t.Fatalf("expected summary kind, got %s", summary.Kind)
	}
}

func TestDaysUntilExpiry(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if days := daysUntilExpiry(now, now.Add(48*time.Hour)); days != 2 {
		t.Fatalf("expected 2 days, got %d", days)
	}
	if days := daysUntilExpiry(now, now.Add(-24*time.Hour)); days != -1 {
		t.Fatalf("expected -1 days, got %d", days)
	}
}

func TestParseMaintenanceSchedule(t *testing.T) {
	weekday, startHour, startMinute, endHour, endMinute, ok := parseMaintenanceSchedule("sunday 02:00-04:00")
	if !ok {
		t.Fatalf("expected maintenance schedule to parse")
	}
	if weekday != time.Sunday || startHour != 2 || startMinute != 0 || endHour != 4 || endMinute != 0 {
		t.Fatalf("unexpected maintenance schedule values")
	}
}

func TestIsWithinWeeklyWindow(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 1, 18, 3, 0, 0, 0, loc) // Sunday
	if !isWithinWeeklyWindow(now, time.Sunday, 2, 0, 4, 0) {
		t.Fatalf("expected within maintenance window")
	}
	if isWithinWeeklyWindow(now, time.Sunday, 4, 0, 5, 0) {
		t.Fatalf("expected outside maintenance window")
	}
}
