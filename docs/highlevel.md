# Tattler

## Introduction

Tattler is a set of packages that allow delivering information from the APIServer  or Etcd to any number of processors that can deliver data as required. This pipeline should act as the central pipeline for this information out of an API Server instead of having multiple watcher overloading your API Server instance.

Currently this provides a few information sources:

- Nodes
- Pods
- Namespaces
- Persistent volumes

The informer sources on a connection provide a complete list of all objects that currently exist and push updates as they come in. A full dump is re-sent at certain intervals (customizable) to make sure the state of the world matches.

This package also provides a safety mechanism that attempts to scrub any passwords or tokens from environmental variables that might be stored in pod container specs.

There are preprocessors if you wish to modify data before it reaches various listeners, such as to remove unneeded fields that otherwise would take space on the wire.

## Details

The packages are meant to allow reader objects to send a stream of objects into a pipeline that moves objects into differnt processors.

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

If at some point we need to have mulitple pipelines, we would need to handle re-ordering at the end and adding locking or lock-free datastructures in place of maps we have today.  I do not feel this will be necessary.

### Adding an APIServer reader

Adding an APIServer reader is as simple as implementing the tattler.Reader() for some APIServer call that outputs k8 Objects. You can then register it with the `tattler` package. You will need to modify the `data/` package in order to have support for your data and source.

### Adding a data processor

Adding a data processor is as simple as writing one that can register an input channel with the `routing.Register()` method.

Note that if your data processor is slower that what it receives and has no buffer, data will be dropped. Scale your buffers appropriately for large clusters that on start might send things like 200K objects.

Do not hold onto data passed in, simply use it and let it expire to prevent memory leaks of large data.
