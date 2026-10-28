# Tattler - A K8 object state reader

<p align="center">
  <img src="./docs/img/tattler.webp"  width="500">
</p>

Tattler is a package that reads the state changes of Kubernetes objects and reports them to user-defined endpoints.

Currently, Tattler supports the following object types:
- Pod
- Node
- Namepace
- Services
- Endpoints
- Deployments
- Persistent Volumes
- RBAC Role, Cluster Role, Binding and Cluster Binding

We currently pull from the following sources:
- Kubernetes API Server (via Watchlists)

In the future, we plan to support more object types and sources.

Tattler provides a few key features over simply calling the API yourself:
- Tattler can be used to filter fields(or change/add to them), filter object types, and filter sources
- Tattler can be configured to read the information from multiple sources
- Tattler redacts the value of every container environment variable, which might contain secrets^*

^* Values are replaced with `REDACTED` for containers, init containers and ephemeral containers, in both Pods and
Deployment pod templates. It is possible for secrets to show up in other places, but this is the most common place
that leads to security issues.

## Getting Started

Tattler has a few key components:
- `Reader` - This is the object that reads the state of the objects from a source, like an API Server watchlist.
- `PreProcessor` - This is the object that transforms the data before Tattler sends it to a processor.
- `Runner` - This is the object that manages pre-processors, readers, and processors. It takes input from a Reader and sends them to Processors.
- `Processor` - This is the object that receives the data. It may send the data to an endpoint, write to a database or just print to the screen.

A Tattler `Runnner` can read from multiple `Reader` objects and send the data to multiple `Processor` objects. This
allows you to read from multiple sources and send the data to multiple endpoints. Processor's can filter for
specific object types and specific sources for their needs.

Tattler is fast and will get only faster in Go 1.23. Its main limitations are the amount of memory it can use
and the source it reads from.  Etcd is notoriously overused and is a common bottleneck. This directly affects
API Server. In addition, object information for things like pods is conveyed in a very inefficient way, which adds
to the load on the API Server/Etcd and network traffic.

It is highly recommended that you test Tattler in a staging environment before deploying it to production. Clusters
with high load and low CPU availability may not be able to handle the additional load Tattler places on the API Server.

## Example

### PreProcessors

This will setup a pre-processor that removes the `ManagedFields` from the `Pod` and `Node` objects. These
fields are not needed for most use cases and contain duplicate infomration.

```go
// preProcs contains a list of pre-processors to be applied to the data before sending it to a processor.
// This pre-processor set is used to remove sensitive information from the data. But you can manipulate the data
// in any way you see fit.
var preProcs = []tattler.PreProcessor{
	func(ctx context.Context, e data.Entry) error {
		switch e.ObjectType() {
		case data.OTPod:
			p, err := e.Pod()
			if err != nil {
				return err
			}
			for i := 0; i < len(p.ManagedFields); i++ {
				f := p.ManagedFields[i]
				f.FieldsV1 = nil
				p.ManagedFields[i] = f
			}
		case data.OTNode:
			n, err := e.Node()
			if err != nil {
				return err
			}
			for i := 0; i < len(n.ManagedFields); i++ {
				f := n.ManagedFields[i]
				f.FieldsV1 = nil
				n.ManagedFields[i] = f
			}
		}
		return nil
	},
}
```

### A simple processor to stdout

Most use cases are going to be either to write to disk, send to another service or write to a database.
This example is a simple processor that writes the object to stdout.

```go
func stdoutProcessor() chan data.Entry {
	in := make(chan data.Entry, 1)

	go func() {
		for entry := range in {
			// entry.SourceType() and entry.ObjectType() can be used to filter. This goes straight to the generic
			// object, but you can switch on the object type and call a more specific method such as entry.Pod().
			fmt.Println(entry.ChangeType(), entry.Object())
		}
	}()

	return in
}
```

### A Watchlist Reader

When the Reader starts a resource without a bookmark, every existing object arrives as a `CTAdd` entry before live
changes for that resource. On large clusters, expect a burst of entries at startup.

With `watchlist.WithBookmarkStore`, the Reader saves its position (a resourceVersion bookmark per resource) and a restart
resumes from it instead of resending every object. A bookmark the API server rejects (expired, or newer than the
server after an etcd restore or rollback) is cleared and triggers an ordered recovery stream: existing objects arrive
as `CTSnapshot` entries before subsequent live changes for that resource. Recovery uses WatchList initial events when
available, or a consistent list followed by a watch from the list's resource version. That list can be large, so at
startup it runs asynchronously instead of within `Run`'s startup deadline: `Run` can return before that resource's
watch exists. Snapshots alone do not identify objects deleted before recovery began. Optional periodic relists remain
independent of the live watch stream.

`Close` returns an error if any resource's watcher stopped permanently, for example because a recovery list was
forbidden. That error is also logged when it happens.

#### Permissions

The Reader needs `watch` on every resource type it reads. In addition:
- `WithRelist` needs `list` on those resource types.
- `WithBookmarkStore` with the ConfigMap store needs `get` and `update` on the bookmark ConfigMap, which must already
  exist. It also needs `list` on the resource types: at startup the Reader lists one object to check that each stored
  bookmark is not newer than the server, and on clusters without WatchList it recovers a rejected bookmark by listing.
  Without `list`, the startup check is skipped and logged, but a recovery that has to list fails, and that resource's
  watcher stops; the error is returned by `Close`.

```go
// startWatchlist creates the watchlist reader and adds it to the Runner. watchlist.New contacts the API server, so
// wrap this in your retry policy if the cluster may be busy at startup.
func startWatchlist(ctx context.Context, t *tattler.Runner, clientset kubernetes.Interface) error {
	// Optional: persist bookmarks in an existing ConfigMap so a restart resumes where it left off.
	// "github.com/Azure/tattler/readers/apiserver/watchlist/bookmarks/store/configmap"
	bookmarks, err := configmap.New(clientset, "tattler", "tattler-bookmarks")
	if err != nil {
		return err
	}

	// "github.com/Azure/tattler/readers/apiserver/watchlist/types"
	r, err := watchlist.New(
		ctx,
		clientset,
		types.RTNamespace|types.RTNode|types.RTPod,
		watchlist.WithRelist(24*time.Hour),
		watchlist.WithBookmarkStore(bookmarks),
	)
	if err != nil {
		return err
	}

	// The Runner calls SetOut and Run on the reader.
	return t.AddReader(ctx, r)
}
```

### Creating a Runner and tying it all together
```go
ctx := context.Background()

// runnerInput is the channel that the watchlist reader (and other readers you provide) will send data to.
runnerInput := make(chan data.Entry, 1)

t, err := tattler.New(
	ctx,
	runnerInput,
	tattler.WithPreProcessor(preProcs...),
)
if err != nil {
	panic(err)
}

// stdoutProcessor returns the channel the processor reads from. Processors must be added before Start().
if err := t.AddProcessor(ctx, "stdoutProcessor", stdoutProcessor()); err != nil {
	panic(err)
}

if err := startWatchlist(ctx, t, clientset); err != nil {
	panic(err)
}

// Starts the readers and begins processing our data.
if err := t.Start(ctx); err != nil {
	panic(err)
}

select {}
```

## Contributing

This project welcomes contributions and suggestions.  Most contributions require you to agree to a
Contributor License Agreement (CLA) declaring that you have the right to, and actually do, grant us
the rights to use your contribution. For details, visit https://cla.opensource.microsoft.com.

When you submit a pull request, a CLA bot will automatically determine whether you need to provide
a CLA and decorate the PR appropriately (e.g., status check, comment). Simply follow the instructions
provided by the bot. You will only need to do this once across all repos using our CLA.

This project has adopted the [Microsoft Open Source Code of Conduct](https://opensource.microsoft.com/codeofconduct/).
For more information see the [Code of Conduct FAQ](https://opensource.microsoft.com/codeofconduct/faq/) or
contact [opencode@microsoft.com](mailto:opencode@microsoft.com) with any additional questions or comments.

## Trademarks

This project may contain trademarks or logos for projects, products, or services. Authorized use of Microsoft
trademarks or logos is subject to and must follow
[Microsoft's Trademark & Brand Guidelines](https://www.microsoft.com/en-us/legal/intellectualproperty/trademarks/usage/general).
Use of Microsoft trademarks or logos in modified versions of this project must not cause confusion or imply Microsoft sponsorship.
Any use of third-party trademarks or logos are subject to those third-party's policies.
