package operator

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/kubectl/pkg/drain"
)

// Drainer cordons and drains nodes.
type Drainer interface {
	Cordon(ctx context.Context, node *corev1.Node, cordon bool) error
	Drain(ctx context.Context, node string) error
}

// kubectlDrainer uses kubectl's drain library with the MCO's settings:
// DaemonSet pods stay, emptyDir data is discarded, unmanaged pods are deleted
// and pod disruption budgets are honoured (evictions are retried until the
// timeout).
type kubectlDrainer struct {
	kube    kubernetes.Interface
	timeout time.Duration
}

// NewKubectlDrainer returns the production Drainer.
func NewKubectlDrainer(kube kubernetes.Interface, timeout time.Duration) Drainer {
	return &kubectlDrainer{kube: kube, timeout: timeout}
}

func (d *kubectlDrainer) helper(ctx context.Context) *drain.Helper {
	return &drain.Helper{
		Ctx:                 ctx,
		Client:              d.kube,
		Force:               true,
		GracePeriodSeconds:  -1,
		IgnoreAllDaemonSets: true,
		DeleteEmptyDirData:  true,
		Timeout:             d.timeout,
		Out:                 klogWriter{},
		ErrOut:              klogWriter{err: true},
		OnPodDeletionOrEvictionFinished: func(pod *corev1.Pod, usingEviction bool, err error) {
			if err == nil {
				klog.V(2).Infof("drain: removed pod %s/%s (eviction=%t)", pod.Namespace, pod.Name, usingEviction)
			}
		},
	}
}

func (d *kubectlDrainer) Cordon(ctx context.Context, node *corev1.Node, cordon bool) error {
	return drain.RunCordonOrUncordon(d.helper(ctx), node, cordon)
}

func (d *kubectlDrainer) Drain(ctx context.Context, node string) error {
	klog.Infof("draining node %s", node)
	return drain.RunNodeDrain(d.helper(ctx), node)
}

type klogWriter struct{ err bool }

func (w klogWriter) Write(p []byte) (int, error) {
	if w.err {
		klog.Warning(string(p))
	} else {
		klog.Info(string(p))
	}
	return len(p), nil
}
