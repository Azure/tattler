package safety

import (
	"testing"

	"github.com/Azure/tattler/data"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
)

// newSecretSpec returns a pod spec with a secret env value in a container, an init container and an ephemeral container.
func newSecretSpec() corev1.PodSpec {
	env := func() []corev1.EnvVar {
		return []corev1.EnvVar{{Name: "DB_PASSWORD", Value: "password123"}, {Name: "Does not matter", Value: "whatever"}}
	}
	return corev1.PodSpec{
		Containers:     []corev1.Container{{Name: "c", Env: env()}},
		InitContainers: []corev1.Container{{Name: "init", Env: env()}},
		EphemeralContainers: []corev1.EphemeralContainer{
			{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug", Env: env()}},
		},
	}
}

// specEnv returns every env var in every kind of container in spec.
func specEnv(spec corev1.PodSpec) []corev1.EnvVar {
	var env []corev1.EnvVar
	for _, c := range spec.Containers {
		env = append(env, c.Env...)
	}
	for _, c := range spec.InitContainers {
		env = append(env, c.Env...)
	}
	for _, c := range spec.EphemeralContainers {
		env = append(env, c.Env...)
	}
	return env
}

func TestScrubber(t *testing.T) {
	t.Parallel()

	meta := metav1.ObjectMeta{Name: "name", Namespace: "default", UID: k8stypes.UID("uid")}

	tests := []struct {
		name string
		obj  data.Entry
		// spec returns the pod spec held by the entry that comes out of the scrubber.
		spec func(data.Entry) (corev1.PodSpec, error)
	}{
		{
			name: "Success: pod env values are redacted in containers, init containers and ephemeral containers",
			obj:  data.MustNewEntry(&corev1.Pod{ObjectMeta: meta, Spec: newSecretSpec()}, data.STWatchList, data.CTAdd),
			spec: func(e data.Entry) (corev1.PodSpec, error) {
				p, err := e.Pod()
				if err != nil {
					return corev1.PodSpec{}, err
				}
				return p.Spec, nil
			},
		},
		{
			name: "Success: deployment pod template env values are redacted in containers, init containers and ephemeral containers",
			obj: data.MustNewEntry(
				&appsv1.Deployment{ObjectMeta: meta, Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: newSecretSpec()}}},
				data.STWatchList,
				data.CTAdd,
			),
			spec: func(e data.Entry) (corev1.PodSpec, error) {
				d, err := e.Deployment()
				if err != nil {
					return corev1.PodSpec{}, err
				}
				return d.Spec.Template.Spec, nil
			},
		},
	}

	for _, test := range tests {
		s := &Secrets{out: make(chan data.Entry, 1)}
		s.scrubber(t.Context(), test.obj)

		spec, err := test.spec(<-s.out)
		if err != nil {
			t.Errorf("TestScrubber(%s): got err == %s, want err == nil", test.name, err)
			continue
		}
		env := specEnv(spec)
		if len(env) != 6 {
			t.Errorf("TestScrubber(%s): got %d env vars, want 6", test.name, len(env))
		}
		for _, ev := range env {
			if ev.Value != redacted {
				t.Errorf("TestScrubber(%s): got env %s value %q, want %q", test.name, ev.Name, ev.Value, redacted)
			}
		}
	}
}

// Commented out because we are scrubbing everything at the moment.  In the future,
// we may want to scrub only certain fields again.
/*
func TestScrubber(t *testing.T) {
	t.Parallel()

	secretName := "DB_PASSWORD"

	secretEnv := corev1.EnvVar{Name: secretName, Value: "password123"}
	nonSecretEnv := corev1.EnvVar{Name: "non-special", Value: "value"}
	redactedEnv := corev1.EnvVar{Name: secretName, Value: "REDACTED"}

	tests := []struct {
		name    string
		data    data.Entry
		want    data.Entry
		wantErr bool
	}{
		{
			name: "Type is not a pod",
			data: data.MustNewEntry(&corev1.Node{}, data.STInformer, data.CTAdd),
			want: data.MustNewEntry(&corev1.Node{}, data.STInformer, data.CTAdd),
		},
		{
			name: "Success",
			data: data.MustNewEntry(
				&corev1.Pod{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Env: []corev1.EnvVar{secretEnv, nonSecretEnv, secretEnv},
							},
							{
								Env: []corev1.EnvVar{secretEnv, nonSecretEnv, secretEnv},
							},
						},
					},
				},
				data.STInformer,
				data.CTAdd,
			),
			want: data.MustNewEntry(
				&corev1.Pod{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Env: []corev1.EnvVar{redactedEnv, nonSecretEnv, redactedEnv},
							},
							{
								Env: []corev1.EnvVar{redactedEnv, nonSecretEnv, redactedEnv},
							},
						},
					},
				},
				data.STInformer,
				data.CTAdd,
			),
		},
	}

	for _, test := range tests {
		s := &Secrets{out: make(chan data.Entry, 1)}
		err := s.scrubber(test.data)
		switch {
		case err == nil && test.wantErr:
			t.Errorf("TestScrubInformer(%s): got err == nil, want err != nil", test.name)
			continue
		case err != nil && !test.wantErr:
			t.Errorf("TestScrubInformer(%s): got err == %v, want err == nil", test.name, err)
			continue
		case err != nil:
			continue
		}

		if diff := pretty.Compare(test.want, <-s.out); diff != "" {
			t.Errorf("TestScrubInformer(%s): -want/+got:\n%s", test.name, diff)
		}
	}
}

func TestScrubPod(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Env: []corev1.EnvVar{
						{
							Name:  "DB_PASSWORD",
							Value: "password123",
						},
					},
				},
			},
		},
	}

	s := &Secrets{}
	s.scrubPod(pod)

	if pod.Spec.Containers[0].Env[0].Value != "REDACTED" {
		t.Errorf("TestScrubPod: got %s, want REDACTED", pod.Spec.Containers[0].Env[0].Value)
	}
}

func TestScrubContainer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		container corev1.Container
		want      corev1.Container
	}{
		{
			name: "No sensitive information",
			container: corev1.Container{
				Env: []corev1.EnvVar{
					{
						Name:  "MY_ENV",
						Value: "my-value",
					},
				},
			},
			want: corev1.Container{
				Env: []corev1.EnvVar{
					{
						Name:  "MY_ENV",
						Value: "my-value",
					},
				},
			},
		},
		{
			name: "Sensitive information present",
			container: corev1.Container{
				Env: []corev1.EnvVar{
					{
						Name:  "DB_PASSWORD",
						Value: "password123",
					},
					{
						Name:  "API_KEY",
						Value: "secretkey",
					},
					{
						Name:  "MY_ENV",
						Value: "my-value",
					},
				},
			},
			want: corev1.Container{
				Env: []corev1.EnvVar{
					{
						Name:  "DB_PASSWORD",
						Value: "REDACTED",
					},
					{
						Name:  "API_KEY",
						Value: "REDACTED",
					},
					{
						Name:  "MY_ENV",
						Value: "my-value",
					},
				},
			},
		},
	}

	for _, test := range tests {
		s := &Secrets{}
		s.scrubContainer(test.container)

		if diff := pretty.Compare(test.want, test.container); diff != "" {
			t.Errorf("TestScrubContainer(%s): -want/+got:\n%s", test.name, diff)
		}
	}
}
*/
