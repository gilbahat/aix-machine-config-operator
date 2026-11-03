package aix

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// OSLevel is the parsed output of `oslevel -s`, e.g. 7300-02-01-2346.
type OSLevel struct {
	Raw        string
	Version    string // "7.3"
	TL         string // technology level, "02"
	SP         string // service pack, "01"
	BuildDate  string // "2346"
	MajorMinor [2]int
}

var oslevelRE = regexp.MustCompile(`^(\d)(\d)00-(\d{2})-(\d{2})-(\d{4})$`)

// ParseOSLevel parses `oslevel -s` output.
func ParseOSLevel(out []byte) (OSLevel, error) {
	raw := strings.TrimSpace(string(out))
	m := oslevelRE.FindStringSubmatch(raw)
	if m == nil {
		return OSLevel{}, fmt.Errorf("unexpected oslevel -s output %q", raw)
	}
	lvl := OSLevel{Raw: raw, Version: m[1] + "." + m[2], TL: m[3], SP: m[4], BuildDate: m[5]}
	lvl.MajorMinor = [2]int{int(m[1][0] - '0'), int(m[2][0] - '0')}
	return lvl, nil
}

// GetOSLevel runs `oslevel -s`.
func GetOSLevel(ctx context.Context, r Runner) (OSLevel, error) {
	out, err := r.Run(ctx, CmdOslevel, "-s")
	if err != nil {
		return OSLevel{}, err
	}
	return ParseOSLevel(out)
}

// MinimumSupportedOSLevel is the oldest AIX release Go supports.
var MinimumSupportedOSLevel = [2]int{7, 2}

// CheckSupported returns an error for AIX releases older than 7.2.
func (l OSLevel) CheckSupported() error {
	if l.MajorMinor[0] < MinimumSupportedOSLevel[0] ||
		(l.MajorMinor[0] == MinimumSupportedOSLevel[0] && l.MajorMinor[1] < MinimumSupportedOSLevel[1]) {
		return fmt.Errorf("AIX %s is not supported; AIX %d.%d or later is required", l.Version, MinimumSupportedOSLevel[0], MinimumSupportedOSLevel[1])
	}
	return nil
}
