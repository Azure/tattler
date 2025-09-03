/*
Package safety provides a set of safety checks for exposing Kubernetes resources to the outside world.

Usage:

	s := safety.New(ctx, in, out, safety.WithLogger(l))

	// Read entries that have been scrubbed of sensitive information.
	go func() {
		// read entries from out
		for e := range out {
			// do something with e
		}
	}()

	// Send entries to be scrubbed.
	for _, e := range entries {
		in <- e
	}
*/
package safety

import (
	"fmt"

	"github.com/Azure/tattler/data"
	"github.com/gostdlib/base/context"

	corev1 "k8s.io/api/core/v1"
)

// Secrets provide a set of safety checks for exposing Kubernetes resources to the outside world.
// It currently scrubs sensitive information from pods with containers that have
// environment variables with names that match a secret regular expression.
type Secrets struct {
	in  <-chan data.Entry
	out chan data.Entry
}

// Option is a functional option for the Secrets.
type Option func(*Secrets) error

// New creates a new Secrets. The pipeline is ready once New() is called successfully.
// Closing in will close out.
func New(ctx context.Context, in <-chan data.Entry, out chan data.Entry, options ...Option) (*Secrets, error) {
	if in == nil || out == nil {
		panic("can't call Secrets.New() with a nil in or out channel")
	}

	s := &Secrets{
		in:  in,
		out: out,
	}

	for _, o := range options {
		if err := o(s); err != nil {
			return nil, err
		}
	}

	go s.run(ctx)
	return s, nil
}

// run starts the Secrets processing.
func (s *Secrets) run(ctx context.Context) {
	defer close(s.out)

	for e := range s.in {
		s.scrubber(ctx, e)
	}
}

// scrubber scrubs sensitive information from an informer.
func (s *Secrets) scrubber(ctx context.Context, e data.Entry) {
	switch e.ObjectType() {
	case data.OTPod:
		p, err := e.Pod()
		if err != nil {
			context.Log(ctx).Error(fmt.Sprintf("safety.Secrets.informerRouter: error casting object type OTPod to pod: %v", err))
			return
		}
		s.scrubPod(p)
	}
	s.out <- e
}

// scrubPod scrubs sensitive information from a pod.
func (s *Secrets) scrubPod(p *corev1.Pod) {
	spec := p.Spec
	for i, cont := range spec.Containers {
		spec.Containers[i] = s.scrubContainer(cont)
	}
}

// This is the regular expression that is used to match environment variables that are sensitive.
// And the code that would be used to scrub the sensitive information.
// Commented out for now because we are going to scrub all values.
/*
var secretRE = regexp.MustCompile(`(?i)(token|pass|pwd|jwt|hash|secret|bearer|cred|secure|signing|cert|code|key)`)

if secretRE.MatchString(ev.Name) {
		ev.Value = redacted
		c.Env[i] = ev
}
*/
var redacted = "REDACTED"

// scrubContainer scrubs sensitive information from a container.
func (s *Secrets) scrubContainer(c corev1.Container) corev1.Container {
	for i, ev := range c.Env {
		ev.Value = redacted
		c.Env[i] = ev
	}
	return c
}
