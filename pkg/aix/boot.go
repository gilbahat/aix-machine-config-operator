package aix

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseElapsed parses a ps etime value, [[dd-]hh:]mm:ss.
func ParseElapsed(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	var days int
	if d, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.Atoi(d)
		if err != nil {
			return 0, fmt.Errorf("unexpected elapsed time %q", s)
		}
		days, s = n, rest
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("unexpected elapsed time %q", s)
	}
	var total int
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("unexpected elapsed time %q", s)
		}
		total = total*60 + n
	}
	return time.Duration(days)*24*time.Hour + time.Duration(total)*time.Second, nil
}

// BootTime returns when the host booted: now minus the elapsed time of init
// (`ps -o etime= -p 1`). Unlike `who -b`, which prints no year and only
// minutes, this is unambiguous for any uptime. The result is accurate to
// about a second plus the time ps takes; compare boot times with SameBoot.
func BootTime(ctx context.Context, r Runner, now time.Time) (time.Time, error) {
	out, err := r.Run(ctx, CmdPs, "-o", "etime=", "-p", "1")
	if err != nil {
		return time.Time{}, fmt.Errorf("reading the boot time: %w", err)
	}
	up, err := ParseElapsed(string(out))
	if err != nil {
		return time.Time{}, err
	}
	return now.Add(-up).UTC().Truncate(time.Second), nil
}

// bootTolerance absorbs the jitter of deriving the boot time from uptime.
const bootTolerance = 2 * time.Minute

// SameBoot reports whether two boot times (RFC 3339) belong to one boot.
func SameBoot(a, b string) bool {
	ta, errA := time.Parse(time.RFC3339, a)
	tb, errB := time.Parse(time.RFC3339, b)
	if errA != nil || errB != nil {
		return a == b
	}
	d := ta.Sub(tb)
	return d < bootTolerance && d > -bootTolerance
}
