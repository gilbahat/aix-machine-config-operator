package operator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	"github.com/gilbahat/aix-machine-config-operator/pkg/nodestate"
)

// syncNode registers an AIX node: it adds the aix role label, which is what
// the default AIXConfigPool selects. The kubelet cannot set node-role labels
// itself (the NodeRestriction admission plugin forbids it), so this is the
// operator's job, as setting the worker role is the machine-approver's and
// WMCO's on their nodes.
//
// AIX nodes are deliberately not given the worker role: the stock MCO's worker
// MachineConfigPool selects node-role.kubernetes.io/worker, and an AIX node in
// that pool would never finish an update (no machine-config-daemon runs on
// it). If an administrator adds the worker label anyway, the pool reports it.
func (o *Operator) syncNode(ctx context.Context, name string) (time.Duration, error) {
	node, err := o.nodeLister.Get(name)
	if apierrors.IsNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !nodestate.IsAIX(node) {
		return 0, nil
	}
	if _, ok := node.Labels[nodestate.RoleLabel]; !ok {
		klog.Infof("registering AIX node %s with role label %s", node.Name, nodestate.RoleLabel)
		if err := o.patchNodeMeta(ctx, node.Name, map[string]interface{}{"labels": map[string]interface{}{nodestate.RoleLabel: ""}}); err != nil {
			return 0, err
		}
		node = node.DeepCopy()
		node.Labels[nodestate.RoleLabel] = ""
	}
	// Release an orphaned cordon even if the conditions cannot be set.
	cordonErr := o.releaseOrphanedCordon(ctx, node)
	if err := o.syncNodeConditions(ctx, node); err != nil {
		return 0, errors.Join(cordonErr, err)
	}
	return 0, cordonErr
}

// releaseOrphanedCordon uncordons a node AMCO cordoned that no pool manages
// any more (it was relabelled, its pool was deleted, or two pools now select
// it), since no pool sync would ever finish its update.
func (o *Operator) releaseOrphanedCordon(ctx context.Context, node *corev1.Node) error {
	if node.Annotations[nodestate.CordonedByAnnotation] == "" || o.draining(node.Name) {
		return nil
	}
	pools, err := o.poolsSelecting(node)
	if err != nil {
		return err
	}
	if len(pools) == 1 {
		return nil
	}
	klog.Infof("node %s was cordoned by AMCO but is selected by %d pools; uncordoning", node.Name, len(pools))
	return o.uncordon(ctx, node)
}

// poolsSelecting returns the names of the pools whose selector matches node.
func (o *Operator) poolsSelecting(node *corev1.Node) ([]string, error) {
	var out []string
	for _, p := range o.listPools() {
		sel, err := poolSelector(p)
		if err != nil {
			continue
		}
		if sel.Matches(labels.Set(node.Labels)) {
			out = append(out, p.Name)
		}
	}
	return out, nil
}

func (o *Operator) enqueueAIXNodes() {
	if o.nodeLister == nil {
		return
	}
	nodes, err := o.nodeLister.List(labels.SelectorFromSet(labels.Set{nodestate.OSLabel: nodestate.OSAIX}))
	if err != nil {
		return
	}
	for _, n := range nodes {
		o.nodeQueue.Add(n.Name)
	}
}

// patchNodeMeta merge-patches a node's metadata; a nil value removes the key.
func (o *Operator) patchNodeMeta(ctx context.Context, node string, meta map[string]interface{}) error {
	patch, err := json.Marshal(map[string]interface{}{"metadata": meta})
	if err != nil {
		return err
	}
	if _, err := o.kube.CoreV1().Nodes().Patch(ctx, node, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("patching node %s: %w", node, err)
	}
	return nil
}

func (o *Operator) setNodeAnnotations(ctx context.Context, node string, ann map[string]interface{}) error {
	return o.patchNodeMeta(ctx, node, map[string]interface{}{"annotations": ann})
}

// workerLabelled lists pool nodes that also carry the worker role label.
func workerLabelled(nodes []*corev1.Node) []string {
	var out []string
	for _, n := range nodes {
		if _, ok := n.Labels[nodestate.WorkerRoleLabel]; ok {
			out = append(out, n.Name)
		}
	}
	return out
}
