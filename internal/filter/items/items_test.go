package items

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

type fakeObject struct {
	uuid            types.UID
	resourceVersion string
	generation      int64
}

func (f fakeObject) GetUID() types.UID {
	return f.uuid
}

func (f fakeObject) GetResourceVersion() string {
	return f.resourceVersion
}

func (f fakeObject) GetGeneration() int64 {
	return f.generation
}

func TestIsState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		item Item
		obj  Object
		want Age
	}{
		{
			name: "Equal",
			item: Item{
				ResourceVersion: "1",
				Generation:      0,
			},
			obj: fakeObject{
				resourceVersion: "1",
				generation:      0,
			},
			want: Equal,
		},
		{
			name: "Newer",
			item: Item{
				ResourceVersion: "2",
				Generation:      1,
			},
			obj: fakeObject{
				resourceVersion: "1",
				generation:      0,
			},
			want: Newer,
		},
		{
			name: "Older",
			item: Item{
				ResourceVersion: "1",
				Generation:      0,
			},
			obj: fakeObject{
				resourceVersion: "2",
				generation:      1,
			},
			want: Older,
		},
	}

	for _, test := range tests {
		got := test.item.IsState(test.obj)
		if got != test.want {
			t.Errorf("TestIsState(%s): got == %v, want got == %v", test.name, got, test.want)
		}
	}
}
