package operator

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	amcov1alpha1 "github.com/gilbahat/aix-machine-config-operator/pkg/apis/amco/v1alpha1"
	"github.com/gilbahat/aix-machine-config-operator/pkg/nodestate"
)

// computeStatus derives the pool status from the sync state. It follows the
// MachineConfigPool status: counts per node state and the Updated, Updating,
// Degraded, RenderDegraded and NodeDegraded conditions.
func (o *Operator) computeStatus(st *poolSync) amcov1alpha1.AIXConfigPoolStatus {
	pool := st.pool
	status := *pool.Status.DeepCopy()
	status.ObservedGeneration = pool.Generation
	status.RenderedConfig = st.target
	status.MachineCount = int32(len(st.nodes))
	status.UpdatedMachineCount, status.ReadyMachineCount, status.UnavailableMachineCount, status.DegradedMachineCount = 0, 0, 0, 0

	var degraded []string
	for _, n := range st.nodes {
		updated := nodestate.IsDone(n, st.target)
		if updated {
			status.UpdatedMachineCount++
			if nodestate.IsReady(n) && !n.Spec.Unschedulable {
				status.ReadyMachineCount++
			}
		}
		if o.unavailable(n, st.target) {
			status.UnavailableMachineCount++
		}
		if nodestate.State(n) == nodestate.StateDegraded {
			status.DegradedMachineCount++
			degraded = append(degraded, fmt.Sprintf("node %s: %s", n.Name, n.Annotations[nodestate.ReasonAnnotation]))
		}
	}
	for name, err := range st.drainErrors {
		degraded = append(degraded, fmt.Sprintf("node %s: drain failed: %s", name, err))
	}
	degraded = append(degraded, st.conflicts...)
	sort.Strings(degraded)

	status.Warnings = append([]string(nil), st.warnings...)
	for _, n := range workerLabelled(st.nodes) {
		status.Warnings = append(status.Warnings, fmt.Sprintf("node %s carries %s; the stock MCO's worker pool will select it and wait for a machine-config-daemon that never runs on AIX. Remove the label.", n, nodestate.WorkerRoleLabel))
	}
	if len(status.Warnings) == 0 {
		status.Warnings = nil
	}

	set := func(t string, on bool, reason, msg string) {
		s := metav1.ConditionFalse
		if on {
			s = metav1.ConditionTrue
		}
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: t, Status: s, Reason: reason, Message: msg, ObservedGeneration: pool.Generation})
	}

	if st.renderErr != nil {
		set(amcov1alpha1.ConditionRenderDegraded, true, "RenderFailed", st.renderErr.Error())
	} else {
		set(amcov1alpha1.ConditionRenderDegraded, false, "Rendered", "")
	}
	if len(degraded) > 0 {
		set(amcov1alpha1.ConditionNodeDegraded, true, "NodeDegraded", strings.Join(degraded, "; "))
	} else {
		set(amcov1alpha1.ConditionNodeDegraded, false, "AsExpected", "")
	}
	if st.renderErr != nil || len(degraded) > 0 {
		set(amcov1alpha1.ConditionDegraded, true, "Degraded", "see RenderDegraded and NodeDegraded")
	} else {
		set(amcov1alpha1.ConditionDegraded, false, "AsExpected", "")
	}

	all := st.target != "" && status.UpdatedMachineCount == status.MachineCount
	switch {
	case all:
		msg := fmt.Sprintf("All nodes are updated with %s", st.target)
		set(amcov1alpha1.ConditionUpdated, true, "AllNodesUpdated", msg)
		set(amcov1alpha1.ConditionUpdating, false, "AllNodesUpdated", msg)
	case pool.Spec.Paused:
		msg := fmt.Sprintf("Pool is paused; %d of %d nodes run %s", status.UpdatedMachineCount, status.MachineCount, st.target)
		set(amcov1alpha1.ConditionUpdated, false, "Paused", msg)
		set(amcov1alpha1.ConditionUpdating, false, "Paused", msg)
	default:
		msg := fmt.Sprintf("%d of %d nodes run %s", status.UpdatedMachineCount, status.MachineCount, st.target)
		if st.target == "" {
			msg = "no configuration has been rendered yet"
		}
		set(amcov1alpha1.ConditionUpdated, false, "Updating", msg)
		set(amcov1alpha1.ConditionUpdating, true, "Updating", msg)
	}
	return status
}

func (o *Operator) updateStatus(ctx context.Context, st *poolSync) error {
	status := o.computeStatus(st)
	if equality.Semantic.DeepEqual(status, st.pool.Status) {
		return nil
	}
	p := st.pool.DeepCopy()
	p.Status = status
	updated, err := o.pools.UpdateStatus(ctx, p)
	if err != nil {
		return fmt.Errorf("updating status of pool %s: %w", p.Name, err)
	}
	st.pool = updated
	return nil
}
