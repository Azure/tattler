package preprocess

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Azure/tattler/data"

	"github.com/kylelemons/godebug/pretty"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var procs = []Processor{
	func(ctx context.Context, e data.Entry) error {
		if e == (data.Entry{}) {
			return fmt.Errorf("entry is empty")
		}
		return nil
	},
	func(ctx context.Context, e data.Entry) error {
		if e.ObjectType() == data.OTNode {
			n, _ := e.Node()
			n.Name = "test"
		}
		return nil
	},
}

func TestRun(t *testing.T) {
	t.Parallel()

	in := make(chan data.Entry, 1)
	out := make(chan data.Entry, 1)
	want := data.MustNewEntry(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "test"}}, data.STInformer, data.CTAdd)

	_, err := New(context.Background(), in, out, procs)
	if err != nil {
		panic(err)
	}

	in <- data.MustNewEntry(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "bad name"}}, data.STInformer, data.CTAdd)
	close(in)

	if diff := pretty.Compare(want, <-out); diff != "" {
		t.Errorf("TestRun: -want/+got:\n%s", diff)
	}

	if diff := pretty.Compare(data.Entry{}, <-out); diff != "" {
		t.Errorf("TestRun: didn't close out channel when in was closed")
	}
}

func TestProcessEntry(t *testing.T) {
	t.Parallel()

	// Deletion and update timestamp are set so that output is deterministic.
	meta := metav1.ObjectMeta{
		UID:               "123",
		DeletionTimestamp: &metav1.Time{Time: time.Now()},
		ManagedFields: []metav1.ManagedFieldsEntry{
			{
				Manager:    "kubelet",
				Operation:  "Update",
				FieldsType: "FieldsV1",
				APIVersion: "v1",
				Time:       &metav1.Time{Time: time.Now()},
			},
		},
	}

	tests := []struct {
		name    string
		in      data.Entry
		want    data.Entry
		wantErr bool
	}{
		{
			name:    "empty",
			in:      data.Entry{},
			wantErr: true,
		},
		{
			name: "node will have its name changed",
			in:   data.MustNewEntry(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "bad name"}}, data.STInformer, data.CTAdd),
			want: data.MustNewEntry(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "test"}}, data.STInformer, data.CTAdd),
		},
		{
			name: "pod will not have its name changed",
			in:   data.MustNewEntry(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "bad name", DeletionTimestamp: meta.DeletionTimestamp, ManagedFields: meta.ManagedFields}}, data.STInformer, data.CTDelete),
			want: data.MustNewEntry(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "bad name", DeletionTimestamp: meta.DeletionTimestamp, ManagedFields: meta.ManagedFields}}, data.STInformer, data.CTDelete),
		},
	}

	for _, test := range tests {
		r := Runner{procs: procs}

		err := r.processEntry(context.Background(), test.in)
		switch {
		case err == nil && test.wantErr:
			t.Errorf("ProcessEntry(%s): got err == nil, want err != nil", test.name)
			continue
		case err != nil && !test.wantErr:
			t.Errorf("ProcessEntry(%s): got err == %v, want err == nil", test.name, err)
			continue
		case err != nil:
			continue
		}

		if diff := pretty.Compare(test.want, test.in); diff != "" {
			t.Errorf("ProcessEntry(%s): -want/+got:\n%s", test.name, diff)
		}
	}
}
