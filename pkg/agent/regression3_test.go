package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gilbahat/aix-machine-config-operator/pkg/aix"
	"github.com/gilbahat/aix-machine-config-operator/pkg/rendered"
)

// Regression (second review): chssys queues a restart in memory only; if a later item fails, the
// retry sees the definition in sync and never restarts the subsystem.
func TestRegressionRedefineRestartLost(t *testing.T) {
	e := newEnv(t)
	e.h.defineSubsystem("appd", aix.StateActive)
	e.h.device("fcs0", "num_cmd_elems", "500")
	e.h.busy["fcs0"] = true
	cfg := &rendered.Config{
		Subsystems:       []rendered.Subsystem{{Name: "appd", State: rendered.SubsystemActive, Definition: &aix.SubsystemDefinition{Path: "/usr/sbin/appd", Arguments: "-v"}}},
		DeviceAttributes: []rendered.DeviceAttribute{{Device: "fcs0", Attribute: "num_cmd_elems", Value: "2048", OnBusy: rendered.OnBusyFail}},
	}
	name, err := e.roll(cfg)
	require.Error(t, err)
	require.Equal(t, "-v", e.h.subsystems["appd"].def.Arguments, "chssys ran")
	e.h.busy["fcs0"] = false
	e.h.resetCalls()
	require.NoError(t, e.reconcile())
	e.assertDone(name)
	t.Logf("mutating on retry: %v", e.h.mutating)
	assert.Contains(t, e.h.mutating, aix.CmdStopsrc+" -s appd", "appd must be restarted to pick up its new definition")
}
