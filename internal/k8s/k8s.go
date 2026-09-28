// Package k8s builds the Kubernetes client and the label-filtered informers
// the hub uses to observe session objects.
package k8s

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

// Labels and annotations the hub puts on every object it owns.
const (
	LabelSession   = "agents-operator.io/session"
	LabelOwner     = "agents-operator.io/owner"
	LabelUser      = "agents-operator.io/user" // per-user credential Secrets
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelName      = "app.kubernetes.io/name"
	LabelComponent = "app.kubernetes.io/component"
	ManagedBy      = "agents-operator"

	AnnotationGeneration = "agents-operator.io/generation"
)

// NewClientset returns an in-cluster client, falling back to kubeconfig
// (the explicit path, $KUBECONFIG or ~/.kube/config) for development.
func NewClientset(kubeconfig string) (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		if kubeconfig == "" {
			kubeconfig = os.Getenv("KUBECONFIG")
		}
		if kubeconfig == "" {
			if home, herr := os.UserHomeDir(); herr == nil {
				kubeconfig = filepath.Join(home, ".kube", "config")
			}
		}
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("no in-cluster config and kubeconfig failed: %w", err)
		}
	}
	cfg.QPS = 20
	cfg.Burst = 50
	cfg.UserAgent = "agents-operator"
	return kubernetes.NewForConfig(cfg)
}

// Informers holds the shared informers for Pods, PVCs and Secrets carrying
// the session label in one namespace.
type Informers struct {
	factory  informers.SharedInformerFactory
	Pods     listersv1.PodLister
	PVCs     listersv1.PersistentVolumeClaimLister
	Secrets  listersv1.SecretLister
	podInf   cache.SharedIndexInformer
	mu       sync.RWMutex
	handlers []func(sessionID string)
}

// NewInformers creates informers filtered to objects with LabelSession.
func NewInformers(cs kubernetes.Interface, namespace string, resync time.Duration) *Informers {
	factory := informers.NewSharedInformerFactoryWithOptions(cs, resync,
		informers.WithNamespace(namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.LabelSelector = LabelSession
		}),
	)
	inf := &Informers{
		factory: factory,
		Pods:    factory.Core().V1().Pods().Lister(),
		PVCs:    factory.Core().V1().PersistentVolumeClaims().Lister(),
		Secrets: factory.Core().V1().Secrets().Lister(),
		podInf:  factory.Core().V1().Pods().Informer(),
	}
	// Touch the informers so the factory starts them.
	_ = factory.Core().V1().PersistentVolumeClaims().Informer()
	_ = factory.Core().V1().Secrets().Informer()
	_, _ = inf.podInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { inf.notify(obj) },
		UpdateFunc: func(_, obj any) { inf.notify(obj) },
		DeleteFunc: func(obj any) { inf.notify(obj) },
	})
	return inf
}

func (i *Informers) notify(obj any) {
	if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = d.Obj
	}
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	id := pod.Labels[LabelSession]
	if id == "" {
		return
	}
	i.mu.RLock()
	hs := append([]func(string){}, i.handlers...)
	i.mu.RUnlock()
	for _, h := range hs {
		h(id)
	}
}

// OnPodChange registers a callback for every pod add/update/delete.
func (i *Informers) OnPodChange(fn func(sessionID string)) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.handlers = append(i.handlers, fn)
}

// Start runs the informers until ctx is done and waits for the first sync.
func (i *Informers) Start(ctx context.Context) error {
	i.factory.Start(ctx.Done())
	for typ, ok := range i.factory.WaitForCacheSync(ctx.Done()) {
		if !ok {
			return fmt.Errorf("informer for %v failed to sync", typ)
		}
	}
	return nil
}

// Synced reports whether all informers have completed their initial list.
func (i *Informers) Synced() bool {
	for _, ok := range i.factory.WaitForCacheSync(closedCh) {
		if !ok {
			return false
		}
	}
	return true
}

var closedCh = func() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

// PodIP returns the IP of a session's pod when it is running.
func (i *Informers) PodIP(namespace, name string) (string, bool) {
	pod, err := i.Pods.Pods(namespace).Get(name)
	if err != nil || pod.Status.PodIP == "" || pod.Status.Phase != corev1.PodRunning {
		return "", false
	}
	return pod.Status.PodIP, true
}
