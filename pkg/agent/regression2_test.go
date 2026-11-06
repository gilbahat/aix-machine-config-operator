package agent

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/gilbahat/aix-machine-config-operator/pkg/aix"
	"github.com/gilbahat/aix-machine-config-operator/pkg/rendered"
)

// Regression (second review): while a later step keeps failing, every retry restarts every service
// that has an action, including ones whose restart already succeeded.
func TestRegressionRetryRestartsEverything(t *testing.T) {
	e := newEnv(t)
	dropIn := rendered.File{Path: "/etc/containerd/conf.d/amco.toml", Mode: 0o644, Contents: "x\n", Origin: rendered.OriginAMCO,
		Actions: []rendered.SubsystemAction{{Subsystem: "containerd", Action: rendered.ActionRestart, IgnoreMissing: true}}}
	e.h.startErr["kubelet"] = errors.New("start timed out")
	_, _ = e.roll(files(dropIn, kubeletConf))
	e.h.resetCalls()
	for i := 0; i < 3; i++ {
		_ = e.reconcile()
	}
	assert.Zero(t, countPrefix(e.h.mutating, aix.CmdStopsrc+" -s containerd"), "containerd's restart already succeeded")
	assert.NotZero(t, countPrefix(e.h.mutating, aix.CmdStartsrc+" -s kubelet"), "the failed kubelet restart is retried")
}
