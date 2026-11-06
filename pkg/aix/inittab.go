package aix

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// InittabEntry is one /etc/inittab record: Identifier:RunLevel:Action:Command.
type InittabEntry struct {
	Identifier string `json:"identifier"`
	RunLevel   string `json:"runLevel"`
	Action     string `json:"action"`
	Command    string `json:"command"`
}

// String renders the entry in the form mkitab and chitab take.
func (e InittabEntry) String() string {
	return e.Identifier + ":" + e.RunLevel + ":" + e.Action + ":" + e.Command
}

// InittabActions are the actions init(8) accepts.
var InittabActions = []string{"boot", "bootwait", "hold", "initdefault", "off", "once", "ondemand", "powerfail", "powerwait", "respawn", "sysinit", "wait"}

var (
	// inittabIDRE: identifiers are at most 14 characters. Letters, digits,
	// '_' and '-' (not leading, so it never reads as an option) keep them
	// from parsing as field separators.
	inittabIDRE = regexp.MustCompile(InittabIDPattern)
	// inittabRunLevelRE: run levels 0-9, a-c, s/S and m/M (single user).
	inittabRunLevelRE = regexp.MustCompile(InittabRunLevelPattern)
)

// Patterns for inittab fields; the CRD schema uses the same patterns.
const (
	InittabIDPattern       = `^[A-Za-z0-9_][A-Za-z0-9_-]{0,13}$`
	InittabRunLevelPattern = `^[0-9a-cA-CsSmM]{1,20}$`
)

// ValidateInittabEntry checks every field of e before it reaches mkitab.
func ValidateInittabEntry(e InittabEntry) error {
	if !inittabIDRE.MatchString(e.Identifier) {
		return fmt.Errorf("invalid inittab identifier %q (1-14 letters, digits, _ or -)", e.Identifier)
	}
	if !inittabRunLevelRE.MatchString(e.RunLevel) {
		return fmt.Errorf("inittab %s: invalid run level %q", e.Identifier, e.RunLevel)
	}
	ok := false
	for _, a := range InittabActions {
		ok = ok || a == e.Action
	}
	if !ok {
		return fmt.Errorf("inittab %s: invalid action %q", e.Identifier, e.Action)
	}
	if e.Command == "" || strings.ContainsAny(e.Command, "\n\r\x00") {
		return fmt.Errorf("inittab %s: the command must be one non-empty line", e.Identifier)
	}
	return nil
}

// ErrInittabEntryNotFound is returned when lsitab finds no such entry.
var ErrInittabEntryNotFound = errors.New("no such inittab entry")

// ParseLsitab parses `lsitab <id>` output. The command is the rest of the
// line, so it may itself contain colons.
func ParseLsitab(out []byte) (InittabEntry, error) {
	line := strings.TrimRight(strings.SplitN(string(out), "\n", 2)[0], "\r")
	parts := strings.SplitN(line, ":", 4)
	if len(parts) != 4 || parts[0] == "" {
		return InittabEntry{}, fmt.Errorf("unexpected lsitab output %q", strings.TrimSpace(string(out)))
	}
	return InittabEntry{Identifier: parts[0], RunLevel: parts[1], Action: parts[2], Command: parts[3]}, nil
}

// Inittab manages individual /etc/inittab entries with lsitab, mkitab,
// chitab and rmitab. It never edits the file directly.
type Inittab struct{ Runner Runner }

// Get returns the entry with identifier id, or ErrInittabEntryNotFound.
func (t Inittab) Get(ctx context.Context, id string) (InittabEntry, error) {
	if !inittabIDRE.MatchString(id) {
		return InittabEntry{}, fmt.Errorf("invalid inittab identifier %q", id)
	}
	out, err := t.Runner.Run(ctx, CmdLsitab, id)
	if err != nil {
		// lsitab exits non-zero with no output when the entry is missing.
		var ce *CommandError
		if errors.As(err, &ce) && strings.TrimSpace(string(out)) == "" {
			return InittabEntry{}, ErrInittabEntryNotFound
		}
		return InittabEntry{}, err
	}
	if strings.TrimSpace(string(out)) == "" {
		return InittabEntry{}, ErrInittabEntryNotFound
	}
	return ParseLsitab(out)
}

// Set creates or changes e (mkitab or chitab).
func (t Inittab) Set(ctx context.Context, e InittabEntry) error {
	if err := ValidateInittabEntry(e); err != nil {
		return err
	}
	_, err := t.Get(ctx, e.Identifier)
	switch {
	case errors.Is(err, ErrInittabEntryNotFound):
		_, err = t.Runner.Run(ctx, CmdMkitab, e.String())
	case err == nil:
		_, err = t.Runner.Run(ctx, CmdChitab, e.String())
	}
	return err
}

// Remove deletes the entry with identifier id; a missing entry is not an error.
func (t Inittab) Remove(ctx context.Context, id string) error {
	if _, err := t.Get(ctx, id); errors.Is(err, ErrInittabEntryNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	_, err := t.Runner.Run(ctx, CmdRmitab, id)
	return err
}
