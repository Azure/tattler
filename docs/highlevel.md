# Tattler

## Introduction

Tattler is a set of packages that allow delivering information from the a K8 source, such as APIServer or Etcd, to any number of processors that can deliver data to whatever service you required. This pipeline should act as the central pipeline for this information out of K8 instead of having multiple services overloading K8 core services.

Currently we provide information from APIServer with plans to add etcd in the future.

Here is the current list of information we export:

- Deployments
- Endpoints
- Ingress Controllers
- Namespaces
- Nodes
- Persistent volumes
- Pods
- RBAC (Roles, RoleBindings, ClusterRoles, ClusterRoleBindings)
- Services

The sources provide a complete list of all objects that currently exist and push updates as they come in. A full dump is re-sent at certain intervals (customizable) to make sure the state of the world matches.

This package also provides a safety mechanism that attempts to scrub any passwords or tokens from environmental variables that might be stored in pod container specs.

There are preprocessors if you wish to modify data before it reaches various listeners, such as to remove unneeded fields that otherwise would take space on the wire.

## Details

The packages are meant to allow reader objects to send a stream of objects into a pipeline that moves objects into differnt processors that handle exporting of the data.

The only restriction to readers is they must by able to output the object as a `data.Entry`. To be stored in a `data.Entry`, you must implement a custom type inside the `data` package that can implement `SourceData`, which is defined as:

```go
// SourceData is a generic type for objects wrappers in this package.
type SourceData interface {
	// GetUID returns the UID of the object.
	GetUID() types.UID
	// Object returns the object as a runtime.Object. This is always for the latest change,
	// in the case that this is an update.
	Object() runtime.Object
}
```
This interface ensures your type is a K8 object and ensures a quick lookup of its UID which is used to remove duplicates during batching operations.

### Pipelining

These packages are setup to make a small pipeline. The pipeline processes objects sent by readers for date safety, batches them up and routes them to data processors.

The data flow is as follows:

```
reader(s) -> safety.Secrets -> preprocessing -> batching.Batcher -> routing.Batches -> data processors
```

- reader(s) are custom readers for various APIServer API calls that write to the input channel of safety.
- safety.Secrets looks into containers and redacts secrets that may have been passed in env variables
- preprocessing is a set of function that modifies the data before anything sees it
- batching.Batcher batches all input over some time period or certain number of items (whichever is first) and sends it for routing to data processors. The batching time is universal.
- routing.Batches accepts batches of data from the batcher and sends the data to all registered data processors.
- data processors are custom data processors that pick through the batched data and do something with it, like send it to a service, write it to disk, etc...

The pipeline is not parallel, but is concurrent. That is, every stage is in its own goroutine, but the pipeline itself is not. That means that every stage can be working on a separate piece of data.

If at some point we need to have multiple pipelines, we would need to handle re-ordering at the end and adding locking or lock-free datastructures in place of maps we have today.  I do not feel this will be necessary.

### Adding an APIServer reader

Adding an APIServer reader is as simple as implementing the tattler.Reader() for some APIServer call that outputs k8 Objects. You can then register it with the `tattler` package. You will need to modify the `data/` package in order to have support for your data and source.

### Adding a data processor

Adding a data processor is as simple as writing one that can register an input channel with the `routing.Register()` method.

Note that if your data processor is slower that what it receives and has no buffer, data will be dropped. Scale your buffers appropriately for large clusters that on start might send 200K objects.

Do not hold onto data passed in, simply use it and let it expire to prevent memory leaks of large data.

### Adding a new data type

Adding a new data type requires making a few modifications. This usually takes about 10 minutes once you know what K8 object you want.

If we want to add a new data type, say `Deployments` to the `watchlist` reader, we would need the following steps:

#### Add the new type to `readers/apiserver/watchlist/internal/watchlist.go`

Add a new `RetrieveType`. This is a bitflag that is used to determine what data to retrieve from the APIServer. This is used in the `watchlist` reader to determine what data to retrieve.

```go
RTDeployment RetrieveType = 1 << 6 // Deployment
```

In the method `mapCreators`, add a new case for the new type that provides a new method to create a watcher for the new type (we will create this next).

```go
m := map[RetrieveType]func(ctx context.Context) []spawnWatcher{
			RTDeployment:        r.createDeploymentsWatcher,
			...
```

Now create a new method that will create a watcher for the new type.

```go
func (r *Reader) createDeploymentsWatcher(ctx context.Context) []spawnWatcher {
	return []spawnWatcher{
		func(options metav1.ListOptions) (watch.Interface, error) {
			wi, err := r.clientset.AppsV1().Deployments("").Watch(ctx, options)
			if err != nil {
				panic(err.Error())
			}
			return wi, nil
		},
	}
}
```

This creates a single `spawnWatcher`, but you can also create multiple if you need to watch multiple objects. The `RBAC` watcher creates 4 watchers, for example, in order to get all the data required.

Which `clientset` call you use depends on the type of object you are watching. In this case, we are watching `Deployments`, so we use `AppsV1().Deployments("")`. My suggestion is if you don't know where to find your object, ask ChatGPT about it. K8 docs are pretty terrible for finding this information and ChatGPT is pretty good at finding it.

Now you must update the `readers/watchlist/watchlist.go` file to have the associated `RetrieveType` flag.

```go
RTDeployment = reader.RTDeployment
```
This is simply an alias to the one created above.

Finally, you must run `go generate ./...` to update the `retrievetype_sting.go` file.

#### Expand the data object for these types

You must edit `data/data.go` to add the following:

Add an `ObjectType` for the new type.

```go
// OTDeployment indicates the data is a deployment.
	OTDeployment ObjectType = 10 // Deployment
```

Modify `ingestObj` to include the new type.

```go
type ingestObj interface {
	corev1.Node | *corev1.Pod | ... |
		*appsv1.Deployment
	...
}
```

Modify `NewEntry` to include the new type.

```go
func NewEntry(obj runtime.Object, st SourceType, ct ChangeType) (Entry, error) {
	switch v := any(obj).(type) {
	...
	case *appsv1.Deployment:
		return newEntry(v, st, ct)
	...
	}
...
}
```

Modify `newEntry` to include the new type.

```go
func newEntry[O ingestObj](obj O, st SourceType, ct ChangeType) (Entry, error) {
	...
	switch v := any(obj).(type) {
	...
	case *appsv1.Deployment:
		ot = OTDeployment
	...
	}
...
}
```

Add an object assert:

```go
// Deployment returns the data as a deployment type change. An error is returned if the type is not Deployment.
func (e Entry) Deployment() (*appsv1.Deployment, error) {
	return assert[*appsv1.Deployment](e.data)
}
```

Run `go generate ./...` to update the `objecttype_string.go` file.

#### Add a test

Modify `data/data_test.go` to include a test for the new type in `TestNewEntry`.
