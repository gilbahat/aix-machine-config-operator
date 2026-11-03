package operator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"
	cfglisters "github.com/openshift/client-go/config/listers/config/v1"
	mcfglisters "github.com/openshift/client-go/machineconfiguration/listers/machineconfiguration/v1"
	ctrlcommon "github.com/openshift/machine-config-operator/pkg/controller/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	amcov1alpha1 "github.com/gilbahat/aix-machine-config-operator/pkg/apis/amco/v1alpha1"
	"github.com/gilbahat/aix-machine-config-operator/pkg/nodestate"
)

const ns = "aix-machine-config-operator"

type fakePools struct {
	h *harness
}

func (f *fakePools) List(context.Context, metav1.ListOptions) (*amcov1alpha1.AIXConfigPoolList, error) {
	return nil, errors.New("not used")
}
func (f *fakePools) Watch(context.Context, metav1.ListOptions) (watch.Interface, error) {
	return nil, errors.New("not used")
}
func (f *fakePools) Get(_ context.Context, name string) (*amcov1alpha1.AIXConfigPool, error) {
	p, _ := f.h.op.getPool(name)
	return p, nil
}
func (f *fakePools) Create(_ context.Context, p *amcov1alpha1.AIXConfigPool) (*amcov1alpha1.AIXConfigPool, error) {
	return p, f.h.pools.Add(p)
}
func (f *fakePools) UpdateStatus(_ context.Context, p *amcov1alpha1.AIXConfigPool) (*amcov1alpha1.AIXConfigPool, error) {
	return p, f.h.pools.Update(p)
}

type fakeDrainer struct {
	h        *harness
	drained  []string
	drainErr error
}

func (d *fakeDrainer) Cordon(ctx context.Context, node *corev1.Node, cordon bool) error {
	n, err := d.h.kube.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	n.Spec.Unschedulable = cordon
	_, err = d.h.kube.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{})
	return err
}

func (d *fakeDrainer) Drain(_ context.Context, node string) error {
	d.drained = append(d.drained, node)
	return d.drainErr
}

type harness struct {
	t       *testing.T
	op      *Operator
	kube    *fake.Clientset
	drainer *fakeDrainer
	nodes   cache.Indexer
	cms     cache.Indexer
	pools   cache.Indexer
	ccs     cache.Indexer
	idms    cache.Indexer
	clock   time.Time
}

func newHarness(t *testing.T, nodes ...*corev1.Node) *harness {
	h := &harness{t: t, kube: fake.NewSimpleClientset(), clock: time.Unix(1000, 0)}
	idx := func() cache.Indexer {
		return cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	}
	h.nodes, h.cms, h.pools, h.ccs, h.idms = idx(), idx(), idx(), idx(), idx()
	cfg := DefaultConfig()
	cfg.Namespace = ns
	h.op = newOperator(cfg, h.kube, &fakePools{h: h})
	h.drainer = &fakeDrainer{h: h}
	h.op.drainer = h.drainer
	h.op.async = func(f func()) { f() }
	h.op.now = func() time.Time { return h.clock }
	h.op.nodeLister = corelisters.NewNodeLister(h.nodes)
	h.op.cmLister = corelisters.NewConfigMapLister(h.cms).ConfigMaps(ns)
	h.op.poolIndexer = h.pools
	h.op.ccLister = mcfglisters.NewControllerConfigLister(h.ccs)
	h.op.kcLister = mcfglisters.NewKubeletConfigLister(idx())
	h.op.crcLister = mcfglisters.NewContainerRuntimeConfigLister(idx())
	h.op.imageLister = cfglisters.NewImageLister(idx())
	h.op.apiServerLister = cfglisters.NewAPIServerLister(idx())
	h.op.idmsLister = cfglisters.NewImageDigestMirrorSetLister(h.idms)
	h.op.itmsLister = cfglisters.NewImageTagMirrorSetLister(idx())

	require.NoError(t, h.ccs.Add(&mcfgv1.ControllerConfig{ObjectMeta: metav1.ObjectMeta{Name: ctrlcommon.ControllerConfigName}, Spec: mcfgv1.ControllerConfigSpec{
		ClusterDNSIP: "172.30.0.10", KubeAPIServerServingCAData: []byte("CA\n"),
	}}))
	for _, n := range nodes {
		_, err := h.kube.CoreV1().Nodes().Create(context.Background(), n, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	h.refresh()
	return h
}

// refresh copies the fake API state into the listers' indexers.
func (h *harness) refresh() {
	ctx := context.Background()
	nl, err := h.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	require.NoError(h.t, err)
	var nodes []interface{}
	for i := range nl.Items {
		nodes = append(nodes, &nl.Items[i])
	}
	require.NoError(h.t, h.nodes.Replace(nodes, ""))
	cl, err := h.kube.CoreV1().ConfigMaps(ns).List(ctx, metav1.ListOptions{})
	require.NoError(h.t, err)
	var cms []interface{}
	for i := range cl.Items {
		cms = append(cms, &cl.Items[i])
	}
	require.NoError(h.t, h.cms.Replace(cms, ""))
}

func (h *harness) addPool(p *amcov1alpha1.AIXConfigPool) {
	require.NoError(h.t, h.pools.Add(p))
}

func (h *harness) sync(pool string) time.Duration {
	h.t.Helper()
	requeue, err := h.op.syncPool(context.Background(), pool)
	require.NoError(h.t, err)
	h.refresh()
	return requeue
}

func (h *harness) node(name string) *corev1.Node {
	n, err := h.kube.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(h.t, err)
	return n
}

func (h *harness) pool(name string) *amcov1alpha1.AIXConfigPool {
	p, ok := h.op.getPool(name)
	require.True(h.t, ok)
	return p
}

// agentApplies simulates the node agent finishing the node's desired config.
func (h *harness) agentApplies(name string) {
	n := h.node(name)
	n.Annotations[nodestate.CurrentConfigAnnotation] = n.Annotations[nodestate.DesiredConfigAnnotation]
	n.Annotations[nodestate.StateAnnotation] = nodestate.StateDone
	_, err := h.kube.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{})
	require.NoError(h.t, err)
	h.refresh()
}

func aixNode(name string, extraLabels ...string) *corev1.Node {
	l := map[string]string{nodestate.OSLabel: "aix", nodestate.RoleLabel: ""}
	for _, x := range extraLabels {
		l[x] = ""
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: l, Annotations: map[string]string{}},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
}

func defaultPool() *amcov1alpha1.AIXConfigPool {
	return &amcov1alpha1.AIXConfigPool{ObjectMeta: metav1.ObjectMeta{Name: "aix", Generation: 1}}
}

func cond(p *amcov1alpha1.AIXConfigPool, t string) *metav1.Condition {
	return meta.FindStatusCondition(p.Status.Conditions, t)
}

func TestRegistersAIXNodes(t *testing.T) {
	aix := aixNode("aix1")
	delete(aix.Labels, nodestate.RoleLabel)
	linux := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "linux1", Labels: map[string]string{nodestate.OSLabel: "linux"}}}
	h := newHarness(t, aix, linux)
	for _, n := range []string{"aix1", "linux1"} {
		_, err := h.op.syncNode(context.Background(), n)
		require.NoError(t, err)
	}
	assert.Contains(t, h.node("aix1").Labels, nodestate.RoleLabel)
	assert.NotContains(t, h.node("linux1").Labels, nodestate.RoleLabel)
}

func TestRolloutDrainsOneNodeAtATime(t *testing.T) {
	linux := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "linux1", Labels: map[string]string{nodestate.OSLabel: "linux", nodestate.RoleLabel: ""}}}
	h := newHarness(t, aixNode("aix1"), aixNode("aix2"), aixNode("aix3"), linux)
	h.addPool(defaultPool())

	h.sync("aix")
	target := h.pool("aix").Status.RenderedConfig
	require.NotEmpty(t, target)
	assert.Equal(t, target, nodestate.Desired(h.node("aix1")))
	assert.True(t, h.node("aix1").Spec.Unschedulable)
	assert.Equal(t, target, h.node("aix1").Annotations[nodestate.CordonedByAnnotation])
	assert.Empty(t, nodestate.Desired(h.node("aix2")))
	assert.Empty(t, nodestate.Desired(h.node("linux1")), "Linux nodes are never managed")
	assert.Equal(t, []string{"aix1"}, h.drainer.drained)
	assert.Equal(t, int32(3), h.pool("aix").Status.MachineCount)
	assert.Equal(t, metav1.ConditionTrue, cond(h.pool("aix"), amcov1alpha1.ConditionUpdating).Status)

	// Nothing else starts while aix1 is unavailable.
	h.sync("aix")
	assert.Empty(t, nodestate.Desired(h.node("aix2")))

	h.agentApplies("aix1")
	h.sync("aix")
	assert.False(t, h.node("aix1").Spec.Unschedulable, "AMCO uncordons nodes it cordoned")
	assert.NotContains(t, h.node("aix1").Annotations, nodestate.CordonedByAnnotation)
	assert.Equal(t, target, nodestate.Desired(h.node("aix2")))

	h.agentApplies("aix2")
	h.sync("aix")
	h.agentApplies("aix3")
	h.sync("aix")
	p := h.pool("aix")
	assert.Equal(t, int32(3), p.Status.UpdatedMachineCount)
	assert.Equal(t, int32(3), p.Status.ReadyMachineCount)
	assert.Equal(t, metav1.ConditionTrue, cond(p, amcov1alpha1.ConditionUpdated).Status)
	assert.Equal(t, metav1.ConditionFalse, cond(p, amcov1alpha1.ConditionDegraded).Status)
}

func updateAll(h *harness, pool string, nodes ...string) {
	for i := 0; i < 2*len(nodes)+2; i++ {
		h.sync(pool)
		for _, n := range nodes {
			if nodestate.Desired(h.node(n)) != nodestate.Current(h.node(n)) {
				h.agentApplies(n)
			}
		}
	}
	h.sync(pool)
}

func TestRegistryChangeIsLiveAndRespectsMaxUnavailable(t *testing.T) {
	h := newHarness(t, aixNode("aix1"), aixNode("aix2"), aixNode("aix3"))
	p := defaultPool()
	p.Spec.MaxUnavailable = &intstr.IntOrString{Type: intstr.String, StrVal: "67%"}
	h.addPool(p)
	updateAll(h, "aix", "aix1", "aix2", "aix3")
	first := h.pool("aix").Status.RenderedConfig
	h.drainer.drained = nil

	require.NoError(t, h.idms.Add(&configv1.ImageDigestMirrorSet{ObjectMeta: metav1.ObjectMeta{Name: "m"}, Spec: configv1.ImageDigestMirrorSetSpec{
		ImageDigestMirrors: []configv1.ImageDigestMirrors{{Source: "quay.io", Mirrors: []configv1.ImageMirror{"mirror.example.com"}}},
	}}))
	h.sync("aix")
	second := h.pool("aix").Status.RenderedConfig
	require.NotEqual(t, first, second)
	var started []string
	for _, n := range []string{"aix1", "aix2", "aix3"} {
		if nodestate.Desired(h.node(n)) == second {
			started = append(started, n)
			assert.False(t, h.node(n).Spec.Unschedulable, "registry changes are applied without cordoning")
		}
	}
	assert.Equal(t, []string{"aix1", "aix2"}, started, "67%% of 3 rounds down to 2")
	assert.Empty(t, h.drainer.drained)
}

func TestPausedPoolStartsNothing(t *testing.T) {
	h := newHarness(t, aixNode("aix1"))
	p := defaultPool()
	p.Spec.Paused = true
	h.addPool(p)
	h.sync("aix")
	assert.Empty(t, nodestate.Desired(h.node("aix1")))
	assert.Equal(t, "Paused", cond(h.pool("aix"), amcov1alpha1.ConditionUpdating).Reason)
}

func TestDrainFailureDegradesAndRetries(t *testing.T) {
	h := newHarness(t, aixNode("aix1"))
	h.addPool(defaultPool())
	h.drainer.drainErr = errors.New("cannot evict pod web-0: PDB")
	h.sync("aix")
	assert.Empty(t, nodestate.Desired(h.node("aix1")))
	requeue := h.sync("aix")
	assert.Equal(t, time.Minute, requeue)
	c := cond(h.pool("aix"), amcov1alpha1.ConditionNodeDegraded)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Contains(t, c.Message, "drain failed: cannot evict pod web-0")

	h.drainer.drainErr = nil
	h.clock = h.clock.Add(2 * time.Minute)
	h.sync("aix")
	assert.NotEmpty(t, nodestate.Desired(h.node("aix1")))
	assert.Equal(t, []string{"aix1", "aix1"}, h.drainer.drained)
}

func TestAgentDegradedIsReported(t *testing.T) {
	h := newHarness(t, aixNode("aix1"))
	h.addPool(defaultPool())
	h.sync("aix")
	n := h.node("aix1")
	n.Annotations[nodestate.StateAnnotation] = nodestate.StateDegraded
	n.Annotations[nodestate.ReasonAnnotation] = "restarting kubelet: timed out"
	_, err := h.kube.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{})
	require.NoError(t, err)
	h.refresh()
	h.sync("aix")
	p := h.pool("aix")
	assert.Equal(t, int32(1), p.Status.DegradedMachineCount)
	assert.Contains(t, cond(p, amcov1alpha1.ConditionNodeDegraded).Message, "restarting kubelet: timed out")
	assert.True(t, h.node("aix1").Spec.Unschedulable, "a degraded node stays cordoned")
}

func TestOverlappingPoolsAndWorkerLabel(t *testing.T) {
	h := newHarness(t, aixNode("aix1", "tier=db"), aixNode("aix2", nodestate.WorkerRoleLabel))
	n := h.node("aix1")
	n.Labels["tier"] = "db"
	_, err := h.kube.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{})
	require.NoError(t, err)
	h.refresh()
	h.addPool(defaultPool())
	h.addPool(&amcov1alpha1.AIXConfigPool{ObjectMeta: metav1.ObjectMeta{Name: "db"}, Spec: amcov1alpha1.AIXConfigPoolSpec{
		NodeSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "db"}},
	}})
	h.sync("aix")
	p := h.pool("aix")
	assert.Equal(t, int32(1), p.Status.MachineCount)
	assert.Contains(t, cond(p, amcov1alpha1.ConditionNodeDegraded).Message, "node aix1 is also selected by pool(s) [db]")
	assert.Contains(t, strings.Join(p.Status.Warnings, "\n"), "node aix2 carries node-role.kubernetes.io/worker")
}

func TestRenderFailureKeepsLastTarget(t *testing.T) {
	h := newHarness(t, aixNode("aix1"))
	h.addPool(defaultPool())
	h.sync("aix")
	target := h.pool("aix").Status.RenderedConfig
	require.NoError(t, h.ccs.Delete(&mcfgv1.ControllerConfig{ObjectMeta: metav1.ObjectMeta{Name: ctrlcommon.ControllerConfigName}}))
	h.sync("aix")
	p := h.pool("aix")
	assert.Equal(t, target, p.Status.RenderedConfig)
	assert.Equal(t, metav1.ConditionTrue, cond(p, amcov1alpha1.ConditionRenderDegraded).Status)
	assert.Equal(t, metav1.ConditionTrue, cond(p, amcov1alpha1.ConditionDegraded).Status)
}

func TestGarbageCollectsUnusedRenderedConfigs(t *testing.T) {
	h := newHarness(t)
	h.addPool(defaultPool())
	h.op.cfg.KeepRendered = 1
	for i := 0; i < 4; i++ {
		_, err := h.kube.CoreV1().ConfigMaps(ns).Create(context.Background(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("rendered-aix-old%d", i), Namespace: ns, Labels: map[string]string{"amco.reuna-labs.io/pool": "aix"},
			CreationTimestamp: metav1.Unix(int64(i), 0),
		}}, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	h.refresh()
	h.sync("aix")
	cms, err := h.kube.CoreV1().ConfigMaps(ns).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	var names []string
	for _, cm := range cms.Items {
		names = append(names, cm.Name)
	}
	assert.ElementsMatch(t, []string{"rendered-aix-old3", h.pool("aix").Status.RenderedConfig}, names)
}

func TestMaxUnavailable(t *testing.T) {
	for _, tc := range []struct {
		spec  *intstr.IntOrString
		total int
		want  int
	}{
		{nil, 10, 1},
		{&intstr.IntOrString{IntVal: 3}, 10, 3},
		{&intstr.IntOrString{Type: intstr.String, StrVal: "10%"}, 5, 1},
		{&intstr.IntOrString{Type: intstr.String, StrVal: "50%"}, 5, 2},
		{&intstr.IntOrString{IntVal: 0}, 5, 1},
	} {
		got, err := maxUnavailable(tc.spec, tc.total)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got)
	}
}
