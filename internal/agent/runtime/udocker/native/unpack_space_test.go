package native

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An unpack the disk cannot hold is refused before it starts, naming the free
// space and what was needed, and leaves nothing behind; with room it goes
// ahead, and unknown free space never refuses.
func TestCreateRefusesAnUnpackTheDiskCannotHold(t *testing.T) {
	p := newImageParts(t)
	layout := Layout{Dir: t.TempDir()}
	if _, err := layout.LoadArchive(context.Background(), ParseRef("team/app:v1"), amd64, bytes.NewReader(classicArchive(t, p, "team/app:v1"))); err != nil {
		t.Fatal(err)
	}
	var asked []string
	free := int64(unpackReserve) // the reserve alone: the layers do not fit
	engine := &Engine{Layout: layout, Client: NewClient(), Reflink: func(string) bool { return false },
		FreeSpace: func(path string) int64 { asked = append(asked, path); return free }}

	err := engine.Create(context.Background(), "full", "team/app:v1", nil)
	if err == nil || !strings.Contains(err.Error(), "not enough disk space to unpack the image") || !strings.Contains(err.Error(), "free up space") {
		t.Fatalf("create on a full disk = %v", err)
	}
	if len(asked) != 1 || asked[0] != filepath.Join(layout.Dir, "containers") {
		t.Fatalf("free space asked for %v, want the containers directory", asked)
	}
	for _, leftover := range []string{"full", "full" + CreateStagingSuffix} {
		if _, err := os.Stat(filepath.Join(layout.Dir, "containers", leftover)); !os.IsNotExist(err) {
			t.Fatalf("a refused create left %s behind: %v", leftover, err)
		}
	}

	free = 64 << 30
	if err := engine.Create(context.Background(), "roomy", "team/app:v1", nil); err != nil {
		t.Fatalf("create with room: %v", err)
	}
	free = -1
	if err := engine.Create(context.Background(), "unknown", "team/app:v1", nil); err != nil {
		t.Fatalf("create with unknown free space: %v", err)
	}
}
