package aix

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRunner returns scripted results keyed by the full command line and
// records every call.
type fakeRunner struct {
	responses map[string][]response
	calls     []string
	inputs    []string
}

type response struct {
	out string
	err error
}

func newFakeRunner() *fakeRunner { return &fakeRunner{responses: map[string][]response{}} }

func (f *fakeRunner) on(cmdline, out string, err error) *fakeRunner {
	f.responses[cmdline] = append(f.responses[cmdline], response{out, err})
	return f
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f.RunWithInput(ctx, nil, name, args...)
}

func (f *fakeRunner) RunWithInput(_ context.Context, input []byte, name string, args ...string) ([]byte, error) {
	key := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, key)
	if input != nil {
		f.inputs = append(f.inputs, string(input))
	}
	rs, ok := f.responses[key]
	if !ok || len(rs) == 0 {
		return nil, fmt.Errorf("unexpected command %q", key)
	}
	r := rs[0]
	if len(rs) > 1 { // the last response repeats
		f.responses[key] = rs[1:]
	}
	return []byte(r.out), r.err
}

var errExit = errors.New("exit status 1")

const lssrcHeader = "Subsystem         Group            PID          Status \n"

func TestParseLssrc(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want SubsystemStatus
		err  error
	}{
		{"active with group", lssrcHeader + " sshd             tcpip            4194678      active\n",
			SubsystemStatus{Name: "sshd", Group: "tcpip", PID: 4194678, State: StateActive}, nil},
		{"inoperative with group", lssrcHeader + " kubelet          kubernetes                    inoperative\n",
			SubsystemStatus{Name: "kubelet", Group: "kubernetes", State: StateInoperative}, nil},
		{"active without group", lssrcHeader + " amco-agent                               5111            active\n",
			SubsystemStatus{Name: "amco-agent", PID: 5111, State: StateActive}, nil},
		{"inoperative without group", lssrcHeader + " amco-agent                                             inoperative\n",
			SubsystemStatus{Name: "amco-agent", State: StateInoperative}, nil},
		{"not on file", "0513-085 The foo Subsystem is not on file.\n", SubsystemStatus{}, ErrSubsystemNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseLssrc([]byte(c.out))
			if c.err != nil {
				assert.ErrorIs(t, err, c.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}
	_, err := ParseLssrc([]byte(lssrcHeader))
	assert.Error(t, err)
}

func testSRC(r Runner) *SRC {
	return &SRC{Runner: r, PollInterval: time.Millisecond, StopTimeout: 20 * time.Millisecond, StartTimeout: 20 * time.Millisecond}
}

func TestSRCRestart(t *testing.T) {
	r := newFakeRunner().
		on("/usr/bin/lssrc -s containerd", lssrcHeader+" containerd kubernetes 42 active\n", nil).
		on("/usr/bin/lssrc -s containerd", lssrcHeader+" containerd kubernetes 42 stopping\n", nil).
		on("/usr/bin/lssrc -s containerd", lssrcHeader+" containerd kubernetes inoperative\n", nil).
		on("/usr/bin/lssrc -s containerd", lssrcHeader+" containerd kubernetes inoperative\n", nil).
		on("/usr/bin/lssrc -s containerd", lssrcHeader+" containerd kubernetes 77 active\n", nil).
		on("/usr/bin/stopsrc -s containerd", "0513-044 The containerd Subsystem was requested to stop.\n", nil).
		on("/usr/bin/startsrc -s containerd", "0513-059 The containerd Subsystem has been started. Subsystem PID is 77.\n", nil)
	require.NoError(t, testSRC(r).Restart(context.Background(), "containerd"))
	assert.Contains(t, r.calls, "/usr/bin/stopsrc -s containerd")
	assert.Contains(t, r.calls, "/usr/bin/startsrc -s containerd")
	assert.NotContains(t, r.calls, "/usr/bin/stopsrc -f -s containerd")
}

func TestSRCStopEscalatesToForce(t *testing.T) {
	r := newFakeRunner().
		on("/usr/bin/lssrc -s kubelet", lssrcHeader+" kubelet kubernetes 42 active\n", nil).
		on("/usr/bin/lssrc -s kubelet", lssrcHeader+" kubelet kubernetes 42 stopping\n", nil).
		on("/usr/bin/stopsrc -s kubelet", "", nil).
		on("/usr/bin/stopsrc -f -s kubelet", "", nil)
	// The scripted lssrc never reports inoperative: the normal stop times out,
	// the daemon escalates to stopsrc -f, and that wait times out too.
	err := testSRC(r).Stop(context.Background(), "kubelet")
	require.Error(t, err)
	assert.Contains(t, r.calls, "/usr/bin/stopsrc -f -s kubelet")
}

func TestSRCForcedStopSucceeds(t *testing.T) {
	r := newFakeRunner().
		on("/usr/bin/lssrc -s kubelet", lssrcHeader+" kubelet kubernetes 42 active\n", nil).
		on("/usr/bin/lssrc -s kubelet", lssrcHeader+" kubelet kubernetes 42 stopping\n", nil).
		on("/usr/bin/stopsrc -s kubelet", "", nil).
		on("/usr/bin/stopsrc -f -s kubelet", "", nil)
	s := testSRC(r)
	s.StopTimeout = 5 * time.Millisecond
	// Once stopsrc -f has been issued, lssrc reports inoperative.
	stopped := false
	wrapped := &hookRunner{Runner: r, after: func(cmd string) {
		if cmd == "/usr/bin/stopsrc -f -s kubelet" && !stopped {
			stopped = true
			r.responses["/usr/bin/lssrc -s kubelet"] = []response{{lssrcHeader + " kubelet kubernetes inoperative\n", nil}}
		}
	}}
	s.Runner = wrapped
	require.NoError(t, s.Stop(context.Background(), "kubelet"))
}

type hookRunner struct {
	Runner
	after func(cmd string)
}

func (h *hookRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := h.Runner.Run(ctx, name, args...)
	h.after(strings.Join(append([]string{name}, args...), " "))
	return out, err
}

func TestSRCStartAlreadyActive(t *testing.T) {
	r := newFakeRunner().on("/usr/bin/lssrc -s kubelet", lssrcHeader+" kubelet kubernetes 42 active\n", nil)
	require.NoError(t, testSRC(r).Start(context.Background(), "kubelet"))
	assert.NotContains(t, r.calls, "/usr/bin/startsrc -s kubelet")
}

func TestSRCMissingSubsystem(t *testing.T) {
	r := newFakeRunner().on("/usr/bin/lssrc -s nope", "0513-085 The nope Subsystem is not on file.\n", errExit)
	err := testSRC(r).Restart(context.Background(), "nope")
	assert.ErrorIs(t, err, ErrSubsystemNotFound)
}

func TestSRCReloadFallsBackToSIGHUP(t *testing.T) {
	r := newFakeRunner().
		on("/usr/bin/lssrc -s sshd", lssrcHeader+" sshd tcpip 4194678 active\n", nil).
		on("/usr/bin/refresh -s sshd", "0513-005 The Subsystem, sshd, only supports signal communication.\n", errExit).
		on("/usr/bin/kill -HUP 4194678", "", nil)
	require.NoError(t, testSRC(r).Reload(context.Background(), "sshd"))
	assert.Contains(t, r.calls, "/usr/bin/kill -HUP 4194678")
}

func TestSRCReloadInoperative(t *testing.T) {
	r := newFakeRunner().on("/usr/bin/lssrc -s sshd", lssrcHeader+" sshd tcpip inoperative\n", nil)
	assert.Error(t, testSRC(r).Reload(context.Background(), "sshd"))
}

func TestValidateSubsystemName(t *testing.T) {
	assert.NoError(t, ValidateSubsystemName("containerd"))
	assert.NoError(t, ValidateSubsystemName("rpc.mountd"))
	assert.Error(t, ValidateSubsystemName("-a"))
	assert.Error(t, ValidateSubsystemName("a b"))
	assert.Error(t, ValidateSubsystemName(strings.Repeat("x", 31)))
}

func TestParseOSLevel(t *testing.T) {
	lvl, err := ParseOSLevel([]byte("7300-02-01-2346\n"))
	require.NoError(t, err)
	assert.Equal(t, "7.3", lvl.Version)
	assert.Equal(t, "02", lvl.TL)
	assert.NoError(t, lvl.CheckSupported())

	old, err := ParseOSLevel([]byte("7100-05-06-2015"))
	require.NoError(t, err)
	assert.Error(t, old.CheckSupported())

	_, err = ParseOSLevel([]byte("7.3.0.0"))
	assert.Error(t, err)
}
