package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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

// Restarter restarts node services; on AIX it is the SRC.
type Restarter interface {
	Restart(ctx context.Context, subsystem string) error
}

// ErrServiceNotDefined is returned (wrapped) by a Restarter when the service
// is not registered on the host. The agent tolerates it, so a node can be
// tested before the kubelet port is installed.
var ErrServiceNotDefined = errors.New("service is not defined on this host")

// Agent reconciles one node.
type Agent struct {
	Host      Host
	Kube      kubernetes.Interface
	Restarter Restarter
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
	// drift is set while the node is degraded because a managed file changed
	// behind the agent's back; only a new config or the force file clears it.
	drift bool
}

// Run watches the node and reconciles until ctx is done.
func (a *Agent) Run(ctx context.Context) error {
	a.queue = workqueue.NewTypedRateLimitingQueueWithConfig(workqueue.DefaultTypedControllerRateLimiter[string](), workqueue.TypedRateLimitingQueueConfig[string]{Name: "node"})
	defer a.queue.ShutDown()

	if len(a.Info) > 0 {
		ann := map[string]interface{}{}
		for k, v := range a.Info {
			ann[k] = v
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
	applied, err := a.Host.LoadApplied()
	if err != nil {
		return a.degrade(ctx, fmt.Errorf("reading applied state: %w", err))
	}
	force := a.Host.ForceRequested()
	upToDate := applied != nil && applied.Pending == nil && applied.Config != nil && applied.Name == desired && nodestate.Current(node) == desired
	if !force && upToDate {
		// The applied state is only recorded after a successful apply, so
		// the only thing left to check is whether the files drifted.
		return a.checkDrift(ctx, node, applied)
	}
	// A new config rewrites everything when the node has drifted, so the
	// update also repairs the drift instead of reporting Done over it.
	full := force || a.drift
	if !full && applied != nil && applied.Config != nil && len(a.Host.Drift(applied.Config)) > 0 {
		full = true
	}
	if force {
		klog.Infof("%s present: re-applying %s in full", ForceFile, desired)
	}
	if err := a.apply(ctx, node, desired, full); err != nil {
		return a.degrade(ctx, err)
	}
	if force {
		if err := a.Host.ClearForce(); err != nil {
			klog.Warningf("removing %s: %v", ForceFile, err)
		}
	}
	return nil
}

func (a *Agent) apply(ctx context.Context, node *corev1.Node, desired string, full bool) error {
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
	if err := ApplyConfig(ctx, a.Host, a.Restarter, desired, to, full); err != nil {
		return err
	}
	a.drift = false
	return a.annotate(ctx, map[string]interface{}{
		nodestate.CurrentConfigAnnotation: desired,
		nodestate.StateAnnotation:         nodestate.StateDone,
		nodestate.ReasonAnnotation:        nil,
	})
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

// ApplyConfig moves the host to cfg and restarts the affected services. It
// is shared by the cluster loop and the standalone `amco-agent apply`
// command. A nil restarter skips restarts.
//
// The change is computed from the last fully applied config, unless full is
// set or a previous apply did not finish, in which case every file is
// rewritten and every service whose files are involved is restarted. Files
// the agent wrote before and that cfg no longer lists are removed.
func ApplyConfig(ctx context.Context, host Host, restarter Restarter, name string, cfg *rendered.Config, full bool) error {
	state, err := host.LoadApplied()
	if err != nil {
		return err
	}
	owned := state.Owned()
	var from *rendered.Config
	if state != nil && state.Pending == nil && !full {
		from = state.Config
	}
	if state != nil && state.Pending != nil {
		klog.Info("the previous apply did not finish; re-applying in full")
	}
	plan := rendered.Diff(from, cfg)

	// Removals and their policies come from everything the agent may have
	// written, including a half-applied config.
	keep := map[string]bool{}
	for _, f := range cfg.Files {
		keep[f.Path] = true
	}
	groups := map[rendered.Group]bool{}
	for _, f := range plan.Write {
		groups[f.Group] = true
	}
	plan.Remove = nil
	for p, f := range owned {
		if !keep[p] {
			plan.Remove = append(plan.Remove, p)
			groups[f.Group] = true
		}
	}
	sort.Strings(plan.Remove)
	plan.Drain, plan.Restart = rendered.PolicyFor(groups)

	klog.Infof("applying %s: %s", name, plan)
	pending := &Applied{Pending: cfg}
	if state != nil {
		pending.Name, pending.Config = state.Name, state.Config
	}
	if err := host.SaveApplied(pending); err != nil {
		return fmt.Errorf("recording apply in progress: %w", err)
	}
	if err := host.Apply(plan, owned); err != nil {
		return fmt.Errorf("writing files: %w", err)
	}
	if err := restart(ctx, restarter, plan.Restart); err != nil {
		return err
	}
	return host.SaveApplied(&Applied{Name: name, Config: cfg})
}

func restart(ctx context.Context, restarter Restarter, subsystems []string) error {
	if restarter == nil {
		if len(subsystems) > 0 {
			klog.Infof("not restarting %s (restarts disabled)", strings.Join(subsystems, ", "))
		}
		return nil
	}
	for _, s := range subsystems {
		klog.Infof("restarting %s", s)
		if err := restarter.Restart(ctx, s); err != nil {
			if errors.Is(err, ErrServiceNotDefined) {
				klog.Warningf("%s is not defined on this host; skipping its restart", s)
				continue
			}
			return fmt.Errorf("restarting %s: %w", s, err)
		}
	}
	return nil
}

func (a *Agent) checkDrift(ctx context.Context, node *corev1.Node, applied *Applied) error {
	drift := a.Host.Drift(applied.Config)
	if len(drift) > 0 {
		a.drift = true
		msg := fmt.Sprintf("configuration drift: %s (restore the files or create %s to re-apply)", strings.Join(drift, "; "), ForceFile)
		if nodestate.State(node) != nodestate.StateDegraded || node.Annotations[nodestate.ReasonAnnotation] != truncate(msg) {
			klog.Warning(msg)
			return a.annotate(ctx, map[string]interface{}{nodestate.StateAnnotation: nodestate.StateDegraded, nodestate.ReasonAnnotation: truncate(msg)})
		}
		return nil
	}
	if a.drift || nodestate.State(node) != nodestate.StateDone {
		a.drift = false
		klog.Info("managed files match the applied config again")
		return a.annotate(ctx, map[string]interface{}{nodestate.StateAnnotation: nodestate.StateDone, nodestate.ReasonAnnotation: nil})
	}
	return nil
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
