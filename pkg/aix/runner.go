// Package aix contains the AIX host operations used by the AMCO node agent.
//
// Every AIX facility is reached by shelling out to the standard AIX command at
// its absolute path through a single Runner, so all calls are logged in one
// place and the parsing logic can be unit-tested on any OS with a fake Runner.
// Nothing in this package is build-tagged.
package aix

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"k8s.io/klog/v2"
)

// Absolute paths of the AIX commands this package invokes. Nothing else is
// ever executed, and never through a shell.
const (
	CmdStartsrc = "/usr/bin/startsrc"
	CmdStopsrc  = "/usr/bin/stopsrc"
	CmdLssrc    = "/usr/bin/lssrc"
	CmdRefresh  = "/usr/bin/refresh"
	CmdKill     = "/usr/bin/kill"
	CmdMkssys   = "/usr/bin/mkssys"
	CmdChssys   = "/usr/bin/chssys"
	CmdRmssys   = "/usr/bin/rmssys"
	CmdOslevel  = "/usr/bin/oslevel"
	CmdWho      = "/usr/bin/who"
	CmdLsitab   = "/usr/sbin/lsitab"
	CmdMkitab   = "/usr/sbin/mkitab"
	CmdChitab   = "/usr/sbin/chitab"
	CmdRmitab   = "/usr/sbin/rmitab"
	CmdLsdev    = "/usr/sbin/lsdev"
	CmdLsattr   = "/usr/sbin/lsattr"
	CmdChdev    = "/usr/sbin/chdev"
	CmdBosboot  = "/usr/sbin/bosboot"
)

// Runner executes an AIX command and returns its combined stdout and stderr.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	// RunWithInput is Run with the given bytes connected to the command's stdin.
	RunWithInput(ctx context.Context, input []byte, name string, args ...string) ([]byte, error)
}

// CommandError is returned by ExecRunner when a command fails. It keeps the
// output so callers can match AIX message codes such as 0513-085.
type CommandError struct {
	Name   string
	Args   []string
	Output []byte
	Err    error
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("%s %s: %v: %s", e.Name, strings.Join(e.Args, " "), e.Err, strings.TrimSpace(string(e.Output)))
}

func (e *CommandError) Unwrap() error { return e.Err }

// ExecRunner runs commands on the local host with a C locale, so that the
// output parsed by this package is not translated, and in UTC, so that the
// times it prints (who -b) do not depend on the host's time zone.
type ExecRunner struct{}

// Run implements Runner.
func (r ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return r.RunWithInput(ctx, nil, name, args...)
}

// RunWithInput implements Runner.
func (ExecRunner) RunWithInput(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
	klog.V(2).Infof("aix: running %s %s", name, strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // fixed absolute command paths
	cmd.Env = append(os.Environ(), "LANG=C", "LC_ALL=C", "TZ=UTC0")
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, &CommandError{Name: name, Args: args, Output: out, Err: err}
	}
	return out, nil
}

// hasMessageCode reports whether out contains the AIX message code (for
// example "0513-085"). AIX prefixes its diagnostics with these stable codes,
// which survive locale changes better than the message text.
func hasMessageCode(out []byte, code string) bool {
	return bytes.Contains(out, []byte(code))
}
