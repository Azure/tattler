package routing

import (
	"testing"

	"github.com/Azure/tattler/data"
)

// BenchmarkHandleInputDrop measures routing an entry to a route that is not keeping up, so every push is a drop.
func BenchmarkHandleInputDrop(b *testing.B) {
	router := &Router{}
	if err := router.Register(b.Context(), "slow", make(chan data.Entry)); err != nil {
		b.Fatal(err)
	}
	entry := data.Entry{}

	b.ReportAllocs()
	for b.Loop() {
		router.route(b.Context(), entry)
	}
}
