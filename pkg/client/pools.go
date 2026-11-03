// Package client is a small typed client and informer for AIXConfigPool,
// built on client-go's REST client so AMCO needs no generated clientset.
package client

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	amcov1alpha1 "github.com/gilbahat/aix-machine-config-operator/pkg/apis/amco/v1alpha1"
)

// Scheme knows the AMCO API types.
var Scheme = runtime.NewScheme()

func init() {
	if err := amcov1alpha1.AddToScheme(Scheme); err != nil {
		panic(err)
	}
	metav1.AddToGroupVersion(Scheme, metav1.SchemeGroupVersion)
}

// PoolInterface reads and writes AIXConfigPools.
type PoolInterface interface {
	List(ctx context.Context, opts metav1.ListOptions) (*amcov1alpha1.AIXConfigPoolList, error)
	Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error)
	Get(ctx context.Context, name string) (*amcov1alpha1.AIXConfigPool, error)
	Create(ctx context.Context, pool *amcov1alpha1.AIXConfigPool) (*amcov1alpha1.AIXConfigPool, error)
	UpdateStatus(ctx context.Context, pool *amcov1alpha1.AIXConfigPool) (*amcov1alpha1.AIXConfigPool, error)
}

type pools struct {
	rc rest.Interface
}

// NewPools returns a PoolInterface for the cluster described by cfg.
func NewPools(cfg *rest.Config) (PoolInterface, error) {
	c := rest.CopyConfig(cfg)
	c.GroupVersion = &amcov1alpha1.SchemeGroupVersion
	c.APIPath = "/apis"
	c.NegotiatedSerializer = serializer.NewCodecFactory(Scheme).WithoutConversion()
	if c.UserAgent == "" {
		c.UserAgent = rest.DefaultKubernetesUserAgent()
	}
	rc, err := rest.RESTClientFor(c)
	if err != nil {
		return nil, err
	}
	return &pools{rc: rc}, nil
}

func (p *pools) List(ctx context.Context, opts metav1.ListOptions) (*amcov1alpha1.AIXConfigPoolList, error) {
	out := &amcov1alpha1.AIXConfigPoolList{}
	err := p.rc.Get().Resource(amcov1alpha1.Resource).VersionedParams(&opts, metav1.ParameterCodec).Do(ctx).Into(out)
	return out, err
}

func (p *pools) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	opts.Watch = true
	return p.rc.Get().Resource(amcov1alpha1.Resource).VersionedParams(&opts, metav1.ParameterCodec).Watch(ctx)
}

func (p *pools) Get(ctx context.Context, name string) (*amcov1alpha1.AIXConfigPool, error) {
	out := &amcov1alpha1.AIXConfigPool{}
	err := p.rc.Get().Resource(amcov1alpha1.Resource).Name(name).Do(ctx).Into(out)
	return out, err
}

func (p *pools) Create(ctx context.Context, pool *amcov1alpha1.AIXConfigPool) (*amcov1alpha1.AIXConfigPool, error) {
	out := &amcov1alpha1.AIXConfigPool{}
	err := p.rc.Post().Resource(amcov1alpha1.Resource).Body(pool).Do(ctx).Into(out)
	return out, err
}

func (p *pools) UpdateStatus(ctx context.Context, pool *amcov1alpha1.AIXConfigPool) (*amcov1alpha1.AIXConfigPool, error) {
	out := &amcov1alpha1.AIXConfigPool{}
	err := p.rc.Put().Resource(amcov1alpha1.Resource).Name(pool.Name).SubResource("status").Body(pool).Do(ctx).Into(out)
	return out, err
}

// NewPoolInformer returns a shared informer over all AIXConfigPools.
func NewPoolInformer(p PoolInterface, resync time.Duration) cache.SharedIndexInformer {
	lw := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
			return p.List(ctx, opts)
		},
		WatchFuncWithContext: func(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
			return p.Watch(ctx, opts)
		},
	}
	return cache.NewSharedIndexInformer(lw, &amcov1alpha1.AIXConfigPool{}, resync, cache.Indexers{})
}
