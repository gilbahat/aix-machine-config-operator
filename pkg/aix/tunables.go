package aix

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

// TunableCommands maps the tunable commands AMCO may run to their paths.
// No other command is ever used for tunables.
var TunableCommands = map[string]string{
	"no":     "/usr/sbin/no",
	"vmo":    "/usr/sbin/vmo",
	"ioo":    "/usr/sbin/ioo",
	"schedo": "/usr/sbin/schedo",
	"nfso":   "/usr/sbin/nfso",
	"raso":   "/usr/sbin/raso",
}

// TunableType is the TYPE letter the tunable commands print in their -L and
// -x listings. The letters and meanings are from the AIX 7.x ioo, no and vmo
// command references:
//
//	D  Dynamic      can be freely changed
//	S  Static       cannot be changed
//	R  Reboot       can only be changed during reboot
//	B  Bosboot      can only be changed using bosboot and reboot
//	M  Mount        changes are only effective for future mountings
//	I  Incremental  can only be incremented (decreases take effect at reboot)
//	C  Connect      changes are only effective for future socket connections
//	d  deprecated   deprecated and cannot be changed
type TunableType string

// Tunable types.
const (
	TunableDynamic     TunableType = "D"
	TunableStatic      TunableType = "S"
	TunableReboot      TunableType = "R"
	TunableBosboot     TunableType = "B"
	TunableMount       TunableType = "M"
	TunableIncremental TunableType = "I"
	TunableConnect     TunableType = "C"
	TunableDeprecated  TunableType = "d"
)

// TunableInfo is one row of a tunable command's -x listing:
//
//	tunable,current,default,reboot,min,max,unit,type,{dtunable }
type TunableInfo struct {
	Name, Current, Default, Boot, Min, Max, Unit string
	Type                                         TunableType
	// Restricted tunables are only listed with -F. IBM advises against
	// changing them, and the commands prompt for confirmation when -p or -r
	// changes one, so AMCO refuses to change them.
	Restricted bool
}

// Patterns for tunable names and values; the CRD schema uses the same patterns.
const (
	// Names are mixed case for JFS2 (j2_maxPageReadAhead) and may contain
	// '%' (minperm%); the value always travels inside one name=value argument.
	TunableNamePattern  = `^[A-Za-z][A-Za-z0-9_%]{0,63}$`
	TunableValuePattern = `^-?[0-9]{1,20}$`
)

var tunableNameRE = regexp.MustCompile(TunableNamePattern)

// ValidateTunable checks a command, name and value before they reach a
// tunable command. Values are decimal integers.
func ValidateTunable(command, name, value string) error {
	if _, ok := TunableCommands[command]; !ok {
		return fmt.Errorf("tunable command %q is not one of no, vmo, ioo, schedo, nfso, raso", command)
	}
	if !tunableNameRE.MatchString(name) {
		return fmt.Errorf("invalid %s tunable name %q", command, name)
	}
	if !tunableValueRE.MatchString(value) {
		return fmt.Errorf("%s %s: value %q must be a decimal integer", command, name, value)
	}
	return nil
}

var tunableValueRE = regexp.MustCompile(TunableValuePattern)

// ParseTunableListing parses `<cmd> -x` output into rows by name. Lines that
// are not tunable rows (fewer than eight comma-separated fields) are skipped.
func ParseTunableListing(out []byte) (map[string]TunableInfo, error) {
	rows := map[string]TunableInfo{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Split(strings.TrimSpace(line), ",")
		if len(f) < 8 || !tunableNameRE.MatchString(f[0]) {
			continue
		}
		rows[f[0]] = TunableInfo{
			Name: f[0], Current: f[1], Default: f[2], Boot: f[3], Min: f[4], Max: f[5],
			Unit: strings.TrimSpace(f[6]), Type: TunableType(strings.TrimSpace(f[7])),
		}
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("no tunables in listing %q", truncateOutput(out))
	}
	return rows, nil
}

var tunableNumberRE = regexp.MustCompile(`^(-?[0-9]+)([KMGTPE]?)(-1)?$`)

// ParseTunableValue reads a value as the tunable commands print it. Large
// values are abbreviated with binary suffixes (K=2^10 ... E=2^60) and an
// optional "-1", as in 2G-1 or 8E-1.
func ParseTunableValue(s string) (*big.Int, bool) {
	m := tunableNumberRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return nil, false
	}
	v, ok := new(big.Int).SetString(m[1], 10)
	if !ok {
		return nil, false
	}
	if m[2] != "" {
		shift := uint(10 * (strings.Index("KMGTPE", m[2]) + 1))
		v.Lsh(v, shift)
	}
	if m[3] != "" {
		v.Sub(v, big.NewInt(1))
	}
	return v, true
}

// TunableValuesEqual compares two values numerically when both parse, and
// as strings otherwise.
func TunableValuesEqual(a, b string) bool {
	x, okA := ParseTunableValue(a)
	y, okB := ParseTunableValue(b)
	if okA && okB {
		return x.Cmp(y) == 0
	}
	return strings.TrimSpace(a) == strings.TrimSpace(b)
}

// ErrUnknownTunable is returned for a name the command does not list.
var ErrUnknownTunable = errors.New("no such tunable")

// Tunables reads and sets tunables. Listings are cached per command until a
// change is made, so checking many tunables costs two commands per command.
type Tunables struct {
	Runner Runner
	cache  map[string]map[string]TunableInfo
}

// Get returns the current listing row for command's tunable name.
func (t *Tunables) Get(ctx context.Context, command, name string) (TunableInfo, error) {
	path, ok := TunableCommands[command]
	if !ok {
		return TunableInfo{}, fmt.Errorf("unsupported tunable command %q", command)
	}
	if t.cache == nil {
		t.cache = map[string]map[string]TunableInfo{}
	}
	rows, ok := t.cache[command]
	if !ok {
		// -x alone lists the tunables that are not restricted; -F -x lists
		// all of them.
		out, err := t.Runner.Run(ctx, path, "-x")
		if err != nil {
			return TunableInfo{}, fmt.Errorf("listing %s tunables: %w", command, err)
		}
		open, err := ParseTunableListing(out)
		if err != nil {
			return TunableInfo{}, fmt.Errorf("listing %s tunables: %w", command, err)
		}
		out, err = t.Runner.Run(ctx, path, "-F", "-x")
		if err != nil {
			return TunableInfo{}, fmt.Errorf("listing %s tunables: %w", command, err)
		}
		all, err := ParseTunableListing(out)
		if err != nil {
			return TunableInfo{}, fmt.Errorf("listing %s tunables: %w", command, err)
		}
		for n, row := range all {
			if _, listed := open[n]; !listed {
				row.Restricted = true
				all[n] = row
			}
		}
		rows = all
		t.cache[command] = rows
	}
	row, ok := rows[name]
	if !ok {
		return TunableInfo{}, fmt.Errorf("%s %s: %w", command, name, ErrUnknownTunable)
	}
	return row, nil
}

// SetNow changes the current and next-boot value (-p).
func (t *Tunables) SetNow(ctx context.Context, command, name, value string) error {
	if err := ValidateTunable(command, name, value); err != nil {
		return err
	}
	delete(t.cache, command)
	_, err := t.Runner.Run(ctx, TunableCommands[command], "-p", "-o", name+"="+value)
	return err
}

// SetReboot changes only the next-boot value (-r). For Bosboot tunables the
// command offers to run bosboot; the answer "n" is supplied on stdin so the
// caller runs bosboot itself and sees its result.
func (t *Tunables) SetReboot(ctx context.Context, command, name, value string) error {
	if err := ValidateTunable(command, name, value); err != nil {
		return err
	}
	delete(t.cache, command)
	_, err := t.Runner.RunWithInput(ctx, []byte("n\n"), TunableCommands[command], "-r", "-o", name+"="+value)
	return err
}

// Bosboot rebuilds the boot image on the boot device, which Bosboot-type
// tunables need before the next boot.
func Bosboot(ctx context.Context, r Runner) error {
	_, err := r.Run(ctx, CmdBosboot, "-ad", "/dev/ipldevice")
	return err
}

func truncateOutput(out []byte) string {
	s := strings.TrimSpace(string(out))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
