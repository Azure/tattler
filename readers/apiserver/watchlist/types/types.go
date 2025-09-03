package types

//go:generate stringer -type=Retrieve -linecomment

// Retrieve is the type of data to retrieve. Uses as a bitwise flag.
// So, like: RTNode | RTPod, or RTNode, or RTPod.
type Retrieve uint32

const (
	// RTNode retrieves node data.
	RTNode Retrieve = 1 << 0 // Node
	// RTPod retrieves pod data.
	RTPod Retrieve = 1 << 1 // Pod
	// RTNamespace retrieves namespace data.
	RTNamespace Retrieve = 1 << 2 // Namespace
	// RTPersistentVolume retrieves persistent volume data.
	RTPersistentVolume Retrieve = 1 << 3 // PersistentVolume
	// RTRBAC retrieves role-based access control data.
	RTRBAC Retrieve = 1 << 4 // RBAC
	// RTService retrieves service data.
	RTService Retrieve = 1 << 5 // Services
	// RTDeployment retrieves deployment data.
	RTDeployment Retrieve = 1 << 6 // Deployment
	// RTIngressController retrieves ingress controller data.
	RTIngressController Retrieve = 1 << 7 // IngressController
	// RTEndpoint retrieves endpoint data.
	RTEndpoint Retrieve = 1 << 8 // Endpoint
)
