package db

import (
	"testing"
	"time"

	"kronwrk/internal/models"
)

func TestWaitPlan(t *testing.T) {
	scheduledFor := time.Date(2026, 7, 10, 2, 0, 0, 0, time.UTC)
	hour := scheduledFor.Add(time.Hour)
	tests := []struct {
		name         string
		waits        []int
		wantStatus   string
		wantDeadline *time.Time
	}{
		{"no conditions queues immediately", nil, models.StatusQueued, nil},
		{"single wait-forever", []int{0}, models.StatusWaiting, nil},
		{"wait-forever dominates a bounded wait", []int{0, 3600}, models.StatusWaiting, nil},
		{"bounded waits use the max", []int{60, 3600}, models.StatusWaiting, &hour},
		{"single bounded wait", []int{3600}, models.StatusWaiting, &hour},
	}
	for _, tt := range tests {
		status, deadline := waitPlan(scheduledFor, tt.waits)
		if status != tt.wantStatus {
			t.Errorf("%s: status = %q, want %q", tt.name, status, tt.wantStatus)
		}
		switch {
		case (deadline == nil) != (tt.wantDeadline == nil):
			t.Errorf("%s: deadline = %v, want %v", tt.name, deadline, tt.wantDeadline)
		case deadline != nil && !deadline.Equal(*tt.wantDeadline):
			t.Errorf("%s: deadline = %v, want %v", tt.name, *deadline, *tt.wantDeadline)
		}
	}
}
