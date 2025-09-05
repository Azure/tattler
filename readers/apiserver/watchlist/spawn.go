package watchlist

import (
	"github.com/gostdlib/base/context"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

func (r *Reader) createNamespaceWatcher(ctx context.Context) []spawnWatcher {
	return []spawnWatcher{
		func(options metav1.ListOptions) (watch.Interface, error) {
			wi, err := r.clientset.CoreV1().Namespaces().Watch(ctx, options)
			if err != nil {
				return nil, err
			}
			return wi, nil
		},
	}
}

func (r *Reader) createNodesWatcher(ctx context.Context) []spawnWatcher {
	return []spawnWatcher{
		func(options metav1.ListOptions) (watch.Interface, error) {
			wi, err := r.clientset.CoreV1().Nodes().Watch(ctx, options)
			if err != nil {
				return nil, err
			}
			return wi, nil
		},
	}
}

func (r *Reader) createPodsWatcher(ctx context.Context) []spawnWatcher {
	return []spawnWatcher{
		func(options metav1.ListOptions) (watch.Interface, error) {

			wi, err := r.clientset.CoreV1().Pods(metav1.NamespaceAll).Watch(ctx, options)
			if err != nil {
				return nil, err
			}
			return wi, nil
		},
	}
}

func (r *Reader) createPersistentVolumesWatcher(ctx context.Context) []spawnWatcher {
	return []spawnWatcher{
		func(options metav1.ListOptions) (watch.Interface, error) {
			wi, err := r.clientset.CoreV1().PersistentVolumes().Watch(ctx, options)
			if err != nil {
				return nil, err
			}
			return wi, nil
		},
	}
}

func (r *Reader) createRBACWatcher(ctx context.Context) []spawnWatcher {
	return []spawnWatcher{
		func(options metav1.ListOptions) (watch.Interface, error) {
			wi, err := r.clientset.RbacV1().Roles(metav1.NamespaceAll).Watch(ctx, options)
			if err != nil {
				return nil, err
			}
			return wi, nil
		},
		func(options metav1.ListOptions) (watch.Interface, error) {
			wi, err := r.clientset.RbacV1().RoleBindings(metav1.NamespaceAll).Watch(ctx, options)
			if err != nil {
				return nil, err
			}
			return wi, nil
		},
		func(options metav1.ListOptions) (watch.Interface, error) {
			wi, err := r.clientset.RbacV1().ClusterRoles().Watch(ctx, options)
			if err != nil {
				return nil, err
			}
			return wi, nil
		},
		func(options metav1.ListOptions) (watch.Interface, error) {
			wi, err := r.clientset.RbacV1().ClusterRoleBindings().Watch(ctx, options)
			if err != nil {
				return nil, err
			}
			return wi, nil
		},
	}
}

func (r *Reader) createServicesWatcher(ctx context.Context) []spawnWatcher {
	return []spawnWatcher{
		func(options metav1.ListOptions) (watch.Interface, error) {
			wi, err := r.clientset.CoreV1().Services(metav1.NamespaceAll).Watch(ctx, options)
			if err != nil {
				return nil, err
			}
			return wi, nil
		},
	}
}

func (r *Reader) createDeploymentsWatcher(ctx context.Context) []spawnWatcher {
	return []spawnWatcher{
		func(options metav1.ListOptions) (watch.Interface, error) {
			wi, err := r.clientset.AppsV1().Deployments(metav1.NamespaceAll).Watch(ctx, options)
			if err != nil {
				return nil, err
			}
			return wi, nil
		},
	}
}

func (r *Reader) createIngressesWatcher(ctx context.Context) []spawnWatcher {
	return []spawnWatcher{
		func(options metav1.ListOptions) (watch.Interface, error) {
			wi, err := r.clientset.NetworkingV1().Ingresses(metav1.NamespaceAll).Watch(ctx, options)
			if err != nil {
				return nil, err
			}
			return wi, nil
		},
	}
}

func (r *Reader) createEndpointsWatcher(ctx context.Context) []spawnWatcher {
	return []spawnWatcher{
		func(options metav1.ListOptions) (watch.Interface, error) {
			wi, err := r.clientset.CoreV1().Endpoints(metav1.NamespaceAll).Watch(ctx, options)
			if err != nil {
				return nil, err
			}
			return wi, nil
		},
	}
}
