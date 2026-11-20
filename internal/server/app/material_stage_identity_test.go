//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"testing"
)

func materialStageIdentityFixture(t *testing.T) (*os.Root, *os.Root, []byte, string) {
	t.Helper()
	dataDir := t.TempDir()
	replicas := filepath.Join(dataDir, "material-replicas")
	if err := os.Mkdir(replicas, 0700); err != nil {
		t.Fatal(err)
	}
	receiver, err := newMaterialReceiver(replicas, 2)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("original verified material bytes")
	digest := materialTestDigest(body)
	if _, err := receiver.receive(t.Context(), digest, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dataDir, materialStageDirectory), 0700); err != nil {
		t.Fatal(err)
	}
	source, err := os.OpenRoot(replicas)
	if err != nil {
		t.Fatal(err)
	}
	target, err := os.OpenRoot(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := errors.Join(source.Close(), target.Close()); err != nil {
			t.Error(err)
		}
	})
	return source, target, body, digest
}

func writeMaterialIdentityForeign(t *testing.T, root *os.Root, name string, body []byte) {
	t.Helper()
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.Write(body)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
}

func TestReceivedMaterialRejectsReplacedVerifiedStage(t *testing.T) {
	source, target, body, digest := materialStageIdentityFixture(t)
	name := "model-logos/" + digest + ".png"
	foreign := bytes.Repeat([]byte("x"), len(body))
	var stage, saved string
	link := func(_ context.Context, root *os.Root, temporary, final string) error {
		stage, saved = temporary, temporary+".saved"
		if err := root.Rename(stage, saved); err != nil {
			t.Fatal(err)
		}
		writeMaterialIdentityForeign(t, root, stage, foreign)
		return root.Link(stage, final)
	}
	err := copyReceivedMaterialWithLink(t.Context(), source, target, name, digest, int64(len(body)), true, link)
	if err == nil {
		t.Error("replacement of the verified stage was acknowledged")
	}
	for _, item := range []struct {
		name string
		want []byte
	}{{stage, foreign}, {name, foreign}, {saved, body}} {
		got, readErr := target.ReadFile(item.name)
		if readErr != nil || !bytes.Equal(got, item.want) {
			t.Errorf("identity-qualified cleanup changed %s: %q %v", item.name, got, readErr)
		}
	}
	marker := path.Join("model-logos", receivedModelLogoOriginDirectory, path.Base(name))
	if _, err := target.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("foreign linked inode acquired an origin marker: %v", err)
	}
}

func TestReceivedMaterialPreservesReplacementAfterLink(t *testing.T) {
	source, target, body, digest := materialStageIdentityFixture(t)
	name := "model-logos/" + digest + ".png"
	foreign := []byte("a separately owned final replacement")
	var stage string
	link := func(_ context.Context, root *os.Root, temporary, final string) error {
		stage = temporary
		if err := root.Link(stage, final); err != nil {
			return err
		}
		if err := root.Remove(final); err != nil {
			t.Fatal(err)
		}
		writeMaterialIdentityForeign(t, root, final, foreign)
		return nil
	}
	if err := copyReceivedMaterialWithLink(t.Context(), source, target, name, digest, int64(len(body)), true, link); err == nil {
		t.Fatal("replacement after Link was acknowledged")
	}
	if got, err := target.ReadFile(name); err != nil || !bytes.Equal(got, foreign) {
		t.Fatalf("foreign final was removed or altered: %q %v", got, err)
	}
	if _, err := target.Lstat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("original owned stage was not cleaned: %v", err)
	}
	marker := path.Join("model-logos", receivedModelLogoOriginDirectory, path.Base(name))
	if _, err := target.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement final acquired an origin marker: %v", err)
	}
}

func TestReceivedMaterialRejectsWriteThroughPublishedInode(t *testing.T) {
	source, target, body, digest := materialStageIdentityFixture(t)
	name := "model-logos/" + digest + ".png"
	foreign := bytes.Repeat([]byte("z"), len(body))
	var stage string
	link := func(_ context.Context, root *os.Root, temporary, final string) error {
		stage = temporary
		if err := root.Link(stage, final); err != nil {
			return err
		}
		file, err := root.OpenFile(final, os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := file.Write(foreign)
		return errors.Join(writeErr, file.Close())
	}
	if err := copyReceivedMaterialWithLink(t.Context(), source, target, name, digest, int64(len(body)), true, link); err == nil {
		t.Fatal("writes through the linked inode were acknowledged")
	}
	if got, err := target.ReadFile(name); err != nil || !bytes.Equal(got, foreign) {
		t.Fatalf("final name was removed or altered: %q %v", got, err)
	}
	if _, err := target.Lstat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("original owned stage was not cleaned: %v", err)
	}
	marker := path.Join("model-logos", receivedModelLogoOriginDirectory, path.Base(name))
	if _, err := target.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("changed final bytes acquired an origin marker: %v", err)
	}
}

func TestReceivedMaterialStageParentReplacementUsesHeldCleanup(t *testing.T) {
	source, target, body, digest := materialStageIdentityFixture(t)
	name := "model-logos/" + digest + ".png"
	foreign := bytes.Repeat([]byte("p"), len(body))
	var stage string
	moved := materialStageDirectory + ".saved"
	link := func(_ context.Context, root *os.Root, temporary, final string) error {
		stage = temporary
		if err := root.Rename(materialStageDirectory, moved); err != nil {
			t.Fatal(err)
		}
		if err := root.Mkdir(materialStageDirectory, 0700); err != nil {
			t.Fatal(err)
		}
		writeMaterialIdentityForeign(t, root, stage, foreign)
		return root.Link(stage, final)
	}
	if err := copyReceivedMaterialWithLink(t.Context(), source, target, name, digest, int64(len(body)), true, link); err == nil {
		t.Fatal("replacement of the original stage parent was acknowledged")
	}
	for _, entry := range []string{stage, name} {
		if got, err := target.ReadFile(entry); err != nil || !bytes.Equal(got, foreign) {
			t.Fatalf("foreign entry was removed or altered: %s %q %v", entry, got, err)
		}
	}
	if _, err := target.Lstat(path.Join(moved, path.Base(stage))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleanup did not use the original held parent: %v", err)
	}
	marker := path.Join("model-logos", receivedModelLogoOriginDirectory, path.Base(name))
	if _, err := target.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("foreign parent entry acquired an origin marker: %v", err)
	}
}

func TestReceivedMaterialPreservesLinkAndActualCleanupFailure(t *testing.T) {
	source, target, body, digest := materialStageIdentityFixture(t)
	linkFailure := errors.New("publication rejected by existing link seam")
	var stage string
	stageDirectory := filepath.Join(target.Name(), materialStageDirectory)
	t.Cleanup(func() {
		if err := os.Chmod(stageDirectory, 0700); err != nil {
			t.Error(err)
		}
	})
	link := func(_ context.Context, _ *os.Root, temporary, _ string) error {
		stage = temporary
		if err := os.Chmod(stageDirectory, 0500); err != nil {
			t.Fatal(err)
		}
		return linkFailure
	}
	err := copyReceivedMaterialWithLink(t.Context(), source, target, "model-logos/cleanup.png", digest, int64(len(body)), true, link)
	if !errors.Is(err, linkFailure) || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("actual removal failure or original link cause was lost: %v", err)
	}
	if got, err := target.ReadFile(stage); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("failed actual cleanup did not retain original bytes: %q %v", got, err)
	}
}
