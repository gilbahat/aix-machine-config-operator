package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/gilbahat/aix-machine-config-operator/pkg/nodestate"
	"github.com/gilbahat/aix-machine-config-operator/pkg/rendered"
)

// Agent reconciles one node.
type Agent struct {
	Engine *Engine
	Kube   kubernetes.Interface
	// NodeName is the node this agent manages.
	NodeName string
	// Namespace holds the rendered ConfigMaps.
	Namespace string
	// Interval is how often drift is checked and failed applies retried.
	Interval time.Duration
	// Info is reported once at start-up (agent version, OS level).
	Info map[string]string

	queue workqueue.TypedRateLimitingInterface[string]
	nodes cache.Indexer
	// drift is set while the node is degraded because a managed item
	// changed behind the agent's back; only a new config, restoring the
	// item or the force file clears it.
	drift bool
}

// Host returns the agent's file host.
func (a *Agent) Host() Host { return a.Engine.Files }

// Run watches the node and reconciles until ctx is done.
func (a *Agent) Run(ctx context.Context) error {
	a.queue = workqueue.NewTypedRateLimitingQueueWithConfig(workqueue.DefaultTypedControllerRateLimiter[string](), workqueue.TypedRateLimitingQueueConfig[string]{Name: "node"})
	defer a.queue.ShutDown()

	if _, err := a.Engine.ReconcileBoot(); err != nil {
		klog.Errorf("checking pending-reboot items against the boot time: %v", err)
	}
	{
		ann := map[string]interface{}{}
		for k, v := range a.Info {
			ann[k] = v
		}
		if b := a.Engine.bootKey(); b != "" {
			ann[nodestate.BootTimeAnnotation] = b
		}
		if err := a.addReport(ann); err != nil {
			klog.Errorf("reading agent state: %v", err)
		}
		// At boot the agent may start before the API server is reachable or
		// before the kubelet has registered the node; keep trying.
		for backoff := time.Second; ; backoff = min(2*backoff, time.Minute) {
			err := a.annotate(ctx, ann)
			if err == nil {
				break
			}
			klog.Warningf("reporting agent start-up on node %s (retrying in %s): %v", a.NodeName, backoff, err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
		}
	}

	lw := cache.NewListWatchFromClient(a.Kube.CoreV1().RESTClient(), "nodes", metav1.NamespaceAll, fields.OneTermEqualSelector("metadata.name", a.NodeName))
	inf := cache.NewSharedIndexInformer(lw, &corev1.Node{}, 0, cache.Indexers{})
	a.nodes = inf.GetIndexer()
	enqueue := func(interface{}) { a.queue.Add(a.NodeName) }
	if _, err := inf.AddEventHandler(cache.ResourceEventHandlerFuncs{AddFunc: enqueue, UpdateFunc: func(_, o interface{}) { enqueue(o) }}); err != nil {
		return err
	}
	go inf.Run(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), inf.HasSynced) {
		return fmt.Errorf("node informer did not sync")
	}
	go func() {
		t := time.NewTicker(a.Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				a.queue.Add(a.NodeName)
			}
		}
	}()
	klog.Infof("agent watching node %s", a.NodeName)
	for {
		key, quit := a.queue.Get()
		if quit {
			return nil
		}
		if err := a.reconcile(ctx); err != nil {
			klog.Errorf("reconciling node %s: %v", a.NodeName, err)
			a.queue.AddRateLimited(key)
		} else {
			a.queue.Forget(key)
		}
		a.queue.Done(key)
		if ctx.Err() != nil {
			return nil
		}
	}
}

func (a *Agent) node() (*corev1.Node, error) {
	obj, ok, err := a.nodes.GetByKey(a.NodeName)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("node %s not found", a.NodeName)
	}
	return obj.(*corev1.Node), nil
}

// reconcile brings the host to the node's desired config.
func (a *Agent) reconcile(ctx context.Context) error {
	node, err := a.node()
	if err != nil {
		return err
	}
	return a.Reconcile(ctx, node)
}

// Reconcile is one pass of the agent's state machine for node:
//
//   - desired unset: nothing to do yet.
//   - force file present, or desired differs from what the host last
//     applied: apply it (state Working, then Done or Degraded).
//   - otherwise: check for drift; report Degraded on drift and Done once the
//     files match again. Drift is never repaired automatically, as in the
//     MCO: someone changed a managed file and should decide.
func (a *Agent) Reconcile(ctx context.Context, node *corev1.Node) error {
	desired := nodestate.Desired(node)
	if desired == "" {
		return nil
	}
	st, err := a.Host().LoadState()
	if err != nil {
		return a.degrade(ctx, fmt.Errorf("reading applied state: %w", err))
	}
	force := a.Host().ForceRequested()
	upToDate := st.Pending == nil && st.Config != nil && st.Name == desired && nodestate.Current(node) == desired
	if !force && upToDate {
		// The applied state is only recorded after a successful apply, so
		// the only thing left to check is whether items drifted.
		return a.checkDrift(ctx, node)
	}
	// Applying compares every item with its live state, so a new config
	// also repairs drifted items; only the force file re-runs every action.
	if force {
		klog.Infof("%s present: re-applying %s in full", ForceFile, desired)
	}
	if err := a.apply(ctx, node, desired, force); err != nil {
		return a.degrade(ctx, err)
	}
	if force {
		if err := a.Host().ClearForce(); err != nil {
			klog.Warningf("removing %s: %v", ForceFile, err)
		}
	}
	return nil
}

func (a *Agent) apply(ctx context.Context, node *corev1.Node, desired string, force bool) error {
	if nodestate.State(node) != nodestate.StateWorking {
		if err := a.annotate(ctx, map[string]interface{}{nodestate.StateAnnotation: nodestate.StateWorking, nodestate.ReasonAnnotation: nil}); err != nil {
			return err
		}
	}
	cm, err := a.Kube.CoreV1().ConfigMaps(a.Namespace).Get(ctx, desired, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("fetching rendered config %s: %w", desired, err)
	}
	to, err := verifyRendered(cm, desired)
	if err != nil {
		return err
	}
	if _, err := a.Engine.Apply(ctx, desired, to, ApplyOptions{Force: force}); err != nil {
		_ = a.reportOnly(ctx)
		return err
	}
	a.drift = false
	ann := map[string]interface{}{
		nodestate.CurrentConfigAnnotation: desired,
		nodestate.StateAnnotation:         nodestate.StateDone,
		nodestate.ReasonAnnotation:        nil,
		nodestate.DriftAnnotation:         nil,
	}
	if err := a.addReport(ann); err != nil {
		return err
	}
	return a.annotate(ctx, ann)
}

// addReport adds the pending-reboot, reprovision and bosboot annotations
// from the agent's state to ann (nil values remove empty reports).
func (a *Agent) addReport(ann map[string]interface{}) error {
	st, err := a.Host().LoadState()
	if err != nil {
		return err
	}
	setJSON(ann, nodestate.PendingRebootAnnotation, st.PendingRebootKeys())
	setJSON(ann, nodestate.ReprovisionAnnotation, st.Reprovision)
	if st.Bosboot != nil {
		setJSON(ann, nodestate.BosbootAnnotation, st.Bosboot)
	} else {
		ann[nodestate.BosbootAnnotation] = nil
	}
	return nil
}

// reportOnly refreshes the report annotations, e.g. after a failed apply.
func (a *Agent) reportOnly(ctx context.Context) error {
	ann := map[string]interface{}{}
	if err := a.addReport(ann); err != nil {
		return err
	}
	return a.annotate(ctx, ann)
}

// setJSON sets key to the JSON of v, or removes it when v is an empty list.
func setJSON[T any](ann map[string]interface{}, key string, v T) {
	data, err := json.Marshal(v)
	if err != nil || string(data) == "null" || string(data) == "[]" {
		ann[key] = nil
		return
	}
	ann[key] = string(data)
}

// verifyRendered checks that a ConfigMap is a genuine rendered config: the
// operator creates them immutable, labelled with their pool and named after
// their content hash, so a ConfigMap that is mutable, unlabelled or whose
// content does not hash to its name is refused.
func verifyRendered(cm *corev1.ConfigMap, name string) (*rendered.Config, error) {
	if cm.Immutable == nil || !*cm.Immutable {
		return nil, fmt.Errorf("rendered config %s is not immutable; refusing it", name)
	}
	if cm.Labels[rendered.PoolLabel] == "" {
		return nil, fmt.Errorf("rendered config %s has no %s label; refusing it", name, rendered.PoolLabel)
	}
	cfg, err := rendered.Unmarshal([]byte(cm.Data[rendered.ConfigMapKey]))
	if err != nil {
		return nil, fmt.Errorf("rendered config %s: %w", name, err)
	}
	if got, err := cfg.Name(); err != nil || got != name {
		return nil, fmt.Errorf("rendered config %s: content does not match its name (hashes to %s); refusing it", name, got)
	}
	return cfg, nil
}

// checkDrift compares every applied item's live state with the applied
// config and reports the result. It never changes the host.
func (a *Agent) checkDrift(ctx context.Context, node *corev1.Node) error {
	issues, err := a.Engine.Drift(ctx)
	if err != nil {
		return a.degrade(ctx, fmt.Errorf("checking for drift: %w", err))
	}
	ann := map[string]interface{}{}
	if err := a.addReport(ann); err != nil {
		return err
	}
	setJSON(ann, nodestate.DriftAnnotation, capIssues(issues))
	if len(issues) > 0 {
		a.drift = true
		var parts []string
		for _, i := range issues {
			parts = append(parts, i.Item+": "+i.Reason)
		}
		msg := truncate(fmt.Sprintf("configuration drift: %s (restore the items or create %s to re-apply)", strings.Join(parts, "; "), ForceFile))
		ann[nodestate.StateAnnotation], ann[nodestate.ReasonAnnotation] = nodestate.StateDegraded, msg
		if !annotationsMatch(node, ann) {
			klog.Warning(msg)
			return a.annotate(ctx, ann)
		}
		return nil
	}
	if a.drift || nodestate.State(node) != nodestate.StateDone {
		a.drift = false
		klog.Info("managed items match the applied config again")
	}
	ann[nodestate.StateAnnotation], ann[nodestate.ReasonAnnotation] = nodestate.StateDone, nil
	if annotationsMatch(node, ann) {
		return nil
	}
	return a.annotate(ctx, ann)
}

// maxReportedIssues keeps the drift annotation, and so the Node, small.
const maxReportedIssues = 50

func capIssues(issues []nodestate.ItemIssue) []nodestate.ItemIssue {
	if len(issues) <= maxReportedIssues {
		return issues
	}
	out := append([]nodestate.ItemIssue(nil), issues[:maxReportedIssues-1]...)
	return append(out, nodestate.ItemIssue{Item: "...", Reason: fmt.Sprintf("%d more items drifted", len(issues)-maxReportedIssues+1)})
}

// annotationsMatch reports whether node already carries ann (nil = absent),
// so a periodic check does not patch the node when nothing changed.
func annotationsMatch(node *corev1.Node, ann map[string]interface{}) bool {
	for k, v := range ann {
		got, ok := node.Annotations[k]
		if v == nil {
			if ok {
				return false
			}
			continue
		}
		if !ok || got != v.(string) {
			return false
		}
	}
	return true
}

func (a *Agent) degrade(ctx context.Context, cause error) error {
	klog.Errorf("node degraded: %v", cause)
	if err := a.annotate(ctx, map[string]interface{}{nodestate.StateAnnotation: nodestate.StateDegraded, nodestate.ReasonAnnotation: truncate(cause.Error())}); err != nil {
		return fmt.Errorf("%v (and reporting it failed: %w)", cause, err)
	}
	return cause
}

func truncate(s string) string {
	const max = 1024
	if len(s) > max {
		return s[:max-3] + "..."
	}
	return s
}

// annotate merge-patches the node's annotations; nil values remove keys.
func (a *Agent) annotate(ctx context.Context, ann map[string]interface{}) error {
	patch, err := json.Marshal(map[string]interface{}{"metadata": map[string]interface{}{"annotations": ann}})
	if err != nil {
		return err
	}
	_, err = a.Kube.CoreV1().Nodes().Patch(ctx, a.NodeName, types.MergePatchType, patch, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("node %s does not exist (has the kubelet registered it?)", a.NodeName)
	}
	return err
}
