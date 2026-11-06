package agent

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gilbahat/aix-machine-config-operator/pkg/aix"
	"github.com/gilbahat/aix-machine-config-operator/pkg/rendered"
)

func countPrefix(list []string, prefix string) int {
	n := 0
	for _, l := range list {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

// Regression (second review): two failed applies in a row orphan the items of the first one: they
// keep AMCO's value forever and their baselines are never used.
func TestRegressionPendingOverwriteOrphansItems(t *testing.T) {
	e := newEnv(t)
	e.h.tunable("no", "tcp_sendspace", "16384", aix.TunableConnect)
	e.h.device("fcs0", "num_cmd_elems", "500")
	e.h.device("fcs1", "num_cmd_elems", "500")
	tun := rendered.Tunable{Command: "no", Name: "tcp_sendspace", Value: "262144"}

	_, err := e.roll(&rendered.Config{Tunables: []rendered.Tunable{tun}})
	require.NoError(t, err)

	// B changes fcs0 and fails afterwards (kubelet restart fails).
	e.h.startErr["kubelet"] = errors.New("start timed out")
	_, err = e.roll(&rendered.Config{Files: []rendered.File{kubeletConf}, Tunables: []rendered.Tunable{tun},
		DeviceAttributes: []rendered.DeviceAttribute{{Device: "fcs0", Attribute: "num_cmd_elems", Value: "2048"}}})
	require.Error(t, err)
	delete(e.h.startErr, "kubelet")
	require.Equal(t, "2048", e.h.devices["fcs0"]["num_cmd_elems"].running)

	// C (without B's items) fails in its apply loop.
	e.h.busy["fcs1"] = true
	_, err = e.roll(&rendered.Config{Tunables: []rendered.Tunable{tun},
		DeviceAttributes: []rendered.DeviceAttribute{{Device: "fcs1", Attribute: "num_cmd_elems", Value: "1024", OnBusy: rendered.OnBusyFail}}})
	require.Error(t, err)

	// D is good.
	_, err = e.roll(&rendered.Config{Tunables: []rendered.Tunable{tun}})
	require.NoError(t, err)
	st, err := e.a.Host().LoadState()
	require.NoError(t, err)
	t.Logf("fcs0 after D: %s; leftover baselines: %v; kubelet.conf exists: %v",
		e.h.devices["fcs0"]["num_cmd_elems"].running, keys(st.Baselines), e.exists(kubeletConf.Path))
	assert.Equal(t, "500", e.h.devices["fcs0"]["num_cmd_elems"].running, "fcs0 should be restored")
}

func keys(m map[string]Baseline) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Regression (second review): a failed bosboot is retried on every reconcile when the previous
// config had the same tunable (Drift on the old config -> full).
func TestRegressionBosbootRetriedViaDrift(t *testing.T) {
	e := newEnv(t)
	e.h.tunable("vmo", "kernel_heap_psize", "4096", aix.TunableBosboot)
	_, err := e.roll(&rendered.Config{Tunables: []rendered.Tunable{{Command: "vmo", Name: "kernel_heap_psize", Value: "65536"}}})
	require.NoError(t, err)

	e.h.bosbootErr = errors.New("0301-150 bosboot: Invalid or no boot device specified")
	_, err = e.roll(&rendered.Config{Tunables: []rendered.Tunable{{Command: "vmo", Name: "kernel_heap_psize", Value: "16777216"}}})
	require.ErrorContains(t, err, "bosboot failed")

	e.h.resetCalls()
	for i := 0; i < 3; i++ {
		_ = e.reconcile()
	}
	n := countPrefix(e.h.mutating, aix.CmdBosboot)
	t.Logf("bosboot runs in 3 retries: %d; mutating: %v", n, e.h.mutating)
	assert.Zero(t, n, "a failed bosboot must not be retried automatically")
}

// Regression (second review): an item skipped as ReprovisionRequired gets no baseline; once it can
// converge it is changed, and removing it never restores it.
func TestRegressionReprovisionedItemGetsNoBaseline(t *testing.T) {
	e := newEnv(t)
	e.h.tunable("no", "tcp_sendspace", "16384", aix.TunableConnect)
	dev := rendered.DeviceAttribute{Device: "fcs1", Attribute: "num_cmd_elems", Value: "2048"}
	_, err := e.roll(&rendered.Config{DeviceAttributes: []rendered.DeviceAttribute{dev}})
	require.NoError(t, err)

	e.h.device("fcs1", "num_cmd_elems", "500") // adapter added (DLPAR)
	_, err = e.roll(&rendered.Config{DeviceAttributes: []rendered.DeviceAttribute{dev},
		Tunables: []rendered.Tunable{{Command: "no", Name: "tcp_sendspace", Value: "262144"}}})
	require.NoError(t, err)
	require.Equal(t, "2048", e.h.devices["fcs1"]["num_cmd_elems"].running)

	_, err = e.roll(&rendered.Config{})
	require.NoError(t, err)
	assert.Equal(t, "500", e.h.devices["fcs1"]["num_cmd_elems"].running, "fcs1 should be restored to its original value")
}

// Regression (second review): a failed restart during a removal leaves the service down for good.
func TestRegressionRestoreRestartLost(t *testing.T) {
	e := newEnv(t)
	e.h.defineSubsystem("appd", aix.StateActive)
	e.write("/etc/app.conf", "orig\n")
	f := rendered.File{Path: "/etc/app.conf", Mode: 0o644, Contents: "amco\n", Origin: rendered.OriginUser,
		Actions: []rendered.SubsystemAction{{Subsystem: "appd", Action: rendered.ActionRestart}}}
	_, err := e.roll(files(f))
	require.NoError(t, err)

	e.h.startErr["appd"] = errors.New("start timed out")
	_, err = e.roll(&rendered.Config{})
	require.Error(t, err)
	delete(e.h.startErr, "appd")

	for i := 0; i < 3; i++ {
		_ = e.reconcile()
	}
	st, _ := e.a.Host().LoadState()
	t.Logf("appd state: %s, Restarting: %v", e.h.subsystems["appd"].state, st.Restarting)
	assert.Equal(t, aix.StateActive, e.h.subsystems["appd"].state, "appd should be started again")
}

// Regression (second review): a baseline value the validators reject (empty attribute) wedges every
// later apply.
func TestRegressionUnrestorableBaselineWedges(t *testing.T) {
	e := newEnv(t)
	e.h.device("en0", "netaddr", "")
	dev := rendered.DeviceAttribute{Device: "en0", Attribute: "netaddr", Value: "10.0.0.1"}
	_, err := e.roll(&rendered.Config{DeviceAttributes: []rendered.DeviceAttribute{dev}})
	require.NoError(t, err)
	_, err = e.roll(&rendered.Config{})
	t.Logf("removal: %v", err)
	assert.NoError(t, err)
	_, err = e.roll(&rendered.Config{Files: []rendered.File{quayHosts}})
	t.Logf("next unrelated config: %v", err)
	assert.NoError(t, err)
}

// Regression (second review): removing the documented 80-app-agent example (subsystem defined by
// AMCO + config file with onChange Restart) fails the first attempt.
func TestRegressionRemovingAppExampleDegrades(t *testing.T) {
	e := newEnv(t)
	f := rendered.File{Path: "/etc/appagent.conf", Mode: 0o644, Contents: "x\n", Origin: rendered.OriginUser,
		Actions: []rendered.SubsystemAction{{Subsystem: "appagent", Action: rendered.ActionRestart}}}
	s := rendered.Subsystem{Name: "appagent", State: rendered.SubsystemActive, Definition: &aix.SubsystemDefinition{Path: "/opt/app/bin/appagent"}}
	_, err := e.roll(&rendered.Config{Files: []rendered.File{f}, Subsystems: []rendered.Subsystem{s}})
	require.NoError(t, err)
	_, err = e.roll(&rendered.Config{})
	t.Logf("removal: %v", err)
	assert.NoError(t, err)
}

// Regression (second review): who -b has no year; an agent restarted after more than a year of
// uptime computes a different boot key and clears pending items.
// Regression (second review): an item that converges after -r back to its running value stays
// reported as pending reboot.
func TestRegressionSpuriousPending(t *testing.T) {
	e := newEnv(t)
	e.h.tunable("no", "ipqmaxlen", "100", aix.TunableReboot)
	_, err := e.roll(&rendered.Config{Tunables: []rendered.Tunable{{Command: "no", Name: "ipqmaxlen", Value: "512"}}})
	require.NoError(t, err)
	_, err = e.roll(&rendered.Config{Tunables: []rendered.Tunable{{Command: "no", Name: "ipqmaxlen", Value: "100"}}})
	require.NoError(t, err)
	_ = e.reconcile()
	t.Logf("pending after reverting to the running value: %v", e.annotationList("amco.reuna-labs.io/pendingReboot"))
	assert.Empty(t, e.annotationList("amco.reuna-labs.io/pendingReboot"))
}
