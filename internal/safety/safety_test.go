package safety

import (
	"testing"

	"github.com/Azure/tattler/data"

	"github.com/kylelemons/godebug/pretty"
	corev1 "k8s.io/api/core/v1"
)

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
