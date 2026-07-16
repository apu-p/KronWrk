package schedule

import (
	"testing"
	"time"
)

func TestNextRun(t *testing.T) {
	// 2024-03-10 is the US spring-forward day; use a plain UTC base for the
	// core arithmetic and a separate TZ case below.
	base := time.Date(2024, 1, 1, 10, 30, 0, 0, time.UTC)

	tests := []struct {
		name string
		expr string
		tz   string
		want time.Time
	}{
		{
			name: "every minute",
			expr: "* * * * *",
			tz:   "UTC",
			want: time.Date(2024, 1, 1, 10, 31, 0, 0, time.UTC),
		},
		{
			name: "top of next hour",
			expr: "0 * * * *",
			tz:   "UTC",
			want: time.Date(2024, 1, 1, 11, 0, 0, 0, time.UTC),
		},
		{
			name: "daily at midnight",
			expr: "0 0 * * *",
			tz:   "UTC",
			want: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NextRun(tt.expr, tt.tz, base)
			if err != nil {
				t.Fatalf("NextRun returned error: %v", err)
			}
			if !got.Equal(tt.want) {
				t.Errorf("NextRun(%q) = %s, want %s", tt.expr, got, tt.want)
			}
		})
	}
}

func TestNextRunTimezone(t *testing.T) {
	// "0 9 * * *" in New York is 14:00 UTC during EST (winter).
	base := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	got, err := NextRun("0 9 * * *", "America/New_York", base)
	if err != nil {
		t.Fatalf("NextRun error: %v", err)
	}
	want := time.Date(2024, 1, 1, 14, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %s, want %s (UTC)", got.UTC(), want)
	}
}

func TestNextRunErrors(t *testing.T) {
	if _, err := NextRun("not a cron", "UTC", time.Now()); err == nil {
		t.Error("expected error for invalid expression")
	}
	if _, err := NextRun("* * * * *", "Mars/Phobos", time.Now()); err == nil {
		t.Error("expected error for invalid timezone")
	}
}
