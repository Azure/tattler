// Package relist provides a way to list Kubernetes resources using the client-go library and stream
// the results back to a channel as data.Entry objects. This is useful for relisting resources
// and marking them as snapshot objects instead of trying to do new/old comparisions via watches.
package relist

import (
	"fmt"

	"github.com/Azure/tattler/data"
	"github.com/Azure/tattler/readers/apiserver/watchlist/internal/types"
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

// listPage if a function that takes a context and list options and returns a list of data.Entry objects.
// You access these via the MakePagers map.
type listPage func(ctx context.Context, opts metav1.ListOptions) (list []data.Entry, continueToken string, err error)

// Relister will retrieve data.Entry objects from the Kubernetes API server using the client-go library
// given a resource type. It will handle pagination and context cancellation.
type Relister struct {
	pagers map[types.Retrieve]listPage
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
// to get a channel of data.Entry objects.
func makePagers(clientset ClientsetInterface) map[types.Retrieve]listPage {
	return map[types.Retrieve]listPage{
		types.RTNode:              adapter(clientset.CoreV1().Nodes().List),
		types.RTPod:               adapter(clientset.CoreV1().Pods("").List),
		types.RTNamespace:         adapter(clientset.CoreV1().Namespaces().List),
		types.RTPersistentVolume:  adapter(clientset.CoreV1().PersistentVolumes().List),
		types.RTRBAC:              adapter(clientset.RbacV1().Roles(metav1.NamespaceAll).List),
		types.RTService:           adapter(clientset.CoreV1().Services("").List),
		types.RTDeployment:        adapter(clientset.AppsV1().Deployments("").List),
		types.RTIngressController: adapter(clientset.NetworkingV1().Ingresses("").List),
		types.RTEndpoint:          adapter(clientset.CoreV1().Endpoints("").List),
	}
}

// List will return a channel of data.Entry objects for the given resource type. It will handle pagination
// and context cancellation. If the context is cancelled, the channel will be closed with an error on
// the last entry. You should read all entries from the channel until it is closed.
func (r *Relister) List(ctx context.Context, rt types.Retrieve) (chan promises.Response[data.Entry], error) {
	lp, ok := r.pagers[rt]
	if !ok {
		return nil, fmt.Errorf("no pager found for resource type %s: %w", rt, exponential.ErrPermanent)
	}
	return r.list(ctx, lp), nil
}

// list takes a ListPage function and returns a channel of data.Entry objects. It will
// handle pagination and context cancellation. If the context is cancelled, the channel will be closed
// and any error will be sent to the channel. If there are no more pages, the channel will be closed.
func (r *Relister) list(ctx context.Context, lister listPage) chan promises.Response[data.Entry] {
	ch := make(chan promises.Response[data.Entry], 1)
	var (
		list          []data.Entry
		continueToken string
		err           error
	)

	context.Pool(ctx).Submit(
		ctx,
		func() {
			defer close(ch)
			for {
				list, continueToken, err = lister(
					ctx,
					metav1.ListOptions{
						Limit:    100,
						Continue: continueToken,
					},
				)
				if err != nil {
					ch <- promises.Response[data.Entry]{Err: err}
					return
				}

				for _, entry := range list {
					select {
					case <-ctx.Done():
						ch <- promises.Response[data.Entry]{Err: context.Cause(ctx)}
						return
					case ch <- promises.Response[data.Entry]{V: entry}:
					}
				}

				if continueToken == "" {
					break
				}
			}
		},
	)

	return ch
}
