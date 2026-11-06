package aix

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"
)

// Subsystem communication methods as mkssys and lssrc -S spell them.
const (
	ContactSignal = "signal" // -S
	ContactSocket = "socket" // -K
)

// SubsystemDefinition is the part of an SRC subsystem definition AMCO
// manages, in mkssys terms.
type SubsystemDefinition struct {
	Path      string `json:"path"`                // -p
	Arguments string `json:"arguments,omitempty"` // -a
	UID       int    `json:"uid"`                 // -u
	Stdin     string `json:"stdin,omitempty"`     // -i
	Stdout    string `json:"stdout,omitempty"`    // -o
	Stderr    string `json:"stderr,omitempty"`    // -e
	Respawn   bool   `json:"respawn,omitempty"`   // -R (true) or -O
	Multiple  bool   `json:"multiple,omitempty"`  // -q (true) or -Q
	Contact   string `json:"contact,omitempty"`   // ContactSignal (-S) or ContactSocket (-K)
	// StopSignal and ForceSignal are only used with ContactSignal (-n, -f).
	StopSignal  int    `json:"stopSignal,omitempty"`
	ForceSignal int    `json:"forceSignal,omitempty"`
	WaitTime    int    `json:"waitTime,omitempty"` // -w, seconds
	Group       string `json:"group,omitempty"`    // -G
}

// WithDefaults fills unset fields with mkssys's defaults, so a definition
// written with only the interesting fields compares equal to what lssrc -S
// reports for it.
func (d SubsystemDefinition) WithDefaults() SubsystemDefinition {
	for _, f := range []*string{&d.Stdin, &d.Stdout, &d.Stderr} {
		if *f == "" {
			*f = "/dev/console"
		}
	}
	if d.Contact == "" {
		d.Contact = ContactSignal
	}
	if d.Contact == ContactSignal {
		if d.StopSignal == 0 {
			d.StopSignal = 15
		}
		if d.ForceSignal == 0 {
			d.ForceSignal = 9
		}
	} else {
		d.StopSignal, d.ForceSignal = 0, 0
	}
	if d.WaitTime == 0 {
		d.WaitTime = 20
	}
	return d
}

// Validate checks a definition before it reaches mkssys or chssys.
func (d SubsystemDefinition) Validate() error {
	if !path.IsAbs(d.Path) || path.Clean(d.Path) != d.Path {
		return fmt.Errorf("subsystem program %q must be an absolute, clean path", d.Path)
	}
	if strings.ContainsAny(d.Arguments, ":\n\r\x00") {
		return fmt.Errorf("subsystem arguments may not contain ':' or line breaks")
	}
	for _, f := range []string{d.Stdin, d.Stdout, d.Stderr} {
		if f != "" && (!path.IsAbs(f) || strings.ContainsAny(f, ":\n\x00")) {
			return fmt.Errorf("subsystem standard stream %q must be an absolute path", f)
		}
	}
	if d.UID < 0 {
		return fmt.Errorf("subsystem uid %d is invalid", d.UID)
	}
	if d.Contact != "" && d.Contact != ContactSignal && d.Contact != ContactSocket {
		return fmt.Errorf("subsystem contact %q must be %s or %s", d.Contact, ContactSignal, ContactSocket)
	}
	for _, sig := range []int{d.StopSignal, d.ForceSignal} {
		if sig < 0 || sig > 64 {
			return fmt.Errorf("signal %d is out of range", sig)
		}
	}
	if d.WaitTime < 0 || d.WaitTime > 3600 {
		return fmt.Errorf("subsystem wait time %d is out of range", d.WaitTime)
	}
	if d.Group != "" && !subsystemNameRE.MatchString(d.Group) {
		return fmt.Errorf("invalid SRC group name %q", d.Group)
	}
	return nil
}

// args returns the mkssys/chssys flags that set d (without -s).
func (d SubsystemDefinition) args(forChange bool) []string {
	d = d.WithDefaults()
	a := []string{"-p", d.Path, "-u", strconv.Itoa(d.UID)}
	if d.Arguments != "" || forChange {
		a = append(a, "-a", d.Arguments)
	}
	a = append(a, "-i", d.Stdin, "-o", d.Stdout, "-e", d.Stderr)
	if d.Respawn {
		a = append(a, "-R")
	} else {
		a = append(a, "-O")
	}
	if d.Multiple {
		a = append(a, "-q")
	} else {
		a = append(a, "-Q")
	}
	if d.Contact == ContactSocket {
		a = append(a, "-K")
	} else {
		a = append(a, "-S", "-n", strconv.Itoa(d.StopSignal), "-f", strconv.Itoa(d.ForceSignal))
	}
	a = append(a, "-w", strconv.Itoa(d.WaitTime))
	if d.Group != "" || forChange {
		a = append(a, "-G", d.Group)
	}
	return a
}

// ParseLssrcDefinition parses `lssrc -S -s <name>`: a header line naming the
// SRCsubsys fields (starting with '#') and one line of colon-separated
// values, with action, multi and contact shown as mkssys flags, e.g.
//
//	#subsysname:synonym:cmdargs:path:uid:auditid:standin:standout:standerr:action:multi:contact:svrkey:svrmtype:priority:signorm:sigforce:display:waittime:grpname:
//	sshd:::/usr/sbin/sshd:0:0:/dev/console:/dev/console:/dev/console:-O:-Q:-S:0:0:20:15:9:-d:20:ssh:
func ParseLssrcDefinition(out []byte) (SubsystemDefinition, error) {
	if hasMessageCode(out, msgSubsystemNotOnFile) {
		return SubsystemDefinition{}, ErrSubsystemNotFound
	}
	var header, values []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "#"):
			header = strings.Split(strings.TrimPrefix(line, "#"), ":")
		case strings.TrimSpace(line) != "" && header != nil && values == nil:
			values = strings.Split(line, ":")
		}
	}
	if header == nil || values == nil {
		return SubsystemDefinition{}, fmt.Errorf("unexpected lssrc -S output %q", truncateOutput(out))
	}
	f := map[string]string{}
	for i, name := range header {
		if name != "" && i < len(values) {
			f[name] = values[i]
		}
	}
	num := func(k string) (int, error) {
		if f[k] == "" {
			return 0, nil
		}
		n, err := strconv.Atoi(f[k])
		if err != nil {
			return 0, fmt.Errorf("lssrc -S field %s: %q is not a number", k, f[k])
		}
		return n, nil
	}
	d := SubsystemDefinition{
		Path: f["path"], Arguments: f["cmdargs"], Stdin: f["standin"], Stdout: f["standout"], Stderr: f["standerr"],
		Group: f["grpname"],
	}
	var err error
	if d.UID, err = num("uid"); err != nil {
		return d, err
	}
	if d.WaitTime, err = num("waittime"); err != nil {
		return d, err
	}
	if d.StopSignal, err = num("signorm"); err != nil {
		return d, err
	}
	if d.ForceSignal, err = num("sigforce"); err != nil {
		return d, err
	}
	switch f["action"] {
	case "-R":
		d.Respawn = true
	case "-O":
	default:
		return d, fmt.Errorf("lssrc -S action %q is neither -R nor -O", f["action"])
	}
	switch f["multi"] {
	case "-q":
		d.Multiple = true
	case "-Q":
	default:
		return d, fmt.Errorf("lssrc -S multi %q is neither -q nor -Q", f["multi"])
	}
	switch f["contact"] {
	case "-S":
		d.Contact = ContactSignal
	case "-K":
		d.Contact = ContactSocket
		d.StopSignal, d.ForceSignal = 0, 0
	default:
		// Message-queue (-I) subsystems are not managed by AMCO; report
		// the value so a comparison never matches.
		d.Contact = f["contact"]
	}
	return d, nil
}

// Definition returns a subsystem's SRC definition.
func (s *SRC) Definition(ctx context.Context, name string) (SubsystemDefinition, error) {
	if err := ValidateSubsystemName(name); err != nil {
		return SubsystemDefinition{}, err
	}
	out, err := s.Runner.Run(ctx, CmdLssrc, "-S", "-s", name)
	if hasMessageCode(out, msgSubsystemNotOnFile) {
		return SubsystemDefinition{}, fmt.Errorf("%s: %w", name, ErrSubsystemNotFound)
	}
	if err != nil {
		return SubsystemDefinition{}, err
	}
	return ParseLssrcDefinition(out)
}

// Define creates a subsystem (mkssys).
func (s *SRC) Define(ctx context.Context, name string, d SubsystemDefinition) error {
	if err := ValidateSubsystemName(name); err != nil {
		return err
	}
	if err := d.Validate(); err != nil {
		return err
	}
	_, err := s.Runner.Run(ctx, CmdMkssys, append([]string{"-s", name}, d.args(false)...)...)
	return err
}

// Redefine changes an existing subsystem's definition (chssys). A running
// subsystem keeps its old definition until it is restarted.
func (s *SRC) Redefine(ctx context.Context, name string, d SubsystemDefinition) error {
	if err := ValidateSubsystemName(name); err != nil {
		return err
	}
	if err := d.Validate(); err != nil {
		return err
	}
	_, err := s.Runner.Run(ctx, CmdChssys, append([]string{"-s", name}, d.args(true)...)...)
	return err
}

// Undefine removes a subsystem (rmssys). It must be stopped first.
func (s *SRC) Undefine(ctx context.Context, name string) error {
	if err := ValidateSubsystemName(name); err != nil {
		return err
	}
	_, err := s.Runner.Run(ctx, CmdRmssys, "-s", name)
	return err
}
