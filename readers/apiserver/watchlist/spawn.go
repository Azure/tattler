package watchlist

import (
	"github.com/gostdlib/base/context"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

func (r *Reader) createNamespaceWatcher(ctx context.Context) []resourceWatcher {
	return []resourceWatcher{
		{key: corev1.SchemeGroupVersion.WithResource("namespaces"), spawn: func(options metav1.ListOptions) (watch.Interface, error) {
			wi, err := r.clientset.CoreV1().Namespaces().Watch(ctx, options)
			if err != nil {
				return nil, err
			}
			return wi, nil
		}},
	}
}

func (r *Reader) createNodesWatcher(ctx context.Context) []resourceWatcher {
	return []resourceWatcher{
		{key: corev1.SchemeGroupVersion.WithResource("nodes"), spawn: func(options metav1.ListOptions) (watch.Interface, error) {
			wi, err := r.clientset.CoreV1().Nodes().Watch(ctx, options)
			if err != nil {
				return nil, err
			}
			return wi, nil
		}},
	}
}

func (r *Reader) createPodsWatcher(ctx context.Context) []resourceWatcher {
	return []resourceWatcher{
		{key: corev1.SchemeGroupVersion.WithResource("pods"), spawn: func(options metav1.ListOptions) (watch.Interface, error) {

			wi, err := r.clientset.CoreV1().Pods(metav1.NamespaceAll).Watch(ctx, options)
			if err != nil {
				return nil, err
			}
			return wi, nil
		}},
	}
}

func (r *Reader) createPersistentVolumesWatcher(ctx context.Context) []resourceWatcher {
	return []resourceWatcher{
		{key: corev1.SchemeGroupVersion.WithResource("persistentvolumes"), spawn: func(options metav1.ListOptions) (watch.Interface, error) {
			wi, err := r.clientset.CoreV1().PersistentVolumes().Watch(ctx, options)
			if err != nil {
				return nil, err
			}
			return wi, nil
		}},
	}
}

func (r *Reader) createRBACWatcher(ctx context.Context) []resourceWatcher {
	return []resourceWatcher{
		{key: rbacv1.SchemeGroupVersion.WithResource("roles"), spawn: func(options metav1.ListOptions) (watch.Interface, error) {
			wi, err := r.clientset.RbacV1().Roles(metav1.NamespaceAll).Watch(ctx, options)
			if err != nil {
				return nil, err
			}
			return wi, nil
		}},
		{key: rbacv1.SchemeGroupVersion.WithResource("rolebindings"), spawn: func(options metav1.ListOptions) (watch.Interface, error) {
			wi, err := r.clientset.RbacV1().RoleBindings(metav1.NamespaceAll).Watch(ctx, options)
			if err != nil {
				return nil, err
			}
			return wi, nil
		}},
		{key: rbacv1.SchemeGroupVersion.WithResource("clusterroles"), spawn: func(options metav1.ListOptions) (watch.Interface, error) {
			wi, err := r.clientset.RbacV1().ClusterRoles().Watch(ctx, options)
			if err != nil {
				return nil, err
			}
			return wi, nil
		}},
		{key: rbacv1.SchemeGroupVersion.WithResource("clusterrolebindings"), spawn: func(options metav1.ListOptions) (watch.Interface, error) {
			wi, err := r.clientset.RbacV1().ClusterRoleBindings().Watch(ctx, options)
			if err != nil {
				return nil, err
			}
			return wi, nil
		}},
	}
}

func (r *Reader) createServicesWatcher(ctx context.Context) []resourceWatcher {
	return []resourceWatcher{
		{key: corev1.SchemeGroupVersion.WithResource("services"), spawn: func(options metav1.ListOptions) (watch.Interface, error) {
			wi, err := r.clientset.CoreV1().Services(metav1.NamespaceAll).Watch(ctx, options)
			if err != nil {
				return nil, err
			}
			return wi, nil
		}},
	}
}

func (r *Reader) createDeploymentsWatcher(ctx context.Context) []resourceWatcher {
	return []resourceWatcher{
		{key: appsv1.SchemeGroupVersion.WithResource("deployments"), spawn: func(options metav1.ListOptions) (watch.Interface, error) {
			wi, err := r.clientset.AppsV1().Deployments(metav1.NamespaceAll).Watch(ctx, options)
			if err != nil {
				return nil, err
			}
			return wi, nil
		}},
	}
}

func (r *Reader) createIngressesWatcher(ctx context.Context) []resourceWatcher {
	return []resourceWatcher{
		{key: networkingv1.SchemeGroupVersion.WithResource("ingresses"), spawn: func(options metav1.ListOptions) (watch.Interface, error) {
			wi, err := r.clientset.NetworkingV1().Ingresses(metav1.NamespaceAll).Watch(ctx, options)
			if err != nil {
				return nil, err
			}
			return wi, nil
		}},
	}
}

func (r *Reader) createEndpointsWatcher(ctx context.Context) []resourceWatcher {
	return []resourceWatcher{
		{key: corev1.SchemeGroupVersion.WithResource("endpoints"), spawn: func(options metav1.ListOptions) (watch.Interface, error) {
			wi, err := r.clientset.CoreV1().Endpoints(metav1.NamespaceAll).Watch(ctx, options)
			if err != nil {
				return nil, err
			}
			return wi, nil
		}},
	}
}
