package operator

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/gilbahat/aix-machine-config-operator/pkg/nodestate"
	"github.com/gilbahat/aix-machine-config-operator/pkg/rendered"
)

// A node still on a v1 rendered config (written by the previous operator)
// must be able to move to the first v2 config.
func TestRegressionV1CurrentConfigBlocksRollout(t *testing.T) {
	v1 := "rendered-aix-1111111111111111"
	n1, n2 := aixNode("aix1"), aixNode("aix2")
	for _, n := range []*corev1.Node{n1, n2} {
		n.Annotations[nodestate.DesiredConfigAnnotation] = v1
		n.Annotations[nodestate.CurrentConfigAnnotation] = v1
		n.Annotations[nodestate.StateAnnotation] = nodestate.StateDone
	}
	h := newHarness(t, n1, n2)
	_, _ = h.kube.CoreV1().ConfigMaps(ns).Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: v1, Namespace: ns, Labels: map[string]string{rendered.PoolLabel: "aix"}},
		Immutable:  ptr.To(true),
		Data:       map[string]string{rendered.ConfigMapKey: `{"version":1,"pool":"aix","files":[{"path":"/etc/kubernetes/kubelet.conf","mode":420,"contents":"x","group":"kubelet"}]}`},
	}, metav1.CreateOptions{})
	h.refresh()
	h.addPool(defaultPool())
	_, err := h.op.syncPool(context.Background(), "aix")

	h.refresh()
	assert.NoError(t, err)
	assert.NotEqual(t, v1, nodestate.Desired(h.node("aix1")))
}
