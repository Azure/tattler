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
- Tattler removes all Pod container environment variables that might contain secrets^*

^* It is possible for secrets to show up in other places, but this is the most common place that leads to security issues.

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
			for _, f := range p.ManagedFields {
				f.FieldsV1 = nil
			}
		case data.OTNode:
			n, err := e.Node()
			if err != nil {
				return err
			}
			for _, f := range n.ManagedFields {
				f.FieldsV1 = nil
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
func stdoutProcessor() chan batching.Batches {
	stdOutCh := make(chan batching.Batches, 1)

	go func() {
		// This is the shorter form if you don't care about the source.
		for batches := range stdOutCh {
			for entry := range batches.Iter(context.Background() {
				// I'm just going straight to the generic object, but you can get the type of object and choose
				// a more specific method to call.
				fmt.Println(entry.Object())
			}
		}
	}()

	// Below is a commented out long form if you want to filter data by source.
	/*
	// Keys here are the source type, if you want to filter by source.
	for batches := range stdOutCh {
		// Keys here are the source type, if you want to filter by source.
		for source, batch := range batches {
			// The source is the type of object we're looking at.
			fmt.Println("Source:", source)
			// The keys are the UID of the object.
			for _, entry := range batch {
				fmt.Println("\t", entry.Object())
			}
		}
	}
	*/
	return stdOutCh
}
```

### A Watchlist Reader

```go
// startWatchlist starts the watchlist reader and processor. The clientset is used to connect to the Kubernetes API Server.
// This panics if it can't start the watchlist. This uses exponential retry to start the watchlist.
func startWatchlists(ctx context.Context, t *tattler.Runner, clientset *kubernetes.Clientset) {
	// Start a reader with exponential backoff.
	// "github.com/Azure/retry/exponential"
	boff, _ := exponential.New() // nolint:errcheck // Can't error on default

	// Retry starting the watchlists. On busy clusters with low CPU availability, the watchlists may not start
	// on the first attempt. We'll retry a few times before giving up. This is cheaper than crashing the program
	// and having to restart it manually.
	attempts := 0
	boff.Retry(   // nolint:errcheck // This panics on too many retries.
			ctx,
			func(ctx context.Context, r exponential.Record) error {
				// Setup reader for APIServer WatchList.
				rWatch, err := watchlist.New(
					ctx,
					clientset,
					watchlist.RTNamespace|watchlist.RTNode|watchlist.RTPod,
					watchlist.WithFilterSize(filterSize),
					watchlist.WithRelist(*relistTime),
				)
				if err != nil {
					if attempts < 10 {
						logger.Error("problem starting watchlists: %s", err, "attempts", attempts+1)
						attempts++
					} else {
						fatalErr(logger, "final attempt at starting watchlists: %s", err)
					}
				}
				return err
			},
	)

	// Add it to the Tattler.Runner
	if err := t.AddReader(ctx, rWatch); err != nil {
		fatalErr(logger, "problem adding watchlist reader: %s", err)
	}
}
```

### Creating a Runner and tying it all together
```go
const (
	// batchTimespan is the maximum amount of time we'll wait before sending a batch of objects.
	batchTimespan = 5 * time.Second
	// batchSize is the maximum number of objects we'll send in a single batch.
	batchSize     = 1000
	// filterSize is the initial size of our filter for tracking objects we've seen. This automatically
	// grows as needed, but starting with a larger size can reduce the number of resizes required.
	filterSize    = 200000
)

ctx := context.Background()

// runnerInput is the channel that the watchlist reader (and other readers you provide) will send data to.
runnerInput := make(chan data.Entry, 1)
// processorInput is the channel that the processor will read from. When add this input channel
// during the AddProcessor() call.
processorInput := stdoutProcessor()

t, err := tattler.New(
	ctx,
	runnerInput,
	batchTimespan,
	tattler.WithBatcherOptions(batching.WithBatchSize(batchSize)),
	tattler.WithPreProcessor(preProcs...), // Will show this in a sec
)

if err != nil {
	fatalErr(logger, "problem creating new tattler instance to run our processors: %s", err)
}

if err := t.AddProcessor(ctx, "stdoutProcessor", processorInput); err != nil {
	fatalErr(logger, "problem creating new tattler instance to run our processors: %s", err)
}

// Start a reader with exponential backoff.
// "github.com/Azure/retry/exponential"
boff, _ := exponential.New() // nolint:errcheck // Can't error on default

// Starts the watchlist.
startWatchlists(bkCtx, t, clientset)

// Starts processing our data.
if err := t.Start(ctx); err != nil {
	panic(err)
}

select{}
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
