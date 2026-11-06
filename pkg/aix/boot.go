package aix

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ParseWhoBoot extracts the boot time from `who -b` output, e.g.
//
//	.        system boot  Nov 06 07:21
//
// who prints no year and only minute precision. The year is taken from now
// (both in UTC, which ExecRunner enforces with TZ=UTC0); a time more than a
// day in the future must be from the previous year. This is the same source
// and precision as the AIX kubelet's GetBootTime.
func ParseWhoBoot(out []byte, now time.Time) (time.Time, error) {
	const marker = "system boot"
	for _, line := range strings.Split(string(out), "\n") {
		i := strings.Index(line, marker)
		if i < 0 {
			continue
		}
		ts := strings.Join(strings.Fields(line[i+len(marker):]), " ")
		now = now.UTC()
		t, err := time.Parse("Jan 2 15:04 2006", fmt.Sprintf("%s %d", ts, now.Year()))
		if err != nil {
			return time.Time{}, fmt.Errorf("unexpected who -b boot record %q: %w", ts, err)
		}
		if t.After(now.Add(24 * time.Hour)) {
			t = t.AddDate(-1, 0, 0)
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("no system boot record in who -b output %q", strings.TrimSpace(string(out)))
}

// BootTime returns the time the host booted, to the minute.
func BootTime(ctx context.Context, r Runner, now time.Time) (time.Time, error) {
	out, err := r.Run(ctx, CmdWho, "-b")
	if err != nil {
		return time.Time{}, fmt.Errorf("reading boot time: %w", err)
	}
	return ParseWhoBoot(out, now)
}
