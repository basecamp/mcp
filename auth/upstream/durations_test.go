package upstream

import (
	"math"
	"strconv"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"60", 60 * time.Second},
		{" 60 ", 60 * time.Second},
		{"0", 0},
		{"", 0},
		{"-5", 0},
		{"+5", 0},
		{"5.5", 0},
		{"soon", 0},
		{"Wed, 21 Oct 2026 07:28:00 GMT", 0},
		{"86400", MaxRetryAfter},
		{"86401", MaxRetryAfter},
		// The values that used to overflow time.Duration into a negative
		// backoff, and one past int64.
		{"9223372036", MaxRetryAfter},
		{"9223372037", MaxRetryAfter},
		{strconv.FormatInt(math.MaxInt64, 10), MaxRetryAfter},
		{"99999999999999999999999999", MaxRetryAfter},
	}
	for _, tt := range tests {
		if got := ParseRetryAfter(tt.in); got != tt.want {
			t.Errorf("ParseRetryAfter(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestLifetime(t *testing.T) {
	maxSecs := int64(MaxLifetime / time.Second)
	tests := []struct {
		in   int64
		want time.Duration
	}{
		{300, 300 * time.Second},
		{1, time.Second},
		{0, 0},
		{-1, 0},
		{math.MinInt64, 0},
		{maxSecs - 1, MaxLifetime - time.Second},
		{maxSecs, MaxLifetime},
		{maxSecs + 1, MaxLifetime},
		// 9,223,372,037 s is the first value whose nanoseconds overflow int64.
		{9223372036, MaxLifetime},
		{9223372037, MaxLifetime},
		{math.MaxInt64, MaxLifetime},
	}
	for _, tt := range tests {
		got := Lifetime(tt.in)
		if got != tt.want {
			t.Errorf("Lifetime(%d) = %v, want %v", tt.in, got, tt.want)
		}
		if got < 0 {
			t.Errorf("Lifetime(%d) is negative", tt.in)
		}
	}
}

// TestSeconds_FractionalLimit: a limit that isn't a whole number of seconds
// never raises a value below it.
func TestSeconds_FractionalLimit(t *testing.T) {
	if got := Seconds(1, 1500*time.Millisecond); got != time.Second {
		t.Errorf("Seconds(1, 1.5s) = %v, want 1s", got)
	}
	if got := Seconds(2, 1500*time.Millisecond); got != 1500*time.Millisecond {
		t.Errorf("Seconds(2, 1.5s) = %v, want 1.5s", got)
	}
}
