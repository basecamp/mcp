package upstream

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// MaxRetryAfter is the longest backoff an endpoint's Retry-After is taken
// at. bc3 escalates abuse blocks, so the bound is generous; past a day the
// value is broken rather than a block, and an unbounded one overflows
// time.Duration (2^63 ns is about 292 years) into a negative backoff.
const MaxRetryAfter = 24 * time.Hour

// MaxLifetime is the longest token lifetime an endpoint's expires_in is
// taken at. Exchanged tokens live for minutes and stdio access tokens for
// weeks; the bound only keeps a broken value from overflowing time.Duration
// or a Unix timestamp into a token that is already expired.
const MaxLifetime = 366 * 24 * time.Hour

// ParseRetryAfter reads a Retry-After header in its delta-seconds form
// (RFC 9110 §10.2.3: one or more digits) and returns it bounded to
// [0, MaxRetryAfter]. Zero means unknown: empty, the HTTP-date form, a sign,
// or anything else that is not digits. A digit string too large for int64 is
// a huge value, so it takes the bound.
func ParseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" || strings.TrimLeft(v, "0123456789") != "" {
		return 0
	}
	secs, err := strconv.ParseInt(v, 10, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0
	}
	return Seconds(secs, MaxRetryAfter)
}

// Lifetime converts an expires_in value to a duration bounded to
// [0, MaxLifetime]. A zero or negative value returns 0; callers decide what
// a lifetime of zero means for them.
func Lifetime(secs int64) time.Duration {
	return Seconds(secs, MaxLifetime)
}

// Seconds converts a count of seconds to a duration bounded to [0, limit],
// comparing before multiplying so no value can overflow.
func Seconds(secs int64, limit time.Duration) time.Duration {
	switch {
	case secs <= 0:
		return 0
	case secs > int64(limit/time.Second):
		return limit
	default:
		return time.Duration(secs) * time.Second
	}
}
