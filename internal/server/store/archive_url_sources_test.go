package store

import (
	"encoding/json"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/pullref"
)

// An image imported from a URL carries both kinds of entry in one
// registry_sources list. These tests pin the two store-side readers of that
// list: neither may treat the archive URL as an image reference.
const (
	storeArchiveURL   = "https://containerimages.example/team-runner--gpu3.tar"
	storeCanonicalRef = "registry.example/team/runner:v1"
	storeMirrorRef    = "quay.io/team/runner:v1"
)

func storeWithArchiveURLImage(t *testing.T, dsn string) (*Store, *Image) {
	t.Helper()
	st, err := Open(dsn, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	sources, err := json.Marshal([]string{storeMirrorRef, storeArchiveURL})
	if err != nil {
		t.Fatal(err)
	}
	image, err := st.UpsertImage(&Image{ImageRef: storeCanonicalRef, Engine: "vllm", Enabled: true, RegistrySources: string(sources)})
	if err != nil {
		t.Fatal(err)
	}
	return st, image
}

func TestResolveImageRefNeverMatchesAnArchiveURLEntry(t *testing.T) {
	st, image := storeWithArchiveURLImage(t, "file:store_resolve_archive_url_test?mode=memory&cache=shared")

	// The declared registry ref still resolves to the catalog row.
	got, err := st.ResolveImageRef(storeMirrorRef)
	if err != nil || got.ID != image.ID {
		t.Fatalf("ResolveImageRef(%q) = %+v err=%v", storeMirrorRef, got, err)
	}
	// The archive URL is an address, not an identity: asking for it must not
	// resolve an image, or a deployment could be built around a tar file name.
	if got, err := st.ResolveImageRef(storeArchiveURL); err != ErrNotFound {
		t.Fatalf("ResolveImageRef(%q) = %+v err=%v, want ErrNotFound", storeArchiveURL, got, err)
	}
}

func TestImageCopiesExcludeArchiveURLEntries(t *testing.T) {
	st, _ := storeWithArchiveURLImage(t, "file:store_pull_refs_archive_url_test?mode=memory&cache=shared")
	refs := st.ImageCopies(storeCanonicalRef, nil)
	if strings.Join(refs, "|") != storeMirrorRef+"|"+storeCanonicalRef {
		t.Fatalf("copies = %v, want the mirror then the image", refs)
	}
	for _, ref := range refs {
		if pullref.IsArchiveURL(ref) {
			t.Fatalf("copies %v include an archive URL", refs)
		}
	}
}

func TestImageCopiesAreUnchangedByAnArchiveURLEntry(t *testing.T) {
	// The copies a node is planned from must be the same whether or not the
	// operator also declared an archive URL on the image.
	withURL, _ := storeWithArchiveURLImage(t, "file:store_pull_refs_with_url_test?mode=memory&cache=shared")
	plain, err := Open("file:store_pull_refs_without_url_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	sources, err := json.Marshal([]string{storeMirrorRef})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.UpsertImage(&Image{ImageRef: storeCanonicalRef, Engine: "vllm", Enabled: true, RegistrySources: string(sources)}); err != nil {
		t.Fatal(err)
	}
	got, want := withURL.ImageCopies(storeCanonicalRef, nil), plain.ImageCopies(storeCanonicalRef, nil)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("copies with URL = %v, without = %v", got, want)
	}
}
