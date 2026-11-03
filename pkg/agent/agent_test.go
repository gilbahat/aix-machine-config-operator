package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/gilbahat/aix-machine-config-operator/pkg/nodestate"
	"github.com/gilbahat/aix-machine-config-operator/pkg/rendered"
)

const ns = "aix-machine-config-operator"

type fakeRestarter struct {
	calls []string
	fail  map[string]error
}

func (f *fakeRestarter) Restart(_ context.Context, s string) error {
	f.calls = append(f.calls, s)
	return f.fail[s]
}

type env struct {
	t    *testing.T
	root string
	kube *fake.Clientset
	r    *fakeRestarter
	a    *Agent
}

func newEnv(t *testing.T) *env {
	e := &env{t: t, root: t.TempDir(), kube: fake.NewSimpleClientset(), r: &fakeRestarter{fail: map[string]error{}}}
	_, err := e.kube.CoreV1().Nodes().Create(context.Background(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "aix1", Annotations: map[string]string{}}}, metav1.CreateOptions{})
	require.NoError(t, err)
	e.a = e.newAgent()
	return e
}

func (e *env) newAgent() *Agent {
	return &Agent{Host: Host{Root: e.root}, Kube: e.kube, Restarter: e.r, NodeName: "aix1", Namespace: ns}
}

func (e *env) publish(files ...rendered.File) string {
	c := &rendered.Config{Version: rendered.FormatVersion, Pool: "aix", Files: files}
	name, err := c.Name()
	require.NoError(e.t, err)
	data, err := c.Marshal()
	require.NoError(e.t, err)
	_, err = e.kube.CoreV1().ConfigMaps(ns).Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       map[string]string{rendered.ConfigMapKey: string(data)},
	}, metav1.CreateOptions{})
	require.NoError(e.t, err)
	return name
}

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

var (
	kubeletConf = rendered.File{Path: "/etc/kubernetes/kubelet.conf", Mode: 0o644, Contents: "kind: KubeletConfiguration\n", Group: rendered.GroupKubelet}
	quayHosts   = rendered.File{Path: "/etc/containerd/certs.d/quay.io/hosts.toml", Mode: 0o644, Contents: "server = \"https://quay.io\"\n", Group: rendered.GroupRegistries}
)

func (e *env) assertDone(name string) {
	e.t.Helper()
	n := e.node()
	assert.Equal(e.t, name, nodestate.Current(n))
	assert.Equal(e.t, nodestate.StateDone, nodestate.State(n))
	assert.Empty(e.t, n.Annotations[nodestate.ReasonAnnotation])
}

func TestFirstApplyAndLiveUpdate(t *testing.T) {
	e := newEnv(t)
	require.NoError(t, e.reconcile(), "no desired config is a no-op")

	first := e.publish(kubeletConf, quayHosts)
	e.setDesired(first)
	require.NoError(t, e.reconcile())
	e.assertDone(first)
	assert.Equal(t, kubeletConf.Contents, e.read(kubeletConf.Path))
	assert.Equal(t, []string{"kubelet"}, e.r.calls)

	// Dropping the registry and changing nothing else: no restart, host dir pruned.
	second := e.publish(kubeletConf)
	e.setDesired(second)
	e.r.calls = nil
	require.NoError(t, e.reconcile())
	e.assertDone(second)
	assert.Empty(t, e.r.calls)
	assert.False(t, e.exists("/etc/containerd/certs.d/quay.io"))
	assert.True(t, e.exists("/etc/containerd/certs.d"))

	// Nothing to do on the next pass.
	require.NoError(t, e.reconcile())
	assert.Empty(t, e.r.calls)
}

func TestPreexistingFileIsBackedUpAndRestored(t *testing.T) {
	e := newEnv(t)
	e.write(kubeletConf.Path, "handwritten\n")
	first := e.publish(kubeletConf)
	e.setDesired(first)
	require.NoError(t, e.reconcile())
	assert.Equal(t, "handwritten\n", e.read(kubeletConf.Path+BackupSuffix))

	second := e.publish(quayHosts)
	e.setDesired(second)
	require.NoError(t, e.reconcile())
	assert.Equal(t, "handwritten\n", e.read(kubeletConf.Path))
	assert.False(t, e.exists(kubeletConf.Path+BackupSuffix))
}

func TestFailedRestartIsRetried(t *testing.T) {
	e := newEnv(t)
	first := e.publish(kubeletConf)
	e.setDesired(first)
	e.r.fail["kubelet"] = errors.New("timed out waiting for SRC subsystem kubelet")
	assert.Error(t, e.reconcile())
	n := e.node()
	assert.Equal(t, nodestate.StateDegraded, nodestate.State(n))
	assert.Contains(t, n.Annotations[nodestate.ReasonAnnotation], "restarting kubelet: timed out")
	assert.Empty(t, nodestate.Current(n))

	delete(e.r.fail, "kubelet")
	require.NoError(t, e.reconcile())
	e.assertDone(first)
	assert.Equal(t, []string{"kubelet", "kubelet"}, e.r.calls)
}

func TestUndefinedServiceIsSkipped(t *testing.T) {
	e := newEnv(t)
	e.r.fail["kubelet"] = fmt.Errorf("kubelet: %w", ErrServiceNotDefined)
	first := e.publish(kubeletConf)
	e.setDesired(first)
	require.NoError(t, e.reconcile())
	e.assertDone(first)
}

func TestDriftDegradesUntilFixedOrForced(t *testing.T) {
	e := newEnv(t)
	first := e.publish(kubeletConf, quayHosts)
	e.setDesired(first)
	require.NoError(t, e.reconcile())

	e.write(quayHosts.Path, "tampered\n")
	require.NoError(t, e.reconcile())
	n := e.node()
	assert.Equal(t, nodestate.StateDegraded, nodestate.State(n))
	assert.Contains(t, n.Annotations[nodestate.ReasonAnnotation], "hosts.toml: content changed")

	// A restarted agent does not overwrite the drifted file.
	e.a = e.newAgent()
	require.NoError(t, e.reconcile())
	assert.Equal(t, "tampered\n", e.read(quayHosts.Path))
	assert.Equal(t, nodestate.StateDegraded, nodestate.State(e.node()))

	// Restoring the file clears the degradation.
	e.write(quayHosts.Path, quayHosts.Contents)
	require.NoError(t, e.reconcile())
	e.assertDone(first)

	// The force file re-applies everything.
	e.write(quayHosts.Path, "tampered again\n")
	require.NoError(t, e.reconcile())
	e.write(ForceFile, "")
	e.r.calls = nil
	require.NoError(t, e.reconcile())
	e.assertDone(first)
	assert.Equal(t, quayHosts.Contents, e.read(quayHosts.Path))
	assert.False(t, e.exists(ForceFile))
	assert.Equal(t, []string{"kubelet"}, e.r.calls)
}

func TestApplyConfigStandalone(t *testing.T) {
	root := t.TempDir()
	h := Host{Root: root}
	cfg := &rendered.Config{Version: rendered.FormatVersion, Pool: "aix", Files: []rendered.File{quayHosts}}
	require.NoError(t, ApplyConfig(context.Background(), h, nil, "test", cfg, nil))
	a, err := h.LoadApplied()
	require.NoError(t, err)
	assert.Equal(t, "test", a.Name)
	assert.Empty(t, h.Drift(cfg))
	st, err := os.Stat(filepath.Join(root, StateFile))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
}
