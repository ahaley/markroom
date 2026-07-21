package web

import (
	"testing"
	"time"
)

func TestReadMins(t *testing.T) {
	tests := []struct{ words, want int }{
		{0, 1}, {219, 1}, {220, 1}, {440, 2}, {2200, 10},
	}
	for _, tt := range tests {
		if got := readMins(tt.words); got != tt.want {
			t.Errorf("readMins(%d) = %d, want %d", tt.words, got, tt.want)
		}
	}
}

func TestTimeAgo(t *testing.T) {
	now := time.Now()
	tests := []struct {
		ago  time.Duration
		want string
	}{
		{30 * time.Second, "just now"},
		{5 * time.Minute, "5m ago"},
		{3 * time.Hour, "3h ago"},
		{5 * 24 * time.Hour, "5d ago"},
	}
	for _, tt := range tests {
		if got := timeAgo(now.Add(-tt.ago)); got != tt.want {
			t.Errorf("timeAgo(-%v) = %q, want %q", tt.ago, got, tt.want)
		}
	}
	old := time.Date(2020, 3, 15, 12, 0, 0, 0, time.Local)
	if got := timeAgo(old); got != "Mar 15, 2020" {
		t.Errorf("timeAgo(old) = %q, want Mar 15, 2020", got)
	}
}
