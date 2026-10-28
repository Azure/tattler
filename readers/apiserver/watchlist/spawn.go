package watchlist

import (
	"github.com/gostdlib/base/context"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
)

type resourceClient[L runtime.Object] interface {
	List(context.Context, metav1.ListOptions) (L, error)
	Watch(context.Context, metav1.ListOptions) (watch.Interface, error)
}

func newWatchSpec[L runtime.Object](ctx context.Context, gvr schema.GroupVersionResource, client resourceClient[L]) watchSpec {
	return watchSpec{
		gvr: gvr,
		spawn: func(options metav1.ListOptions) (watch.Interface, error) {
			return client.Watch(ctx, options)
		},
		list: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
			return client.List(ctx, options)
		},
	}
}

func (r *Reader) createNamespaceWatcher(ctx context.Context) []watchSpec {
	return []watchSpec{
		newWatchSpec(ctx, corev1.SchemeGroupVersion.WithResource("namespaces"), r.clientset.CoreV1().Namespaces()),
	}
}

func (r *Reader) createNodesWatcher(ctx context.Context) []watchSpec {
	return []watchSpec{
		newWatchSpec(ctx, corev1.SchemeGroupVersion.WithResource("nodes"), r.clientset.CoreV1().Nodes()),
	}
}

func (r *Reader) createPodsWatcher(ctx context.Context) []watchSpec {
	return []watchSpec{
		newWatchSpec(ctx, corev1.SchemeGroupVersion.WithResource("pods"), r.clientset.CoreV1().Pods(metav1.NamespaceAll)),
	}
}

func (r *Reader) createPersistentVolumesWatcher(ctx context.Context) []watchSpec {
	return []watchSpec{
		newWatchSpec(ctx, corev1.SchemeGroupVersion.WithResource("persistentvolumes"), r.clientset.CoreV1().PersistentVolumes()),
	}
}

func (r *Reader) createRBACWatcher(ctx context.Context) []watchSpec {
	return []watchSpec{
		newWatchSpec(ctx, rbacv1.SchemeGroupVersion.WithResource("roles"), r.clientset.RbacV1().Roles(metav1.NamespaceAll)),
		newWatchSpec(ctx, rbacv1.SchemeGroupVersion.WithResource("rolebindings"), r.clientset.RbacV1().RoleBindings(metav1.NamespaceAll)),
		newWatchSpec(ctx, rbacv1.SchemeGroupVersion.WithResource("clusterroles"), r.clientset.RbacV1().ClusterRoles()),
		newWatchSpec(ctx, rbacv1.SchemeGroupVersion.WithResource("clusterrolebindings"), r.clientset.RbacV1().ClusterRoleBindings()),
	}
}

func (r *Reader) createServicesWatcher(ctx context.Context) []watchSpec {
	return []watchSpec{
		newWatchSpec(ctx, corev1.SchemeGroupVersion.WithResource("services"), r.clientset.CoreV1().Services(metav1.NamespaceAll)),
	}
}

func (r *Reader) createDeploymentsWatcher(ctx context.Context) []watchSpec {
	return []watchSpec{
		newWatchSpec(ctx, appsv1.SchemeGroupVersion.WithResource("deployments"), r.clientset.AppsV1().Deployments(metav1.NamespaceAll)),
	}
}

func (r *Reader) createIngressesWatcher(ctx context.Context) []watchSpec {
	return []watchSpec{
		newWatchSpec(ctx, networkingv1.SchemeGroupVersion.WithResource("ingresses"), r.clientset.NetworkingV1().Ingresses(metav1.NamespaceAll)),
	}
}

func (r *Reader) createEndpointsWatcher(ctx context.Context) []watchSpec {
	return []watchSpec{
		newWatchSpec(ctx, corev1.SchemeGroupVersion.WithResource("endpoints"), r.clientset.CoreV1().Endpoints(metav1.NamespaceAll)),
	}
}
