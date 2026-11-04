package clipboard

import (
	"slices"
	"testing"
)

// A file manager's copy is a uri-list (GNOME's starts with copy or cut):
// the local files in it, decoded.
func TestFileURIs(t *testing.T) {
	got := fileURIs("copy\nfile:///home/op/a%20b.png\r\nfile://otherhost/x\nhttps://example.com/y\nfile:///tmp/c.txt\n")
	if want := []string{"/home/op/a b.png", "/tmp/c.txt"}; !slices.Equal(got, want) {
		t.Fatalf("fileURIs = %q, want %q", got, want)
	}
}
