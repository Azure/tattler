// Package relist provides a way to list Kubernetes resources using the client-go library and stream
// the results back to a channel as data.Entry objects. This is useful for relisting resources
// and marking them as snapshot objects instead of trying to do new/old comparisions via watches.
package relist

import (
	"fmt"

	"github.com/Azure/tattler/data"
	"github.com/Azure/tattler/readers/apiserver/watchlist/types"
	"github.com/gostdlib/base/context"
	"github.com/gostdlib/base/retry/exponential"
	"github.com/gostdlib/base/values/generics/promises"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	appsv1 "k8s.io/client-go/kubernetes/typed/apps/v1"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	networkingv1 "k8s.io/client-go/kubernetes/typed/networking/v1"
	rbacv1 "k8s.io/client-go/kubernetes/typed/rbac/v1"
)

// ClientsetInterface defines the interface needed for Kubernetes operations
type ClientsetInterface interface {
	CoreV1() corev1.CoreV1Interface
	AppsV1() appsv1.AppsV1Interface
	NetworkingV1() networkingv1.NetworkingV1Interface
	RbacV1() rbacv1.RbacV1Interface
}

// listPage is a function that takes a context and list options and returns a list of data.Entry objects.
// You access these via the MakePagers map.
type listPage func(ctx context.Context, opts metav1.ListOptions) (list []data.Entry, continueToken string, err error)

// Relister will retrieve data.Entry objects from the Kubernetes API server using the client-go library
// given a resource type. It will handle pagination and context cancellation.
type Relister struct {
	pagers map[types.Retrieve][]listPage
}

// New creates a new Relister.
func New(clientset ClientsetInterface) (*Relister, error) {
	if clientset == nil {
		return nil, fmt.Errorf("clientset is nil")
	}
	return &Relister{
		pagers: makePagers(clientset),
	}, nil
}

// makePagers creates a map of ListPage functions for each resource type. You use these via the List function
// to get a channel of data.Entry objects. Some resource types (like RBAC) have multiple listers.
func makePagers(clientset ClientsetInterface) map[types.Retrieve][]listPage {
	return map[types.Retrieve][]listPage{
		types.RTNode:              {adapter(clientset.CoreV1().Nodes().List)},
		types.RTPod:               {adapter(clientset.CoreV1().Pods(metav1.NamespaceAll).List)},
		types.RTNamespace:         {adapter(clientset.CoreV1().Namespaces().List)},
		types.RTPersistentVolume:  {adapter(clientset.CoreV1().PersistentVolumes().List)},
		types.RTRBAC: {
			adapter(clientset.RbacV1().Roles(metav1.NamespaceAll).List),
			adapter(clientset.RbacV1().RoleBindings(metav1.NamespaceAll).List),
			adapter(clientset.RbacV1().ClusterRoles().List),
			adapter(clientset.RbacV1().ClusterRoleBindings().List),
		},
		types.RTService:           {adapter(clientset.CoreV1().Services(metav1.NamespaceAll).List)},
		types.RTDeployment:        {adapter(clientset.AppsV1().Deployments(metav1.NamespaceAll).List)},
		types.RTIngressController: {adapter(clientset.NetworkingV1().Ingresses(metav1.NamespaceAll).List)},
		types.RTEndpoint:          {adapter(clientset.CoreV1().Endpoints(metav1.NamespaceAll).List)},
	}
}

// List will return a channel of data.Entry objects for the given resource type. It will handle pagination
// and context cancellation. If the context is cancelled, the channel will be closed with an error on
// the last entry. You should read all entries from the channel until it is closed.
func (r *Relister) List(ctx context.Context, rt types.Retrieve) (chan promises.Response[data.Entry], error) {
	listers, ok := r.pagers[rt]
	if !ok {
		return nil, fmt.Errorf("no pager found for resource type %s: %w", rt, exponential.ErrPermanent)
	}
	return r.listAll(ctx, listers), nil
}

// listAll takes multiple ListPage functions and returns a single channel of data.Entry objects.
// It spawns a goroutine for each lister and merges the results. The channel is closed when all
// listers have completed.
func (r *Relister) listAll(ctx context.Context, listers []listPage) chan promises.Response[data.Entry] {
	ch := make(chan promises.Response[data.Entry], 1)

	context.Pool(ctx).Submit(
		ctx,
		func() {
			defer close(ch)
			for _, lister := range listers {
				if err := r.listOne(ctx, lister, ch); err != nil {
					ch <- promises.Response[data.Entry]{Err: err}
					return
				}
			}
		},
	)

	return ch
}

// listOne lists all entries from a single lister and sends them to the channel.
// It handles pagination and returns an error if the context is cancelled or listing fails.
func (r *Relister) listOne(ctx context.Context, lister listPage, ch chan promises.Response[data.Entry]) error {
	var continueToken string

	for {
		list, cont, err := retryableList(ctx, lister, metav1.ListOptions{Limit: 100, Continue: continueToken})
		if err != nil {
			return err
		}

		for _, entry := range list {
			select {
			case <-ctx.Done():
				return context.Cause(ctx)
			case ch <- promises.Response[data.Entry]{V: entry}:
			}
		}

		continueToken = cont
		if continueToken == "" {
			break
		}
	}
	return nil
}

var back = exponential.Must(exponential.New())

// retryableList wraps a listPage with exponential backoff using existing package boff.
func retryableList(ctx context.Context, lister listPage, options metav1.ListOptions) ([]data.Entry, string, error) {
	var result []data.Entry
	var cont string

	err := back.Retry(
		ctx,
		func(ctx context.Context, rec exponential.Record) error {
			var retryErr error
			result, cont, retryErr = lister(ctx, options)
			return retryErr // Let exponential backoff handle the retry logic
		},
		exponential.WithMaxAttempts(10),
	)

	return result, cont, err
}
