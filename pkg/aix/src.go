package aix

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"k8s.io/klog/v2"
)

// SRC (System Resource Controller) message codes this package recognises.
const (
	msgSubsystemNotOnFile   = "0513-085" // The <name> Subsystem is not on file.
	msgSubsystemActive      = "0513-029" // The <name> Subsystem is already active.
	msgSubsystemInoperative = "0513-004" // The Subsystem or Group, <name>, is currently inoperative.
)

// Subsystem states as printed in the Status column of lssrc.
const (
	StateActive      = "active"
	StateInoperative = "inoperative"
	StateStopping    = "stopping"
	StateWarned      = "warned"
)

// ErrSubsystemNotFound is returned when SRC has no definition for a subsystem.
var ErrSubsystemNotFound = errors.New("SRC subsystem is not defined (see mkssys)")

// SubsystemStatus is one row of `lssrc -s <name>`.
type SubsystemStatus struct {
	Name  string
	Group string
	PID   int
	State string
}

// ParseLssrc parses the output of `lssrc -s <name>`. The PID and Group columns
// are blank for an inoperative subsystem and for subsystems without a group,
// so rows have between two and four fields.
func ParseLssrc(out []byte) (SubsystemStatus, error) {
	if hasMessageCode(out, msgSubsystemNotOnFile) {
		return SubsystemStatus{}, ErrSubsystemNotFound
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] == "Subsystem" {
			continue
		}
		if len(fields) < 2 || len(fields) > 4 {
			return SubsystemStatus{}, fmt.Errorf("unexpected lssrc row %q", line)
		}
		st := SubsystemStatus{Name: fields[0], State: fields[len(fields)-1]}
		middle := fields[1 : len(fields)-1]
		for _, f := range middle {
			if pid, err := strconv.Atoi(f); err == nil {
				st.PID = pid
			} else {
				st.Group = f
			}
		}
		return st, nil
	}
	return SubsystemStatus{}, fmt.Errorf("no subsystem row in lssrc output %q", strings.TrimSpace(string(out)))
}

// subsystemNameRE limits names to what mkssys accepts, so a name can never
// be taken for an option of an SRC command.
var subsystemNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,29}$`)

// ValidateSubsystemName rejects names SRC would not accept.
func ValidateSubsystemName(name string) error {
	if !subsystemNameRE.MatchString(name) {
		return fmt.Errorf("invalid SRC subsystem name %q", name)
	}
	return nil
}

// SRC drives AIX subsystems through startsrc, stopsrc, refresh and lssrc.
type SRC struct {
	Runner Runner
	// PollInterval is how often lssrc is polled while waiting for a state change.
	PollInterval time.Duration
	// StopTimeout is how long a normal stop may take before a forced stop.
	StopTimeout time.Duration
	// StartTimeout is how long a subsystem may take to become active.
	StartTimeout time.Duration
}

// NewSRC returns an SRC with defaults suitable for node services.
func NewSRC(r Runner) *SRC {
	return &SRC{Runner: r, PollInterval: time.Second, StopTimeout: 90 * time.Second, StartTimeout: 60 * time.Second}
}

// Status returns the current status of a subsystem.
func (s *SRC) Status(ctx context.Context, name string) (SubsystemStatus, error) {
	if err := ValidateSubsystemName(name); err != nil {
		return SubsystemStatus{}, err
	}
	out, err := s.Runner.Run(ctx, CmdLssrc, "-s", name)
	if hasMessageCode(out, msgSubsystemNotOnFile) {
		return SubsystemStatus{}, fmt.Errorf("%s: %w", name, ErrSubsystemNotFound)
	}
	if err != nil {
		return SubsystemStatus{}, err
	}
	return ParseLssrc(out)
}

// Start starts a subsystem and waits until SRC reports it active.
func (s *SRC) Start(ctx context.Context, name string) error {
	st, err := s.Status(ctx, name)
	if err != nil {
		return err
	}
	if st.State == StateActive {
		return nil
	}
	if out, err := s.Runner.Run(ctx, CmdStartsrc, "-s", name); err != nil && !hasMessageCode(out, msgSubsystemActive) {
		return fmt.Errorf("starting SRC subsystem %s: %w", name, err)
	}
	return s.waitFor(ctx, name, s.StartTimeout, func(st SubsystemStatus) bool { return st.State == StateActive })
}

// Stop stops a subsystem, escalating to a forced stop (stopsrc -f) if a normal
// stop does not complete within StopTimeout.
func (s *SRC) Stop(ctx context.Context, name string) error {
	st, err := s.Status(ctx, name)
	if err != nil {
		return err
	}
	if st.State == StateInoperative {
		return nil
	}
	inoperative := func(st SubsystemStatus) bool { return st.State == StateInoperative }
	if out, err := s.Runner.Run(ctx, CmdStopsrc, "-s", name); err != nil && !hasMessageCode(out, msgSubsystemInoperative) {
		return fmt.Errorf("stopping SRC subsystem %s: %w", name, err)
	}
	if err := s.waitFor(ctx, name, s.StopTimeout, inoperative); err == nil {
		return nil
	}
	klog.Warningf("SRC subsystem %s did not stop within %s, forcing", name, s.StopTimeout)
	if out, err := s.Runner.Run(ctx, CmdStopsrc, "-f", "-s", name); err != nil && !hasMessageCode(out, msgSubsystemInoperative) {
		return fmt.Errorf("force-stopping SRC subsystem %s: %w", name, err)
	}
	return s.waitFor(ctx, name, s.StopTimeout, inoperative)
}

// Restart stops (if running) and starts a subsystem; the systemctl restart
// equivalent.
func (s *SRC) Restart(ctx context.Context, name string) error {
	if err := s.Stop(ctx, name); err != nil {
		return err
	}
	return s.Start(ctx, name)
}

// Reload asks a running subsystem to re-read its configuration. Socket and
// message subsystems are sent `refresh`; subsystems that only support signal
// communication reject refresh, so they are sent SIGHUP directly, which is the
// usual daemon convention and what systemd's ExecReload does for most units.
func (s *SRC) Reload(ctx context.Context, name string) error {
	st, err := s.Status(ctx, name)
	if err != nil {
		return err
	}
	if st.State != StateActive {
		return fmt.Errorf("cannot reload SRC subsystem %s: it is %s", name, st.State)
	}
	out, refreshErr := s.Runner.Run(ctx, CmdRefresh, "-s", name)
	if refreshErr == nil {
		return nil
	}
	if st.PID <= 0 {
		return fmt.Errorf("refreshing SRC subsystem %s: %w (and no PID to signal): %s", name, refreshErr, strings.TrimSpace(string(out)))
	}
	klog.Infof("refresh not supported by SRC subsystem %s, sending SIGHUP to PID %d", name, st.PID)
	if _, err := s.Runner.Run(ctx, CmdKill, "-HUP", strconv.Itoa(st.PID)); err != nil {
		return fmt.Errorf("sending SIGHUP to SRC subsystem %s (PID %d): %w", name, st.PID, err)
	}
	return nil
}

func (s *SRC) waitFor(ctx context.Context, name string, timeout time.Duration, done func(SubsystemStatus) bool) error {
	deadline := time.Now().Add(timeout)
	var last SubsystemStatus
	for {
		st, err := s.Status(ctx, name)
		if err != nil {
			return err
		}
		if done(st) {
			return nil
		}
		last = st
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for SRC subsystem %s (state %s)", timeout, name, last.State)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.PollInterval):
		}
	}
}
