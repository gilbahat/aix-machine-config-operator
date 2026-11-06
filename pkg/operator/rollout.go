package operator

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"

	"github.com/gilbahat/aix-machine-config-operator/pkg/nodestate"
	"github.com/gilbahat/aix-machine-config-operator/pkg/rendered"
)

// maxUnavailable resolves the pool's maxUnavailable against its size, with
// the MCO's rules: default 1, percentages round down, never less than 1.
func maxUnavailable(spec *intstr.IntOrString, total int) (int, error) {
	v := intstr.FromInt32(1)
	if spec != nil {
		v = *spec
	}
	n, err := intstr.GetScaledValueFromIntOrPercent(&v, total, false)
	if err != nil {
		return 0, err
	}
	if n < 1 {
		n = 1
	}
	return n, nil
}

// unavailable reports whether node counts against maxUnavailable.
func (o *Operator) unavailable(node *corev1.Node, target string) bool {
	if !nodestate.IsReady(node) || node.Spec.Unschedulable {
		return true
	}
	if nodestate.Desired(node) != nodestate.Current(node) || nodestate.State(node) == nodestate.StateWorking {
		return true
	}
	return o.draining(node.Name)
}

// draining reports whether a cordon-and-drain is in progress for node.
func (o *Operator) draining(node string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, ok := o.inflight[node]
	return ok
}

// finishedCordon reports whether AMCO cordoned node for the config it now
// runs, i.e. the update it was cordoned for is complete. While a drain is
// running or being retried the node is still on its old config (desired is
// not yet cordonedFor), so it stays cordoned.
func (o *Operator) finishedCordon(n *corev1.Node) bool {
	cordonedFor := n.Annotations[nodestate.CordonedByAnnotation]
	return cordonedFor != "" && cordonedFor == nodestate.Desired(n) && nodestate.IsDone(n, cordonedFor) && !o.draining(n.Name)
}

// uncordon lifts a cordon AMCO made and forgets it.
func (o *Operator) uncordon(ctx context.Context, n *corev1.Node) error {
	if err := o.drainer.Cordon(ctx, n, false); err != nil {
		return err
	}
	return o.setNodeAnnotations(ctx, n.Name, map[string]interface{}{nodestate.CordonedByAnnotation: nil})
}

// rollout moves the pool's nodes to st.target. It returns how soon the pool
// should be looked at again (for drain retries).
func (o *Operator) rollout(ctx context.Context, st *poolSync) (time.Duration, error) {
	st.drainErrors = map[string]string{}
	if st.target == "" {
		return 0, nil
	}
	// Finish nodes that are done: uncordon the ones AMCO cordoned.
	for i, n := range st.nodes {
		if o.finishedCordon(n) {
			klog.Infof("node %s finished updating to %s; uncordoning", n.Name, nodestate.Current(n))
			if err := o.uncordon(ctx, n); err != nil {
				return 0, err
			}
			// Count the node as available for the rest of this sync.
			n = n.DeepCopy()
			n.Spec.Unschedulable = false
			delete(n.Annotations, nodestate.CordonedByAnnotation)
			st.nodes[i] = n
		}
	}

	limit, err := maxUnavailable(st.pool.Spec.MaxUnavailable, len(st.nodes))
	if err != nil {
		return 0, fmt.Errorf("pool %s: invalid maxUnavailable: %w", st.pool.Name, err)
	}
	busy := 0
	var candidates []*corev1.Node
	for _, n := range st.nodes {
		if o.unavailable(n, st.target) {
			busy++
		}
		if nodestate.Desired(n) == st.target {
			continue
		}
		// Let an agent finish what it is doing; a degraded node may be
		// moved on to a new config, which is how it recovers.
		if nodestate.State(n) == nodestate.StateWorking {
			continue
		}
		// Do not cordon a node nobody will update: wait until its agent
		// has reported in.
		if !nodestate.HasAgent(n) {
			continue
		}
		candidates = append(candidates, n)
	}
	// Nodes that are already unavailable cost nothing extra, so they go first.
	sort.SliceStable(candidates, func(i, j int) bool {
		ui, uj := o.unavailable(candidates[i], st.target), o.unavailable(candidates[j], st.target)
		if ui != uj {
			return ui
		}
		return candidates[i].Name < candidates[j].Name
	})

	var requeue time.Duration
	o.reportDrainFailures(st)
	if st.pool.Spec.Paused {
		return 0, nil
	}
	for _, n := range candidates {
		if !o.unavailable(n, st.target) {
			if busy >= limit {
				break
			}
			busy++
		}
		wait, err := o.startUpdate(ctx, st, n)
		if err != nil {
			return 0, err
		}
		if wait > 0 && (requeue == 0 || wait < requeue) {
			requeue = wait
		}
	}
	return requeue, nil
}

// startUpdate moves one node towards st.target: directly when the change is
// live, or by cordoning and draining it first (asynchronously) when it is not.
func (o *Operator) startUpdate(ctx context.Context, st *poolSync, node *corev1.Node) (time.Duration, error) {
	target := st.target
	o.mu.Lock()
	if _, busy := o.inflight[node.Name]; busy {
		o.mu.Unlock()
		return 0, nil
	}
	if f, ok := o.failures[node.Name]; ok && f.target == target {
		if wait := f.at.Add(o.cfg.DrainRetryInterval).Sub(o.now()); wait > 0 {
			o.mu.Unlock()
			return wait, nil
		}
	}
	o.mu.Unlock()

	changes, drain, err := o.planFor(node, target)
	if err != nil {
		return 0, err
	}
	how := "in place"
	if drain {
		how = "after a drain"
	}
	klog.Infof("pool %s: updating node %s from %q to %s, %d item(s) %s", st.pool.Name, node.Name, nodestate.Current(node), target, len(changes), how)
	if !drain {
		return 0, o.setNodeAnnotations(ctx, node.Name, map[string]interface{}{nodestate.DesiredConfigAnnotation: target})
	}

	o.mu.Lock()
	o.inflight[node.Name] = target
	o.mu.Unlock()
	pool := st.pool.Name
	o.async(func() {
		err := o.cordonAndDrain(ctx, node.Name, target)
		o.mu.Lock()
		delete(o.inflight, node.Name)
		if err != nil {
			o.failures[node.Name] = drainFailure{target: target, err: err, at: o.now()}
		} else {
			delete(o.failures, node.Name)
		}
		o.mu.Unlock()
		if err != nil {
			klog.Errorf("pool %s: draining node %s failed (retrying in %s): %v", pool, node.Name, o.cfg.DrainRetryInterval, err)
			o.poolQueue.AddAfter(pool, o.cfg.DrainRetryInterval)
			return
		}
		o.poolQueue.Add(pool)
	})
	return 0, nil
}

func (o *Operator) cordonAndDrain(ctx context.Context, name string, target string) error {
	// Work from the node as it is now, not as it was when the drain was queued.
	node, err := o.kube.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	// Only remember the cordon if AMCO made it: an administrator's cordon
	// stays in place after the update.
	if !node.Spec.Unschedulable || node.Annotations[nodestate.CordonedByAnnotation] != "" {
		if err := o.setNodeAnnotations(ctx, node.Name, map[string]interface{}{nodestate.CordonedByAnnotation: target}); err != nil {
			return err
		}
		if err := o.drainer.Cordon(ctx, node, true); err != nil {
			return err
		}
	}
	if err := o.drainer.Drain(ctx, node.Name); err != nil {
		return err
	}
	return o.setNodeAnnotations(ctx, node.Name, map[string]interface{}{nodestate.DesiredConfigAnnotation: target})
}

// planFor works out which items moving node to target changes, and whether
// one of them asks for a drain. When the node's current config is unknown (a
// new node, or one whose config was garbage collected) every item counts as
// changed.
func (o *Operator) planFor(node *corev1.Node, target string) ([]string, bool, error) {
	to, err := o.renderedConfig(target)
	if err != nil {
		return nil, false, err
	}
	if to == nil {
		return nil, false, fmt.Errorf("rendered config %s not found", target)
	}
	from, err := o.renderedConfig(nodestate.Current(node))
	if err != nil {
		return nil, false, err
	}
	return rendered.Changes(from, to), rendered.DrainNeeded(from, to), nil
}

// reportDrainFailures copies the pool's drain failures into st. Failures are
// kept per node across pools and only forgotten when the node is gone (or no
// longer an AIX node) or a drain succeeds.
func (o *Operator) reportDrainFailures(st *poolSync) {
	o.mu.Lock()
	defer o.mu.Unlock()
	members := map[string]bool{}
	for _, n := range st.nodes {
		members[n.Name] = true
	}
	for name, f := range o.failures {
		if n, err := o.nodeLister.Get(name); err != nil || !nodestate.IsAIX(n) {
			delete(o.failures, name)
			continue
		}
		if members[name] && f.target == st.target {
			st.drainErrors[name] = f.err.Error()
		}
	}
}
