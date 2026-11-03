// Package operator is the AMCO control loop. It
//
//   - registers AIX nodes by giving them the aix role label,
//   - renders each AIXConfigPool's configuration into an immutable ConfigMap
//     whenever an input changes (ControllerConfig and its CA bundles, Image,
//     ImageDigestMirrorSet, ImageTagMirrorSet, ContainerRuntimeConfig,
//     KubeletConfig, APIServer),
//   - rolls the configuration out to the pool's nodes within maxUnavailable,
//     cordoning and draining first when the change needs it, uncordoning
//     afterwards, and
//   - reports progress in the pool's status conditions.
//
// Nodes are only touched through annotations (pkg/nodestate); the AIX node
// agent does the work on the host.
package operator

import (
	"context"
	"fmt"
	"sync"
	"time"

	configinformers "github.com/openshift/client-go/config/informers/externalversions"
	cfglisters "github.com/openshift/client-go/config/listers/config/v1"
	mcfginformers "github.com/openshift/client-go/machineconfiguration/informers/externalversions"
	mcfglisters "github.com/openshift/client-go/machineconfiguration/listers/machineconfiguration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	configclient "github.com/openshift/client-go/config/clientset/versioned"
	mcfgclient "github.com/openshift/client-go/machineconfiguration/clientset/versioned"

	amcov1alpha1 "github.com/gilbahat/aix-machine-config-operator/pkg/apis/amco/v1alpha1"
	"github.com/gilbahat/aix-machine-config-operator/pkg/client"
	"github.com/gilbahat/aix-machine-config-operator/pkg/nodestate"
	"github.com/gilbahat/aix-machine-config-operator/pkg/rendered"
)

// Config holds the operator's tunables.
type Config struct {
	// Namespace holds the rendered ConfigMaps.
	Namespace string
	// DrainTimeout bounds one drain attempt.
	DrainTimeout time.Duration
	// DrainRetryInterval is the wait before retrying a failed drain.
	DrainRetryInterval time.Duration
	// KeepRendered is how many unreferenced rendered ConfigMaps to keep per pool.
	KeepRendered int
	// Resync is the informer resync period.
	Resync time.Duration
}

// DefaultConfig returns the defaults used by the binary.
func DefaultConfig() Config {
	return Config{
		Namespace:          "aix-machine-config-operator",
		DrainTimeout:       10 * time.Minute,
		DrainRetryInterval: time.Minute,
		KeepRendered:       3,
		Resync:             10 * time.Minute,
	}
}

// Operator is the AMCO controller.
type Operator struct {
	cfg   Config
	kube  kubernetes.Interface
	pools client.PoolInterface

	nodeLister      corelisters.NodeLister
	cmLister        corelisters.ConfigMapNamespaceLister
	poolIndexer     cache.Indexer
	ccLister        mcfglisters.ControllerConfigLister
	kcLister        mcfglisters.KubeletConfigLister
	crcLister       mcfglisters.ContainerRuntimeConfigLister
	imageLister     cfglisters.ImageLister
	apiServerLister cfglisters.APIServerLister
	idmsLister      cfglisters.ImageDigestMirrorSetLister
	itmsLister      cfglisters.ImageTagMirrorSetLister

	synced []cache.InformerSynced
	starts []func(<-chan struct{})

	poolQueue workqueue.TypedRateLimitingInterface[string]
	nodeQueue workqueue.TypedRateLimitingInterface[string]

	drainer Drainer
	// async runs drains; tests replace it to run them inline.
	async func(func())
	now   func() time.Time

	mu       sync.Mutex
	inflight map[string]string       // node -> rendered config it is being drained for
	failures map[string]drainFailure // node -> last drain failure
}

type drainFailure struct {
	target string
	err    error
	at     time.Time
}

// New wires the operator to the cluster's informers.
func New(cfg Config, kube kubernetes.Interface, configClient configclient.Interface, mcfgClient mcfgclient.Interface, pools client.PoolInterface) *Operator {
	kubeInf := kubeinformers.NewSharedInformerFactory(kube, cfg.Resync)
	nsInf := kubeinformers.NewSharedInformerFactoryWithOptions(kube, cfg.Resync, kubeinformers.WithNamespace(cfg.Namespace))
	cfgInf := configinformers.NewSharedInformerFactory(configClient, cfg.Resync)
	mcfgInf := mcfginformers.NewSharedInformerFactory(mcfgClient, cfg.Resync)
	poolInf := client.NewPoolInformer(pools, cfg.Resync)

	o := newOperator(cfg, kube, pools)
	o.drainer = NewKubectlDrainer(kube, cfg.DrainTimeout)

	nodes := kubeInf.Core().V1().Nodes()
	cms := nsInf.Core().V1().ConfigMaps()
	ccs := mcfgInf.Machineconfiguration().V1().ControllerConfigs()
	kcs := mcfgInf.Machineconfiguration().V1().KubeletConfigs()
	crcs := mcfgInf.Machineconfiguration().V1().ContainerRuntimeConfigs()
	images := cfgInf.Config().V1().Images()
	apiServers := cfgInf.Config().V1().APIServers()
	idms := cfgInf.Config().V1().ImageDigestMirrorSets()
	itms := cfgInf.Config().V1().ImageTagMirrorSets()

	o.nodeLister = nodes.Lister()
	o.cmLister = cms.Lister().ConfigMaps(cfg.Namespace)
	o.poolIndexer = poolInf.GetIndexer()
	o.ccLister = ccs.Lister()
	o.kcLister = kcs.Lister()
	o.crcLister = crcs.Lister()
	o.imageLister = images.Lister()
	o.apiServerLister = apiServers.Lister()
	o.idmsLister = idms.Lister()
	o.itmsLister = itms.Lister()

	allPools := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(interface{}) { o.enqueueAllPools() },
		UpdateFunc: func(interface{}, interface{}) { o.enqueueAllPools() },
		DeleteFunc: func(interface{}) { o.enqueueAllPools() },
	}
	poolChanged := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(interface{}) { o.enqueueAIXNodes() },
		UpdateFunc: func(interface{}, interface{}) { o.enqueueAIXNodes() },
		DeleteFunc: func(interface{}) { o.enqueueAIXNodes() },
	}
	if _, err := poolInf.AddEventHandler(poolChanged); err != nil {
		utilruntime.HandleError(err)
	}
	for _, inf := range []cache.SharedIndexInformer{
		cms.Informer(), ccs.Informer(), kcs.Informer(), crcs.Informer(),
		images.Informer(), apiServers.Informer(), idms.Informer(), itms.Informer(), poolInf,
	} {
		if _, err := inf.AddEventHandler(allPools); err != nil {
			utilruntime.HandleError(err)
		}
		o.synced = append(o.synced, inf.HasSynced)
	}
	nodeHandler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { o.onNode(obj) },
		UpdateFunc: func(_, obj interface{}) { o.onNode(obj) },
		DeleteFunc: func(interface{}) { o.enqueueAllPools() },
	}
	if _, err := nodes.Informer().AddEventHandler(nodeHandler); err != nil {
		utilruntime.HandleError(err)
	}
	o.synced = append(o.synced, nodes.Informer().HasSynced)

	o.starts = []func(<-chan struct{}){kubeInf.Start, nsInf.Start, cfgInf.Start, mcfgInf.Start, func(stop <-chan struct{}) { go poolInf.Run(stop) }}
	return o
}

func newOperator(cfg Config, kube kubernetes.Interface, pools client.PoolInterface) *Operator {
	return &Operator{
		cfg:       cfg,
		kube:      kube,
		pools:     pools,
		poolQueue: workqueue.NewTypedRateLimitingQueueWithConfig(workqueue.DefaultTypedControllerRateLimiter[string](), workqueue.TypedRateLimitingQueueConfig[string]{Name: "aixconfigpools"}),
		nodeQueue: workqueue.NewTypedRateLimitingQueueWithConfig(workqueue.DefaultTypedControllerRateLimiter[string](), workqueue.TypedRateLimitingQueueConfig[string]{Name: "aixnodes"}),
		async:     func(f func()) { go f() },
		now:       time.Now,
		inflight:  map[string]string{},
		failures:  map[string]drainFailure{},
	}
}

// Run starts the informers and workers and blocks until ctx is done.
func (o *Operator) Run(ctx context.Context) error {
	defer utilruntime.HandleCrash()
	defer o.poolQueue.ShutDown()
	defer o.nodeQueue.ShutDown()

	for _, start := range o.starts {
		start(ctx.Done())
	}
	klog.Info("waiting for informer caches to sync")
	if !cache.WaitForCacheSync(ctx.Done(), o.synced...) {
		return fmt.Errorf("informer caches did not sync")
	}
	klog.Info("AIX Machine Config Operator started")
	go wait.UntilWithContext(ctx, func(ctx context.Context) { o.work(ctx, o.nodeQueue, o.syncNode) }, time.Second)
	// One pool worker: rollouts of different pools are cheap and serial
	// processing keeps node bookkeeping simple.
	go wait.UntilWithContext(ctx, func(ctx context.Context) { o.work(ctx, o.poolQueue, o.syncPool) }, time.Second)
	<-ctx.Done()
	return nil
}

func (o *Operator) work(ctx context.Context, q workqueue.TypedRateLimitingInterface[string], sync func(context.Context, string) (time.Duration, error)) {
	for {
		key, quit := q.Get()
		if quit {
			return
		}
		requeue, err := sync(ctx, key)
		switch {
		case err != nil:
			klog.Errorf("syncing %q: %v", key, err)
			q.AddRateLimited(key)
		default:
			q.Forget(key)
			if requeue > 0 {
				q.AddAfter(key, requeue)
			}
		}
		q.Done(key)
	}
}

func (o *Operator) onNode(obj interface{}) {
	node, ok := obj.(*corev1.Node)
	if !ok || !nodestate.IsAIX(node) {
		return
	}
	o.nodeQueue.Add(node.Name)
	o.enqueueAllPools()
}

func (o *Operator) enqueueAllPools() {
	if o.poolIndexer == nil {
		return
	}
	for _, obj := range o.poolIndexer.List() {
		if p, ok := obj.(*amcov1alpha1.AIXConfigPool); ok {
			o.poolQueue.Add(p.Name)
		}
	}
}

func (o *Operator) listPools() []*amcov1alpha1.AIXConfigPool {
	var out []*amcov1alpha1.AIXConfigPool
	for _, obj := range o.poolIndexer.List() {
		if p, ok := obj.(*amcov1alpha1.AIXConfigPool); ok {
			out = append(out, p)
		}
	}
	return out
}

func (o *Operator) getPool(name string) (*amcov1alpha1.AIXConfigPool, bool) {
	obj, ok, err := o.poolIndexer.GetByKey(name)
	if err != nil || !ok {
		return nil, false
	}
	p, ok := obj.(*amcov1alpha1.AIXConfigPool)
	return p, ok
}

// renderedConfig returns the parsed rendered config stored in ConfigMap name,
// or nil if it does not exist (any more).
func (o *Operator) renderedConfig(name string) (*rendered.Config, error) {
	if name == "" {
		return nil, nil
	}
	cm, err := o.cmLister.Get(name)
	if apierrors.IsNotFound(err) {
		// The informer may not have seen a ConfigMap created moments ago.
		cm, err = o.kube.CoreV1().ConfigMaps(o.cfg.Namespace).Get(context.TODO(), name, metav1.GetOptions{})
	}
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return rendered.Unmarshal([]byte(cm.Data[rendered.ConfigMapKey]))
}
