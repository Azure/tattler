/*
Package safety provides a set of safety checks for exposing Kubernetes resources to the outside world.

Usage:

	s, err := safety.New(ctx, in, out)
	if err != nil {
		// Do something
	}

	// Read entries that have been scrubbed of sensitive information.
	context.Pool(ctx).Submit(
		ctx,
		func() {
			for e := range out {
				// do something with e
			}
		},
	)

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
// It currently redacts the values of all environment variables in the containers, init containers and ephemeral
// containers of pods and deployment pod templates.
type Secrets struct {
	in  <-chan data.Entry
	out chan data.Entry
}

// New creates a new Secrets. The pipeline is ready once New() is called successfully.
// Closing in will close out.
func New(ctx context.Context, in <-chan data.Entry, out chan data.Entry) (*Secrets, error) {
	if in == nil || out == nil {
		panic("can't call Secrets.New() with a nil in or out channel")
	}

	s := &Secrets{
		in:  in,
		out: out,
	}

	// run parks on in for the life of the pipeline, so it goes on the unlimited Default pool rather than taking a
	// slot from a Limited one.
	if !context.Pool(ctx).Default().Submit(ctx, func() { s.run(ctx) }) {
		return nil, fmt.Errorf("safety.New: could not start the scrubber: %w", context.Cause(ctx))
	}
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
	case data.OTDeployment:
		d, err := e.Deployment()
		if err != nil {
			context.Log(ctx).Error(fmt.Sprintf("safety.Secrets.informerRouter: error casting object type OTDeployment to deployment: %v", err))
			return
		}
		d.Spec.Template.Spec = s.scrubPodSpec(d.Spec.Template.Spec)
	}
	s.out <- e
}

// scrubPod scrubs sensitive information from a pod.
func (s *Secrets) scrubPod(p *corev1.Pod) {
	p.Spec = s.scrubPodSpec(p.Spec)
}

// scrubPodSpec scrubs sensitive information from every kind of container in a pod spec.
func (s *Secrets) scrubPodSpec(spec corev1.PodSpec) corev1.PodSpec {
	for i, cont := range spec.Containers {
		spec.Containers[i] = s.scrubContainer(cont)
	}
	for i, cont := range spec.InitContainers {
		spec.InitContainers[i] = s.scrubContainer(cont)
	}
	for i, cont := range spec.EphemeralContainers {
		cont.Env = s.scrubEnv(cont.Env)
		spec.EphemeralContainers[i] = cont
	}
	return spec
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
	c.Env = s.scrubEnv(c.Env)
	return c
}

// scrubEnv redacts the value of every environment variable.
func (s *Secrets) scrubEnv(env []corev1.EnvVar) []corev1.EnvVar {
	for i, ev := range env {
		ev.Value = redacted
		env[i] = ev
	}
	return env
}
