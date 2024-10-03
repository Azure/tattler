// Package data provides data types for readers. All data types for readers are
// packages inside an Entry. This allows for a single channel to be used for all
// data types.
package data

import (
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

var (
	// ErrInvalidType is returned when the type is invalid.
	ErrInvalidType = errors.New("invalid type")
)

//go:generate stringer -type=SourceType -linecomment

type SourceType uint8

const (
	//
	STUnknown   SourceType = 0 // Unknown
	STInformer  SourceType = 1 // Informer
	STWatchList SourceType = 2 // WatchList
	STEtcd      SourceType = 3 // Etcd
)

//go:generate stringer -type=ObjectType -linecomment

// ObjectType is the type of the object held in a type.
type ObjectType uint8

const (
	// OTUnknown indicates a bug in the code.
	OTUnknown ObjectType = 0 // Unknown
	// OTNode indicates the data is a node.
	OTNode ObjectType = 1 // Node
	// OTPod indicates the data is a pod.
	OTPod ObjectType = 2 // Pod
	// OTNamespace indicates the data is a namespace.
	OTNamespace ObjectType = 3 // Namespace
	// OTPersistentVolume indicates the data is a persistent volume.
	OTPersistentVolume ObjectType = 4 // PersistentVolume
	// OTClusterRole indicates the data is a cluster role.
	OTClusterRole ObjectType = 5 // ClusterRole
	// OTClusterRoleBinding indicates the data is a cluster role binding.
	OTClusterRoleBinding ObjectType = 6 // ClusterRoleBinding
	// OTRole indicates the data is a role.
	OTRole ObjectType = 7 // Role
	// OTRoleBinding indicates the data is a role binding.
	OTRoleBinding ObjectType = 8 // RoleBinding
	// OTService indicates the data is a service.
	OTService ObjectType = 9 // Service
	// OTDeployment indicates the data is a deployment.
	OTDeployment ObjectType = 10 // Deployment
	// OTIngressController indicates the data is an ingress controller.
	OTIngressController ObjectType = 11 // IngressController
	// OTEndpoint indicates the data is an endpoint.
	OTEndpoint ObjectType = 12 // Endpoint
)

//go:generate stringer -type=ChangeType -linecomment

// ChangeType is the type of change.
type ChangeType uint8

const (
	// CTUnknown indicates a bug in the code.
	CTUnknown ChangeType = 0 // Unknown
	// CTAdd indicates the data was added.
	CTAdd ChangeType = 1 // Add
	// CTUpdate indicates the data was updated.
	CTUpdate ChangeType = 2 // Update
	// CTDelete indicates the data was deleted.
	CTDelete ChangeType = 3 // Delete
	// CTSnapshot indicates the data is a snapshot. A snapshot is
	// when we relist the same data object in order to make sure our
	// data is up to date.
	CTSnapshot ChangeType = 4 // Snapshot
)

// ingestObj is a generic type for objects that can be ingested.
type ingestObj interface {
	*corev1.Node | *corev1.Pod | *corev1.Namespace | *corev1.PersistentVolume |
		*rbacv1.ClusterRole | *rbacv1.ClusterRoleBinding | *rbacv1.Role | *rbacv1.RoleBinding |
		*corev1.Service | *appsv1.Deployment | *networkingv1.Ingress | *corev1.Endpoints

	runtime.Object

	GetUID() types.UID
}

// Entry is a data entry.
// This is field aligned for better performance.
type Entry struct {
	// data holds the data.
	data runtime.Object
	// uid is the UID of the object.
	uid types.UID

	// SourceType is the type of the entry.
	sourceType SourceType
	// ChangeType is the type of change.
	changeType ChangeType
	// ObjectType is the type of the object.
	objectType ObjectType
}

// NewEntry creates a new Entry. The underlying object must be a corev1.Node, corev1.Pod,
// corev1.PersistentVolume or corev1.Namespace.
func NewEntry(obj runtime.Object, st SourceType, ct ChangeType) (Entry, error) {
	switch v := any(obj).(type) {
	case *corev1.Node:
		return newEntry(v, st, ct)
	case *corev1.Pod:
		return newEntry(v, st, ct)
	case *corev1.Namespace:
		return newEntry(v, st, ct)
	case *corev1.PersistentVolume:
		return newEntry(v, st, ct)
	case *rbacv1.ClusterRole:
		return newEntry(v, st, ct)
	case *rbacv1.ClusterRoleBinding:
		return newEntry(v, st, ct)
	case *rbacv1.Role:
		return newEntry(v, st, ct)
	case *rbacv1.RoleBinding:
		return newEntry(v, st, ct)
	case *corev1.Service:
		return newEntry(v, st, ct)
	case *appsv1.Deployment:
		return newEntry(v, st, ct)
	case *networkingv1.Ingress:
		return newEntry(v, st, ct)
	case *corev1.Endpoints:
		return newEntry(v, st, ct)
	}
	return Entry{}, ErrInvalidType
}

// newEntry creates a new Entry.
func newEntry[O ingestObj](obj O, st SourceType, ct ChangeType) (Entry, error) {
	if obj == nil {
		return Entry{}, fmt.Errorf("new object is nil")
	}

	var ot ObjectType
	switch v := any(obj).(type) {
	case *corev1.Node:
		ot = OTNode
	case *corev1.Pod:
		ot = OTPod
	case *corev1.Namespace:
		ot = OTNamespace
	case *corev1.PersistentVolume:
		ot = OTPersistentVolume
	case *rbacv1.ClusterRole:
		ot = OTClusterRole
	case *rbacv1.ClusterRoleBinding:
		ot = OTClusterRoleBinding
	case *rbacv1.Role:
		ot = OTRole
	case *rbacv1.RoleBinding:
		ot = OTRoleBinding
	case *corev1.Service:
		ot = OTService
	case *appsv1.Deployment:
		ot = OTDeployment
	case *networkingv1.Ingress:
		ot = OTIngressController
	case *corev1.Endpoints:
		ot = OTEndpoint
	default:
		return Entry{}, fmt.Errorf("unknown object type(%T)", v)
	}

	return Entry{
		data:       obj,
		uid:        obj.GetUID(),
		sourceType: st,
		changeType: ct,
		objectType: ot,
	}, nil
}

// MustNewEntry creates a new Entry. It panics if an error occurs.
func MustNewEntry[T runtime.Object](obj T, st SourceType, ct ChangeType) Entry {
	e, err := NewEntry(obj, st, ct)
	if err != nil {
		panic(err)
	}
	return e
}

// UID returns the UID of the underlying object.
func (e Entry) UID() types.UID {
	return e.uid
}

// ObjectType returns the type of the underlying object to allow
// for calling the correct decoder method such as Node, Pod, Namespace, etc.
func (e Entry) ObjectType() ObjectType {
	return e.objectType
}

// ChangeType returns the type of change that occurred, add, update or delete.
func (e Entry) ChangeType() ChangeType {
	return e.changeType
}

// SourceType returns the data source of the entry.
func (e Entry) SourceType() SourceType {
	return e.sourceType
}

// Object returns the data as a runtime.Object.
func (e Entry) Object() runtime.Object {
	return e.data
}

// Node returns the data for a Node type change. An error is returned if the type is not Node.
func (e Entry) Node() (*corev1.Node, error) {
	return assert[*corev1.Node](e.data)
}

// Pod returns the data a pod type change. An error is returned if the type is not Pod.
func (e Entry) Pod() (*corev1.Pod, error) {
	return assert[*corev1.Pod](e.data)
}

// Namespace returns the data as a namespace type change. An error is returned if the type is not Namespace.
func (e Entry) Namespace() (*corev1.Namespace, error) {
	return assert[*corev1.Namespace](e.data)
}

// PersistentVolume returns the data as a persistent volume type change. An error is returned if the type is not PersistentVolume.
func (e Entry) PersistentVolume() (*corev1.PersistentVolume, error) {
	return assert[*corev1.PersistentVolume](e.data)
}

// ClusterRole returns the data as a cluster role type change. An error is returned if the type is not ClusterRole.
func (e Entry) ClusterRole() (*rbacv1.ClusterRole, error) {
	return assert[*rbacv1.ClusterRole](e.data)
}

// ClusterRoleBinding returns the data as a cluster role binding type change. An error is returned if the type is not ClusterRoleBinding.
func (e Entry) ClusterRoleBinding() (*rbacv1.ClusterRoleBinding, error) {
	return assert[*rbacv1.ClusterRoleBinding](e.data)
}

// Role returns the data as a role type change. An error is returned if the type is not Role.
func (e Entry) Role() (*rbacv1.Role, error) {
	return assert[*rbacv1.Role](e.data)
}

// RoleBinding returns the data as a role binding type change. An error is returned if the type is not RoleBinding.
func (e Entry) RoleBinding() (*rbacv1.RoleBinding, error) {
	return assert[*rbacv1.RoleBinding](e.data)
}

// Service returns the data as a service type change. An error is returned if the type is not Service.
func (e Entry) Service() (*corev1.Service, error) {
	return assert[*corev1.Service](e.data)
}

// Deployment returns the data as a deployment type change. An error is returned if the type is not Deployment.
func (e Entry) Deployment() (*appsv1.Deployment, error) {
	return assert[*appsv1.Deployment](e.data)
}

// IngressController returns the data as an ingress controller type change. An error is returned if the type is not IngressController.
func (e Entry) IngressController() (*networkingv1.Ingress, error) {
	return assert[*networkingv1.Ingress](e.data)
}

// Endpoint returns the data as an endpoint type change. An error is returned if the type is not Endpoint.
func (e Entry) Endpoint() (*corev1.Endpoints, error) {
	return assert[*corev1.Endpoints](e.data)
}

// assert asserts the type of the object to the type specfied by the AssertTo generic type.
func assert[AssertTo runtime.Object](obj runtime.Object) (AssertTo, error) {
	var empty AssertTo

	if obj == nil {
		return empty, fmt.Errorf("object is nil")
	}

	v, ok := obj.(AssertTo)
	if !ok {
		return empty, fmt.Errorf("object is not of type %T", empty)
	}

	return v, nil
}
