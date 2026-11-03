package operator

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
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
	o.mu.Lock()
	_, draining := o.inflight[node.Name]
	o.mu.Unlock()
	return draining
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
		if cordonedFor := n.Annotations[nodestate.CordonedByAnnotation]; cordonedFor != "" && nodestate.IsDone(n, nodestate.Desired(n)) {
			klog.Infof("node %s finished updating to %s; uncordoning", n.Name, nodestate.Current(n))
			if err := o.drainer.Cordon(ctx, n, false); err != nil {
				return 0, err
			}
			if err := o.setNodeAnnotations(ctx, n.Name, map[string]interface{}{nodestate.CordonedByAnnotation: nil}); err != nil {
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

	plan, err := o.planFor(node, target)
	if err != nil {
		return 0, err
	}
	klog.Infof("pool %s: updating node %s from %q to %s (%s)", st.pool.Name, node.Name, nodestate.Current(node), target, plan)
	if !plan.Drain {
		return 0, o.setNodeAnnotations(ctx, node.Name, map[string]interface{}{nodestate.DesiredConfigAnnotation: target})
	}

	o.mu.Lock()
	o.inflight[node.Name] = target
	o.mu.Unlock()
	pool := st.pool.Name
	o.async(func() {
		err := o.cordonAndDrain(ctx, node, target)
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
		}
		o.poolQueue.Add(pool)
	})
	return 0, nil
}

func (o *Operator) cordonAndDrain(ctx context.Context, node *corev1.Node, target string) error {
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

// planFor works out what moving node to target involves. When the node's
// current config is unknown (a new node, or one whose config was garbage
// collected) the plan assumes every file changes.
func (o *Operator) planFor(node *corev1.Node, target string) (rendered.Plan, error) {
	to, err := o.renderedConfig(target)
	if err != nil {
		return rendered.Plan{}, err
	}
	if to == nil {
		return rendered.Plan{}, fmt.Errorf("rendered config %s not found", target)
	}
	from, err := o.renderedConfig(nodestate.Current(node))
	if err != nil {
		return rendered.Plan{}, err
	}
	return rendered.Diff(from, to), nil
}

func (o *Operator) reportDrainFailures(st *poolSync) {
	o.mu.Lock()
	defer o.mu.Unlock()
	members := map[string]bool{}
	for _, n := range st.nodes {
		members[n.Name] = true
	}
	for name, f := range o.failures {
		if !members[name] {
			delete(o.failures, name)
			continue
		}
		if f.target == st.target {
			st.drainErrors[name] = f.err.Error()
		}
	}
}
