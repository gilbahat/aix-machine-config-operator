package operator

import (
	"context"
	"fmt"
	"sort"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"
	ctrlcommon "github.com/openshift/machine-config-operator/pkg/controller/common"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	amcov1alpha1 "github.com/gilbahat/aix-machine-config-operator/pkg/apis/amco/v1alpha1"
	"github.com/gilbahat/aix-machine-config-operator/pkg/nodestate"
	"github.com/gilbahat/aix-machine-config-operator/pkg/render"
	"github.com/gilbahat/aix-machine-config-operator/pkg/rendered"
)

// clusterSingleton is the name of the cluster-scoped config singletons.
const clusterSingleton = "cluster"

// syncPool renders, rolls out and reports one pool.
func (o *Operator) syncPool(ctx context.Context, name string) (time.Duration, error) {
	pool, ok := o.getPool(name)
	if !ok {
		return 0, nil
	}
	pool = pool.DeepCopy()

	members, conflicts, err := o.poolMembers(pool)
	if err != nil {
		return 0, err
	}

	st := poolSync{pool: pool, nodes: members, conflicts: conflicts}
	st.target = pool.Status.RenderedConfig
	var storeErr error
	if res, err := o.render(pool); err != nil {
		st.renderErr = err
	} else {
		st.warnings = res.Warnings
		target, err := o.ensureRendered(ctx, pool, res.Config)
		if err != nil {
			st.renderErr, storeErr = err, err
		} else {
			st.target = target
		}
	}

	requeue, rollErr := o.rollout(ctx, &st)
	if err := o.updateStatus(ctx, &st); err != nil {
		return 0, err
	}
	if storeErr != nil {
		return 0, storeErr
	}
	if err := o.gcRendered(ctx, pool, st.target); err != nil {
		klog.Warningf("pool %s: garbage-collecting rendered configs: %v", pool.Name, err)
	}
	return requeue, rollErr
}

// poolSync carries one pool's state through a sync.
type poolSync struct {
	pool      *amcov1alpha1.AIXConfigPool
	nodes     []*corev1.Node
	conflicts []string
	target    string
	renderErr error
	warnings  []string
	// drainErrors are per-node drain failures, reported as NodeDegraded.
	drainErrors map[string]string
}

// poolSelector returns the pool's node selector, defaulting to the aix role.
func poolSelector(pool *amcov1alpha1.AIXConfigPool) (labels.Selector, error) {
	if pool.Spec.NodeSelector == nil {
		return labels.SelectorFromSet(labels.Set{nodestate.RoleLabel: ""}), nil
	}
	return metav1.LabelSelectorAsSelector(pool.Spec.NodeSelector)
}

// poolMembers returns the AIX nodes the pool selects. A node selected by more
// than one pool belongs to none of them and is reported as a conflict, as the
// MCO does for nodes matching several custom pools.
func (o *Operator) poolMembers(pool *amcov1alpha1.AIXConfigPool) ([]*corev1.Node, []string, error) {
	sel, err := poolSelector(pool)
	if err != nil {
		return nil, nil, fmt.Errorf("pool %s: invalid nodeSelector: %w", pool.Name, err)
	}
	all, err := o.nodeLister.List(labels.Everything())
	if err != nil {
		return nil, nil, err
	}
	others := map[string]labels.Selector{}
	for _, p := range o.listPools() {
		if p.Name == pool.Name {
			continue
		}
		if s, err := poolSelector(p); err == nil {
			others[p.Name] = s
		}
	}
	var members []*corev1.Node
	var conflicts []string
	for _, n := range all {
		if !nodestate.IsAIX(n) || !sel.Matches(labels.Set(n.Labels)) {
			continue
		}
		var also []string
		for name, s := range others {
			if s.Matches(labels.Set(n.Labels)) {
				also = append(also, name)
			}
		}
		if len(also) > 0 {
			sort.Strings(also)
			conflicts = append(conflicts, fmt.Sprintf("node %s is also selected by pool(s) %v and is not managed", n.Name, also))
			continue
		}
		members = append(members, n)
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })
	sort.Strings(conflicts)
	return members, conflicts, nil
}

// maxPoolNameLength keeps the pool name usable as a label value, which the
// rendered ConfigMaps carry.
const maxPoolNameLength = 63

// render gathers the pool's inputs from the listers and renders them.
func (o *Operator) render(pool *amcov1alpha1.AIXConfigPool) (*render.Result, error) {
	if len(pool.Name) > maxPoolNameLength {
		return nil, fmt.Errorf("pool name %q is longer than %d characters", pool.Name, maxPoolNameLength)
	}
	in := render.Inputs{Pool: pool}
	cc, err := o.ccLister.Get(ctrlcommon.ControllerConfigName)
	if err != nil {
		return nil, fmt.Errorf("getting ControllerConfig %s: %w", ctrlcommon.ControllerConfigName, err)
	}
	in.ControllerConfig = cc
	if in.Image, err = optional(o.imageLister.Get(clusterSingleton)); err != nil {
		return nil, err
	}
	if in.APIServer, err = optional(o.apiServerLister.Get(clusterSingleton)); err != nil {
		return nil, err
	}
	if in.ImageDigestMirrorSets, err = o.idmsLister.List(labels.Everything()); err != nil {
		return nil, err
	}
	if in.ImageTagMirrorSets, err = o.itmsLister.List(labels.Everything()); err != nil {
		return nil, err
	}
	if in.KubeletConfigs, err = o.kcLister.List(labels.Everything()); err != nil {
		return nil, err
	}
	if in.ContainerRuntimeConfigs, err = o.crcLister.List(labels.Everything()); err != nil {
		return nil, err
	}
	sortByName(in.ImageDigestMirrorSets, func(x *configv1.ImageDigestMirrorSet) string { return x.Name })
	sortByName(in.ImageTagMirrorSets, func(x *configv1.ImageTagMirrorSet) string { return x.Name })
	sortByName(in.KubeletConfigs, func(x *mcfgv1.KubeletConfig) string { return x.Name })
	sortByName(in.ContainerRuntimeConfigs, func(x *mcfgv1.ContainerRuntimeConfig) string { return x.Name })
	if o.machineConfigIndexer != nil {
		for _, obj := range o.machineConfigIndexer.List() {
			if mc, ok := obj.(*amcov1alpha1.AIXMachineConfig); ok {
				in.MachineConfigs = append(in.MachineConfigs, mc)
			}
		}
		sortByName(in.MachineConfigs, func(x *amcov1alpha1.AIXMachineConfig) string { return x.Name })
	}
	return render.Render(in)
}

func optional[T any](obj *T, err error) (*T, error) {
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return obj, err
}

// sortByName makes lister output (map order) deterministic.
func sortByName[T any](s []T, name func(T) string) {
	sort.Slice(s, func(i, j int) bool { return name(s[i]) < name(s[j]) })
}

// ensureRendered stores cfg in its content-addressed ConfigMap and returns
// the ConfigMap's name. The ConfigMap is immutable and owned by the pool.
func (o *Operator) ensureRendered(ctx context.Context, pool *amcov1alpha1.AIXConfigPool, cfg *rendered.Config) (string, error) {
	name, err := cfg.Name()
	if err != nil {
		return "", err
	}
	if _, err := o.cmLister.Get(name); err == nil {
		return name, nil
	}
	data, err := cfg.Marshal()
	if err != nil {
		return "", err
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: o.cfg.Namespace,
			Labels:    map[string]string{rendered.PoolLabel: pool.Name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: amcov1alpha1.SchemeGroupVersion.String(),
				Kind:       "AIXConfigPool",
				Name:       pool.Name,
				UID:        pool.UID,
			}},
		},
		Immutable: ptr.To(true),
		Data:      map[string]string{rendered.ConfigMapKey: string(data)},
	}
	if _, err := o.kube.CoreV1().ConfigMaps(o.cfg.Namespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("creating rendered config %s: %w", name, err)
	}
	klog.Infof("pool %s: rendered config %s", pool.Name, name)
	return name, nil
}

// gcRendered deletes the pool's rendered ConfigMaps that no node references,
// apart from the target and the KeepRendered most recent ones.
func (o *Operator) gcRendered(ctx context.Context, pool *amcov1alpha1.AIXConfigPool, target string) error {
	cms, err := o.cmLister.List(labels.SelectorFromSet(labels.Set{rendered.PoolLabel: pool.Name}))
	if err != nil {
		return err
	}
	inUse := map[string]bool{target: true, pool.Status.RenderedConfig: true}
	nodes, err := o.nodeLister.List(labels.Everything())
	if err != nil {
		return err
	}
	for _, n := range nodes {
		inUse[nodestate.Desired(n)] = true
		inUse[nodestate.Current(n)] = true
	}
	var unused []*corev1.ConfigMap
	for _, cm := range cms {
		if !inUse[cm.Name] {
			unused = append(unused, cm)
		}
	}
	sort.Slice(unused, func(i, j int) bool {
		return unused[j].CreationTimestamp.Before(&unused[i].CreationTimestamp)
	})
	for i, cm := range unused {
		if i < o.cfg.KeepRendered {
			continue
		}
		klog.Infof("pool %s: deleting unused rendered config %s", pool.Name, cm.Name)
		if err := o.kube.CoreV1().ConfigMaps(o.cfg.Namespace).Delete(ctx, cm.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
