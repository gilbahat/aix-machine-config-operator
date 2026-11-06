package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/gilbahat/aix-machine-config-operator/pkg/aix"
	"github.com/gilbahat/aix-machine-config-operator/pkg/nodestate"
	"github.com/gilbahat/aix-machine-config-operator/pkg/rendered"
)

const ns = "aix-machine-config-operator"

type env struct {
	t    *testing.T
	root string
	kube *fake.Clientset
	h    *fakeHost
	a    *Agent
}

func newEnv(t *testing.T) *env {
	e := &env{t: t, root: t.TempDir(), kube: fake.NewSimpleClientset(), h: newFakeHost()}
	e.h.defineSubsystem("containerd", aix.StateActive)
	e.h.defineSubsystem("kubelet", aix.StateActive)
	_, err := e.kube.CoreV1().Nodes().Create(context.Background(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "aix1", Annotations: map[string]string{}}}, metav1.CreateOptions{})
	require.NoError(t, err)
	e.a = e.newAgent()
	return e
}

func (e *env) engine() *Engine {
	return &Engine{
		Files: Host{Root: e.root}, Runner: e.h, BootTime: e.h.boot,
		Chown: e.h.chown, FileOwner: e.h.fileOwner,
		LookupUID: func(n string) (int, error) {
			return map[string]int{"root": 0, "bin": 2}[n], nil
		},
		LookupGID: func(n string) (int, error) {
			return map[string]int{"system": 0, "security": 7}[n], nil
		},
	}
}

func (e *env) newAgent() *Agent {
	return &Agent{Engine: e.engine(), Kube: e.kube, NodeName: "aix1", Namespace: ns}
}

// boot simulates a reboot by someone else and restarts the agent.
func (e *env) boot() {
	e.h.reboot()
	e.a = e.newAgent()
	_, err := e.a.Engine.ReconcileBoot()
	require.NoError(e.t, err)
}

func (e *env) publish(c *rendered.Config) string {
	c.Version, c.Pool = rendered.FormatVersion, "aix"
	name, err := c.Name()
	require.NoError(e.t, err)
	data, err := c.Marshal()
	require.NoError(e.t, err)
	_, err = e.kube.CoreV1().ConfigMaps(ns).Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{rendered.PoolLabel: "aix"}},
		Immutable:  ptr.To(true),
		Data:       map[string]string{rendered.ConfigMapKey: string(data)},
	}, metav1.CreateOptions{})
	if !apierrors.IsAlreadyExists(err) {
		require.NoError(e.t, err)
	}
	return name
}

func files(fs ...rendered.File) *rendered.Config { return &rendered.Config{Files: fs} }

func (e *env) node() *corev1.Node {
	n, err := e.kube.CoreV1().Nodes().Get(context.Background(), "aix1", metav1.GetOptions{})
	require.NoError(e.t, err)
	return n
}

func (e *env) setDesired(name string) {
	n := e.node()
	n.Annotations[nodestate.DesiredConfigAnnotation] = name
	_, err := e.kube.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{})
	require.NoError(e.t, err)
}

func (e *env) roll(c *rendered.Config) (string, error) {
	name := e.publish(c)
	e.setDesired(name)
	return name, e.reconcile()
}

func (e *env) reconcile() error {
	return e.a.Reconcile(context.Background(), e.node())
}

func (e *env) read(p string) string {
	data, err := os.ReadFile(filepath.Join(e.root, p))
	require.NoError(e.t, err)
	return string(data)
}

func (e *env) exists(p string) bool {
	_, err := os.Stat(filepath.Join(e.root, p))
	return err == nil
}

func (e *env) write(p, data string) {
	full := filepath.Join(e.root, p)
	require.NoError(e.t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(e.t, os.WriteFile(full, []byte(data), 0o644))
}

func (e *env) annotationList(key string) []string {
	v, ok := e.node().Annotations[key]
	if !ok {
		return nil
	}
	var out []string
	require.NoError(e.t, json.Unmarshal([]byte(v), &out))
	return out
}

func (e *env) issues(key string) []nodestate.ItemIssue {
	v, ok := e.node().Annotations[key]
	if !ok {
		return nil
	}
	var out []nodestate.ItemIssue
	require.NoError(e.t, json.Unmarshal([]byte(v), &out))
	return out
}

func (e *env) assertDone(name string) {
	e.t.Helper()
	n := e.node()
	assert.Equal(e.t, name, nodestate.Current(n))
	assert.Equal(e.t, nodestate.StateDone, nodestate.State(n), n.Annotations[nodestate.ReasonAnnotation])
	assert.Empty(e.t, n.Annotations[nodestate.ReasonAnnotation])
}

var (
	kubeletConf = rendered.File{Path: "/etc/kubernetes/kubelet.conf", Mode: 0o644, Contents: "kind: KubeletConfiguration\n", Origin: rendered.OriginAMCO,
		Actions: []rendered.SubsystemAction{{Subsystem: "kubelet", Action: rendered.ActionRestart, IgnoreMissing: true}}, Policy: rendered.Policy{Drain: true}}
	quayHosts = rendered.File{Path: "/etc/containerd/certs.d/quay.io/hosts.toml", Mode: 0o644, Contents: "server = \"https://quay.io\"\n", Origin: rendered.OriginAMCO}
	sshdConf  = rendered.File{Path: "/etc/ssh/sshd_config", Mode: 0o600, Contents: "PermitRootLogin no\n", Origin: rendered.OriginUser,
		Actions: []rendered.SubsystemAction{{Subsystem: "sshd", Action: rendered.ActionRefresh}}}
)

func TestFirstApplyAndLiveUpdate(t *testing.T) {
	e := newEnv(t)
	require.NoError(t, e.reconcile(), "no desired config is a no-op")

	first, err := e.roll(files(kubeletConf, quayHosts))
	require.NoError(t, err)
	e.assertDone(first)
	assert.Equal(t, kubeletConf.Contents, e.read(kubeletConf.Path))
	assert.Contains(t, e.h.mutating, aix.CmdStartsrc+" -s kubelet", "the kubelet was restarted")

	// Dropping the registry and changing nothing else: no restart, host dir pruned.
	e.h.resetCalls()
	second, err := e.roll(files(kubeletConf))
	require.NoError(t, err)
	e.assertDone(second)
	assert.Empty(t, e.h.mutating)
	assert.False(t, e.exists("/etc/containerd/certs.d/quay.io"))

	// Nothing to do on the next pass.
	e.h.resetCalls()
	require.NoError(t, e.reconcile())
	assert.Empty(t, e.h.mutating)
}

func TestPreexistingFileIsBackedUpAndRestored(t *testing.T) {
	e := newEnv(t)
	e.write(kubeletConf.Path, "handwritten\n")
	_, err := e.roll(files(kubeletConf))
	require.NoError(t, err)
	assert.Equal(t, "handwritten\n", e.read(backupPath(kubeletConf.Path)))

	_, err = e.roll(files(quayHosts))
	require.NoError(t, err)
	assert.Equal(t, "handwritten\n", e.read(kubeletConf.Path))
	assert.False(t, e.exists(backupPath(kubeletConf.Path)))
}

func TestFailedRestartIsRetried(t *testing.T) {
	e := newEnv(t)
	e.h.startErr["kubelet"] = errors.New("0513-059 timed out")
	name, err := e.roll(files(kubeletConf))
	assert.Error(t, err)
	n := e.node()
	assert.Equal(t, nodestate.StateDegraded, nodestate.State(n))
	assert.Contains(t, n.Annotations[nodestate.ReasonAnnotation], "restart of kubelet")
	assert.Empty(t, nodestate.Current(n))

	delete(e.h.startErr, "kubelet")
	e.h.resetCalls()
	require.NoError(t, e.reconcile())
	e.assertDone(name)
	assert.Contains(t, e.h.mutating, aix.CmdStartsrc+" -s kubelet", "the retry runs the restart again")
}

func TestUndefinedServiceIsSkipped(t *testing.T) {
	e := newEnv(t)
	delete(e.h.subsystems, "kubelet")
	name, err := e.roll(files(kubeletConf))
	require.NoError(t, err)
	e.assertDone(name)
}

func TestFileOwnerModeAndRefresh(t *testing.T) {
	e := newEnv(t)
	e.h.defineSubsystem("sshd", aix.StateActive)
	f := sshdConf
	f.Owner, f.Group = "root", "security"
	_, err := e.roll(files(f))
	require.NoError(t, err)
	st, err := os.Stat(filepath.Join(e.root, f.Path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
	assert.Equal(t, [2]int{0, 7}, e.h.owners[filepath.Join(e.root, f.Path)])
	assert.Contains(t, e.h.mutating, aix.CmdRefresh+" -s sshd")

	bin := rendered.File{Path: "/etc/app/blob", Mode: 0o644, Contents: "AAEC/w==", Encoding: "base64", Origin: rendered.OriginUser}
	_, err = e.roll(files(f, bin))
	require.NoError(t, err)
	assert.Equal(t, "\x00\x01\x02\xff", e.read(bin.Path))
}

func TestDriftDegradesUntilFixedOrForced(t *testing.T) {
	e := newEnv(t)
	first, err := e.roll(files(kubeletConf, quayHosts))
	require.NoError(t, err)

	e.write(quayHosts.Path, "tampered\n")
	require.NoError(t, e.reconcile())
	n := e.node()
	assert.Equal(t, nodestate.StateDegraded, nodestate.State(n))
	assert.Contains(t, n.Annotations[nodestate.ReasonAnnotation], "hosts.toml: content changed")
	assert.Equal(t, []nodestate.ItemIssue{{Item: "file:" + quayHosts.Path, Reason: "content changed"}}, e.issues(nodestate.DriftAnnotation))

	// A restarted agent does not overwrite the drifted file.
	e.a = e.newAgent()
	require.NoError(t, e.reconcile())
	assert.Equal(t, "tampered\n", e.read(quayHosts.Path))

	// Restoring the file clears the degradation.
	e.write(quayHosts.Path, quayHosts.Contents)
	require.NoError(t, e.reconcile())
	e.assertDone(first)
	assert.Empty(t, e.issues(nodestate.DriftAnnotation))

	// The force file re-applies everything.
	e.write(quayHosts.Path, "tampered again\n")
	require.NoError(t, e.reconcile())
	e.write(ForceFile, "")
	e.h.resetCalls()
	require.NoError(t, e.reconcile())
	e.assertDone(first)
	assert.Equal(t, quayHosts.Contents, e.read(quayHosts.Path))
	assert.False(t, e.exists(ForceFile))
	assert.Contains(t, e.h.mutating, aix.CmdStartsrc+" -s kubelet")
}

func TestFailedApplyIsRepairedByTheNextConfig(t *testing.T) {
	e := newEnv(t)
	_, err := e.roll(files(kubeletConf))
	require.NoError(t, err)

	k1 := kubeletConf
	k1.Contents = "kind: KubeletConfiguration\nmaxPods: 1\n"
	e.h.startErr["kubelet"] = errors.New("start timed out")
	_, err = e.roll(files(k1))
	assert.Error(t, err)

	// The next config's kubelet.conf equals the first one; the host has
	// k1's file and a stopped kubelet, so the agent must rewrite and restart.
	delete(e.h.startErr, "kubelet")
	e.h.resetCalls()
	t2, err := e.roll(files(kubeletConf, quayHosts))
	require.NoError(t, err)
	e.assertDone(t2)
	assert.Equal(t, kubeletConf.Contents, e.read(kubeletConf.Path))
	assert.Equal(t, aix.StateActive, e.h.subsystems["kubelet"].state)
}

func TestRefusesForgedConfigMaps(t *testing.T) {
	cfg := &rendered.Config{Version: rendered.FormatVersion, Pool: "aix", Files: []rendered.File{quayHosts}}
	name, err := cfg.Name()
	require.NoError(t, err)
	data, err := cfg.Marshal()
	require.NoError(t, err)
	good := func() *corev1.ConfigMap {
		return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{rendered.PoolLabel: "aix"}},
			Immutable:  ptr.To(true),
			Data:       map[string]string{rendered.ConfigMapKey: string(data)},
		}
	}
	_, err = verifyRendered(good(), name)
	require.NoError(t, err)
	mutable := good()
	mutable.Immutable = nil
	unlabelled := good()
	unlabelled.Labels = nil
	for what, cm := range map[string]*corev1.ConfigMap{"not immutable": mutable, "label": unlabelled} {
		_, err := verifyRendered(cm, name)
		assert.ErrorContains(t, err, what)
	}
	_, err = verifyRendered(good(), "rendered-aix-0000000000000000")
	assert.ErrorContains(t, err, "does not match its name")
}

func TestStandaloneFilesOnly(t *testing.T) {
	root := t.TempDir()
	e := &Engine{Files: Host{Root: root}, NoRestart: true, Chown: func(string, int, int) error { return nil },
		FileOwner: func(string, os.FileInfo) (int, int, bool) { return 0, 0, true }}
	cfg := &rendered.Config{Version: rendered.FormatVersion, Pool: "aix", Files: []rendered.File{quayHosts, kubeletConf}}
	res, err := e.Apply(context.Background(), "test", cfg, ApplyOptions{})
	require.NoError(t, err)
	assert.Len(t, res.Changed, 2)
	st, err := os.Stat(filepath.Join(root, StateFile))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())

	cfg.Tunables = []rendered.Tunable{{Command: "no", Name: "tcp_sendspace", Value: "1"}}
	_, err = e.Apply(context.Background(), "test2", cfg, ApplyOptions{})
	assert.ErrorContains(t, err, "needs AIX commands")
}

// hostConfig exercises every item type.
func hostConfig() *rendered.Config {
	return &rendered.Config{
		Files: []rendered.File{sshdConf},
		Subsystems: []rendered.Subsystem{
			{Name: "sshd", State: rendered.SubsystemActive},
			{Name: "appd", State: rendered.SubsystemActive, Definition: &aix.SubsystemDefinition{Path: "/opt/app/bin/appd", Arguments: "-f /etc/app.conf", Respawn: true}},
		},
		Inittab:          []rendered.InittabEntry{{InittabEntry: aix.InittabEntry{Identifier: "appd", RunLevel: "2", Action: "once", Command: "/usr/bin/startsrc -s appd"}}},
		Tunables:         []rendered.Tunable{{Command: "no", Name: "tcp_sendspace", Value: "262144"}},
		DeviceAttributes: []rendered.DeviceAttribute{{Device: "fcs0", Attribute: "num_cmd_elems", Value: "2048"}},
	}
}

func newHostEnv(t *testing.T) *env {
	e := newEnv(t)
	e.h.defineSubsystem("sshd", aix.StateInoperative)
	e.h.tunable("no", "tcp_sendspace", "16384", aix.TunableConnect)
	e.h.device("fcs0", "num_cmd_elems", "500")
	e.write(sshdConf.Path, "# original\n")
	return e
}

func TestHostItemsApplyIdempotentAndRestore(t *testing.T) {
	e := newHostEnv(t)
	name, err := e.roll(hostConfig())
	require.NoError(t, err)
	e.assertDone(name)
	assert.Equal(t, aix.StateActive, e.h.subsystems["sshd"].state)
	assert.Equal(t, aix.StateActive, e.h.subsystems["appd"].state)
	assert.True(t, e.h.subsystems["appd"].def.Respawn)
	assert.Equal(t, "/usr/bin/startsrc -s appd", e.h.inittab["appd"].Command)
	assert.Equal(t, "262144", e.h.tunables["no"]["tcp_sendspace"].cur)
	assert.Equal(t, "2048", e.h.devices["fcs0"]["num_cmd_elems"].running)
	assert.NotContains(t, e.h.mutating, aix.CmdRefresh+" -s sshd", "sshd was just started; no refresh")

	// Re-apply with no change: zero commands that change anything.
	e.h.resetCalls()
	again := hostConfig()
	again.Version, again.Pool = rendered.FormatVersion, "aix"
	_, err = e.a.Engine.Apply(context.Background(), name, again, ApplyOptions{})
	require.NoError(t, err)
	assert.Empty(t, e.h.mutating, "re-applying an applied config changes nothing")
	assert.NotEmpty(t, e.h.calls, "it does read the live state")

	// Removing every item restores the baselines.
	e.h.resetCalls()
	_, err = e.roll(&rendered.Config{})
	require.NoError(t, err)
	assert.Equal(t, "# original\n", e.read(sshdConf.Path))
	assert.Equal(t, aix.StateInoperative, e.h.subsystems["sshd"].state)
	assert.NotContains(t, e.h.subsystems, "appd", "AMCO defined appd, so it is removed")
	assert.NotContains(t, e.h.inittab, "appd")
	assert.Equal(t, "16384", e.h.tunables["no"]["tcp_sendspace"].cur)
	assert.Equal(t, "500", e.h.devices["fcs0"]["num_cmd_elems"].running)
	st, err := e.a.Host().LoadState()
	require.NoError(t, err)
	assert.Empty(t, st.Baselines)
}

func TestHostItemDrift(t *testing.T) {
	e := newHostEnv(t)
	name, err := e.roll(hostConfig())
	require.NoError(t, err)

	e.h.tunables["no"]["tcp_sendspace"].cur = "1"
	e.h.devices["fcs0"]["num_cmd_elems"].odm = "999"
	e.h.subsystems["sshd"].state = aix.StateInoperative
	delete(e.h.inittab, "appd")
	e.h.resetCalls()
	require.NoError(t, e.reconcile())
	assert.Empty(t, e.h.mutating, "drift is reported, never repaired")
	var items []string
	for _, i := range e.issues(nodestate.DriftAnnotation) {
		items = append(items, i.Item)
	}
	assert.ElementsMatch(t, []string{"tunable:no:tcp_sendspace", "device:fcs0:num_cmd_elems", "subsystem:sshd", "inittab:appd"}, items)
	assert.Equal(t, nodestate.StateDegraded, nodestate.State(e.node()))
	assert.Equal(t, name, nodestate.Current(e.node()))
}

func TestRebootTunableBecomesPendingThenClears(t *testing.T) {
	e := newEnv(t)
	e.h.tunable("no", "ipqmaxlen", "100", aix.TunableReboot)
	cfg := &rendered.Config{Tunables: []rendered.Tunable{{Command: "no", Name: "ipqmaxlen", Value: "512"}}}
	name, err := e.roll(cfg)
	require.NoError(t, err)
	e.assertDone(name)
	assert.Equal(t, []string{"tunable:no:ipqmaxlen"}, e.annotationList(nodestate.PendingRebootAnnotation))
	assert.Equal(t, "100", e.h.tunables["no"]["ipqmaxlen"].cur, "never applied live, never rebooted")
	assert.Equal(t, "512", e.h.tunables["no"]["ipqmaxlen"].boot)
	assert.NotContains(t, e.h.mutating, aix.CmdBosboot)

	// Pending is not drift, and re-applying changes nothing.
	e.h.resetCalls()
	require.NoError(t, e.reconcile())
	assert.Empty(t, e.issues(nodestate.DriftAnnotation))
	assert.Empty(t, e.h.mutating)

	// Someone reboots the node; the agent clears the item on start.
	e.boot()
	require.NoError(t, e.reconcile())
	assert.Empty(t, e.annotationList(nodestate.PendingRebootAnnotation))
	assert.Empty(t, e.issues(nodestate.DriftAnnotation))
	e.assertDone(name)
}

func TestPendingItemNotEffectiveAfterBootIsDrift(t *testing.T) {
	e := newEnv(t)
	e.h.tunable("no", "ipqmaxlen", "100", aix.TunableReboot)
	_, err := e.roll(&rendered.Config{Tunables: []rendered.Tunable{{Command: "no", Name: "ipqmaxlen", Value: "512"}}})
	require.NoError(t, err)
	e.h.reboot()
	e.h.tunables["no"]["ipqmaxlen"].cur = "100" // the boot did not pick it up
	e.a = e.newAgent()
	_, err = e.a.Engine.ReconcileBoot()
	require.NoError(t, err)
	require.NoError(t, e.reconcile())
	assert.Empty(t, e.annotationList(nodestate.PendingRebootAnnotation))
	assert.Equal(t, "tunable:no:ipqmaxlen", e.issues(nodestate.DriftAnnotation)[0].Item)
}

func TestBosbootTunable(t *testing.T) {
	e := newEnv(t)
	e.h.tunable("vmo", "kernel_heap_psize", "4096", aix.TunableBosboot)
	e.h.bosbootErr = errors.New("0301-150 bosboot: Invalid or no boot device specified")
	cfg := &rendered.Config{Tunables: []rendered.Tunable{{Command: "vmo", Name: "kernel_heap_psize", Value: "65536"}}}
	_, err := e.roll(cfg)
	assert.ErrorContains(t, err, "bosboot failed")
	assert.Equal(t, nodestate.StateDegraded, nodestate.State(e.node()))
	var bs nodestate.BosbootStatus
	require.NoError(t, json.Unmarshal([]byte(e.node().Annotations[nodestate.BosbootAnnotation]), &bs))
	assert.Equal(t, nodestate.BosbootFailed, bs.State)

	// No automatic retry.
	e.h.bosbootErr = nil
	e.h.resetCalls()
	assert.Error(t, e.reconcile())
	assert.Empty(t, e.h.mutating)

	// The force file retries it.
	e.write(ForceFile, "")
	require.NoError(t, e.reconcile())
	assert.Contains(t, e.h.mutating, aix.CmdBosboot+" -ad /dev/ipldevice")
	require.NoError(t, json.Unmarshal([]byte(e.node().Annotations[nodestate.BosbootAnnotation]), &bs))
	assert.Equal(t, nodestate.BosbootRebuilt, bs.State)
	assert.Equal(t, []string{"tunable:vmo:kernel_heap_psize"}, e.annotationList(nodestate.PendingRebootAnnotation))

	e.boot()
	require.NoError(t, e.reconcile())
	assert.NotContains(t, e.node().Annotations, nodestate.BosbootAnnotation, "the boot used the rebuilt image")
	assert.Empty(t, e.annotationList(nodestate.PendingRebootAnnotation))
}

func TestBusyDevice(t *testing.T) {
	e := newEnv(t)
	e.h.device("fcs0", "num_cmd_elems", "500")
	e.h.busy["fcs0"] = true
	name, err := e.roll(&rendered.Config{DeviceAttributes: []rendered.DeviceAttribute{{Device: "fcs0", Attribute: "num_cmd_elems", Value: "2048"}}})
	require.NoError(t, err)
	e.assertDone(name)
	assert.Contains(t, e.h.mutating, aix.CmdChdev+" -l fcs0 -a num_cmd_elems=2048 -P")
	assert.Equal(t, []string{"device:fcs0:num_cmd_elems"}, e.annotationList(nodestate.PendingRebootAnnotation))
	assert.Equal(t, "500", e.h.devices["fcs0"]["num_cmd_elems"].running)

	e.boot()
	require.NoError(t, e.reconcile())
	assert.Empty(t, e.annotationList(nodestate.PendingRebootAnnotation))
	assert.Equal(t, "2048", e.h.devices["fcs0"]["num_cmd_elems"].running)

	// onBusy: Fail.
	e.h.busy["fcs0"] = true
	_, err = e.roll(&rendered.Config{DeviceAttributes: []rendered.DeviceAttribute{{Device: "fcs0", Attribute: "num_cmd_elems", Value: "1024", OnBusy: rendered.OnBusyFail}}})
	assert.ErrorContains(t, err, "onBusy is Fail")
}

func TestReprovisionRequired(t *testing.T) {
	e := newEnv(t)
	e.h.tunable("no", "tcp_sendspace", "16384", aix.TunableConnect)
	cfg := &rendered.Config{
		Tunables:         []rendered.Tunable{{Command: "no", Name: "tcp_sendspace", Value: "262144"}},
		DeviceAttributes: []rendered.DeviceAttribute{{Device: "fcs1", Attribute: "num_cmd_elems", Value: "2048"}},
		Subsystems:       []rendered.Subsystem{{Name: "nosuchd", State: rendered.SubsystemActive}},
	}
	name, err := e.roll(cfg)
	require.NoError(t, err, "items that cannot converge do not block the others")
	e.assertDone(name)
	assert.Equal(t, "262144", e.h.tunables["no"]["tcp_sendspace"].cur)
	var items []string
	for _, i := range e.issues(nodestate.ReprovisionAnnotation) {
		items = append(items, i.Item)
	}
	assert.Equal(t, []string{"subsystem:nosuchd", "device:fcs1:num_cmd_elems"}, items)

	// A required item blocks everything, before anything changes.
	cfg.Tunables[0].Value = "1"
	cfg.DeviceAttributes[0].Required = true
	e.h.resetCalls()
	_, err = e.roll(cfg)
	assert.ErrorContains(t, err, "device:fcs1:num_cmd_elems cannot converge in place and is required")
	assert.Empty(t, e.h.mutating)
	assert.Equal(t, "262144", e.h.tunables["no"]["tcp_sendspace"].cur)
}

func TestRejectedTunables(t *testing.T) {
	for typ, want := range map[aix.TunableType]string{aix.TunableStatic: "Static", aix.TunableDeprecated: "deprecated", "X": "unknown tunable type"} {
		e := newEnv(t)
		e.h.tunable("vmo", "t", "1", typ)
		_, err := e.roll(&rendered.Config{Tunables: []rendered.Tunable{{Command: "vmo", Name: "t", Value: "2"}}})
		assert.ErrorContains(t, err, want)
		assert.Equal(t, nodestate.StateDegraded, nodestate.State(e.node()))
	}
	e := newEnv(t)
	e.h.tunable("vmo", "lru_file_repage", "0", aix.TunableDynamic)
	e.h.tunables["vmo"]["lru_file_repage"].restricted = true
	_, err := e.roll(&rendered.Config{Tunables: []rendered.Tunable{{Command: "vmo", Name: "lru_file_repage", Value: "1"}}})
	assert.ErrorContains(t, err, "restricted")
}

func TestInterruptedApplyResumes(t *testing.T) {
	e := newHostEnv(t)
	e.h.startErr["appd"] = errors.New("cannot start")
	_, err := e.roll(hostConfig())
	require.Error(t, err)
	st, err := e.a.Host().LoadState()
	require.NoError(t, err)
	require.NotNil(t, st.Pending)
	assert.Contains(t, st.Baselines, "file:/etc/ssh/sshd_config", "baselines are saved before the first change")

	delete(e.h.startErr, "appd")
	require.NoError(t, e.reconcile())
	assert.Equal(t, aix.StateActive, e.h.subsystems["appd"].state)
	// The baseline is still the original, not AMCO's half-applied state.
	_, err = e.roll(&rendered.Config{})
	require.NoError(t, err)
	assert.Equal(t, "# original\n", e.read(sshdConf.Path))
	assert.Equal(t, "16384", e.h.tunables["no"]["tcp_sendspace"].cur)
}

func TestV1StateIsMigrated(t *testing.T) {
	e := newEnv(t)
	e.write(StateFile, `{"name":"rendered-aix-1111111111111111","config":{"version":1,"pool":"aix","files":[{"path":"/etc/containerd/certs.d/quay.io/hosts.toml","mode":420,"contents":"old","group":"registries"}]}}`)
	e.write(quayHosts.Path, "old")
	name, err := e.roll(files(kubeletConf))
	require.NoError(t, err)
	e.assertDone(name)
	assert.False(t, e.exists(quayHosts.Path), "the v1 file left the config and had no original")
}
