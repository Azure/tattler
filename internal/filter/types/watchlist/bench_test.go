package watchlist

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
)

func BenchmarkFilterSingle(b *testing.B) {
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		in := make(chan watch.Event, 1)
		out := make(chan watch.Event, 1)
		_, err := New(context.Background(), in, out)
		if err != nil {
			panic(err)
		}

		wg := sync.WaitGroup{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range out {
			}
		}()

		b.StartTimer()
		for i := 0; i < 100000; i++ {
			in <- watch.Event{
				Type:   watch.Added,
				Object: &v1.Pod{ObjectMeta: metav1.ObjectMeta{UID: types.UID(uuid.New().String())}},
			}
		}
		close(in)
		wg.Wait()
	}

}
