package operator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/gilbahat/aix-machine-config-operator/pkg/nodestate"
)

// nodeConditions turns the agent's reports into node conditions, so that
// `oc describe node` and alerting see what waits for a reboot, what needs
// reprovisioning and what drifted. AMCO never reboots or reprovisions a node
// itself: the conditions are for operations and for the LPAR layer.
func nodeConditions(node *corev1.Node) []corev1.NodeCondition {
	cond := func(t string, on bool, reasonTrue, reasonFalse, msg string) corev1.NodeCondition {
		c := corev1.NodeCondition{Type: corev1.NodeConditionType(t), Status: corev1.ConditionFalse, Reason: reasonFalse}
		if on {
			c.Status, c.Reason, c.Message = corev1.ConditionTrue, reasonTrue, msg
		}
		return c
	}

	pending := nodestate.PendingReboot(node)
	var msg []string
	if len(pending) > 0 {
		msg = append(msg, fmt.Sprintf("%d item(s) take effect at the next boot: %s.", len(pending), strings.Join(pending, ", ")))
	}
	if b := nodestate.Bosboot(node); b != nil {
		switch b.State {
		case nodestate.BosbootRebuilt:
			msg = append(msg, fmt.Sprintf("The boot image was rebuilt (bosboot) at %s; the next boot uses it.", b.Time))
		case nodestate.BosbootFailed:
			msg = append(msg, fmt.Sprintf("bosboot failed at %s: %s.", b.Time, b.Error))
		}
	}
	if bt := node.Annotations[nodestate.BootTimeAnnotation]; bt != "" && len(msg) > 0 {
		msg = append(msg, "Up since "+bt+".")
	}
	reboot := cond(nodestate.ConditionRebootRequired, len(pending) > 0, "ChangesPendingReboot", "NoChangesPending", strings.Join(msg, " "))

	reprov := nodestate.Reprovision(node)
	reprovision := cond(nodestate.ConditionReprovisionRequired, len(reprov) > 0, "ItemsCannotConvergeInPlace", "AsExpected", describe(reprov))

	drifted := nodestate.Drift(node)
	drift := cond(nodestate.ConditionConfigDrift, len(drifted) > 0, "ConfigDrift", "NoDrift", describe(drifted))
	return []corev1.NodeCondition{reboot, reprovision, drift}
}

func describe(issues []nodestate.ItemIssue) string {
	var parts []string
	for _, i := range issues {
		parts = append(parts, i.Item+": "+i.Reason)
	}
	return strings.Join(parts, "; ")
}

// syncNodeConditions sets the AIX node conditions when they changed. Only
// nodes whose agent has reported in get them.
func (o *Operator) syncNodeConditions(ctx context.Context, node *corev1.Node) error {
	if !nodestate.HasAgent(node) {
		return nil
	}
	now := metav1.NewTime(o.now())
	var patch []corev1.NodeCondition
	for _, want := range nodeConditions(node) {
		var have *corev1.NodeCondition
		for i := range node.Status.Conditions {
			if node.Status.Conditions[i].Type == want.Type {
				have = &node.Status.Conditions[i]
			}
		}
		if have != nil && have.Status == want.Status && have.Reason == want.Reason && have.Message == want.Message {
			continue
		}
		want.LastHeartbeatTime, want.LastTransitionTime = now, now
		if have != nil && have.Status == want.Status {
			want.LastTransitionTime = have.LastTransitionTime
		}
		patch = append(patch, want)
	}
	if len(patch) == 0 {
		return nil
	}
	data, err := json.Marshal(map[string]interface{}{"status": map[string]interface{}{"conditions": patch}})
	if err != nil {
		return err
	}
	// Conditions merge by type, so the kubelet's own conditions are untouched.
	if _, err := o.kube.CoreV1().Nodes().Patch(ctx, node.Name, types.StrategicMergePatchType, data, metav1.PatchOptions{}, "status"); err != nil {
		return fmt.Errorf("setting conditions on node %s: %w", node.Name, err)
	}
	return nil
}
