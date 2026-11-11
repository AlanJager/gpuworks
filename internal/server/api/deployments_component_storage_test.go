package api

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// componentRig is the save-to rig plus a template component (a text encoder
// the template fetches beside the primary).
func componentRig(t *testing.T) (*saveToRig, *store.Model) {
	t.Helper()
	r := newSaveToRig(t)
	component, err := r.st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "org/base", Revision: "abc", ArtifactSubfolder: "text_encoder"})
	if err != nil {
		t.Fatal(err)
	}
	return r, component
}

// MiniMax-H3-Image on the H20 server: the primary was already on the default
// NFS, the request named no storage, and the text encoder component was
// downloaded into the node's local storage. The create now fetches such a
// component into the shared storage the primary sits on.
func TestComponentSaveToFollowsThePrimarysSharedStorage(t *testing.T) {
	t.Run("a missing component goes beside the primary", func(t *testing.T) {
		r, component := componentRig(t)
		r.nfsState(t, proto.StorageStateReady, true)
		r.nfsHoldsModel(t)
		got, err := r.h.componentSaveTo(nil, r.model, []*store.Model{component}, nil, []*store.Node{r.node})
		if err != nil || got == nil || got.ID != r.nfs.ID {
			t.Fatalf("componentSaveTo = %+v, %v; want %s", got, err, r.nfs.Name)
		}
		if got, err := r.h.componentSaveTo(&dto.ModelSource{}, r.model, []*store.Model{component}, nil, []*store.Node{r.node}); err != nil || got == nil || got.ID != r.nfs.ID {
			t.Fatalf("an empty model source = %+v, %v; want %s", got, err, r.nfs.Name)
		}
	})
	t.Run("a named storage keeps its say", func(t *testing.T) {
		r, component := componentRig(t)
		r.nfsState(t, proto.StorageStateReady, true)
		r.nfsHoldsModel(t)
		named := &dto.ModelSource{StorageID: r.st.DefaultLocalStorageID()}
		if got, err := r.h.componentSaveTo(named, r.model, []*store.Model{component}, nil, []*store.Node{r.node}); err != nil || got != nil {
			t.Fatalf("componentSaveTo = %+v, %v; want nil", got, err)
		}
	})
	t.Run("nothing to do or nowhere to put it", func(t *testing.T) {
		r, component := componentRig(t)
		r.nfsState(t, proto.StorageStateReady, true)
		if got, err := r.h.componentSaveTo(nil, r.model, []*store.Model{component}, nil, []*store.Node{r.node}); err != nil || got != nil {
			t.Fatalf("primary not on the NFS = %+v, %v; want nil", got, err)
		}
		r.nfsHoldsModel(t)
		if got, err := r.h.componentSaveTo(nil, r.model, nil, nil, []*store.Node{r.node}); err != nil || got != nil {
			t.Fatalf("no components = %+v, %v; want nil", got, err)
		}
		if got, err := r.h.componentSaveTo(nil, r.model, []*store.Model{component}, []uint{r.st.DefaultLocalStorageID()}, []*store.Node{r.node}); err != nil || got != nil {
			t.Fatalf("pinned to local storage = %+v, %v; want nil", got, err)
		}
		if _, err := r.st.UpsertModelFile(&store.ModelFile{ModelID: component.ID, StorageID: r.nfs.ID, Path: store.SharedModelRelPath(component), State: proto.MFPresent, SizeBytes: 10}); err != nil {
			t.Fatal(err)
		}
		if got, err := r.h.componentSaveTo(nil, r.model, []*store.Model{component}, nil, []*store.Node{r.node}); err != nil || got != nil {
			t.Fatalf("component already readable = %+v, %v; want nil", got, err)
		}
	})
	t.Run("a storage that takes no new files is skipped", func(t *testing.T) {
		r, component := componentRig(t)
		r.nfsState(t, proto.StorageStateReady, false)
		r.nfsHoldsModel(t)
		if got, err := r.h.componentSaveTo(nil, r.model, []*store.Model{component}, nil, []*store.Node{r.node}); err != nil || got != nil {
			t.Fatalf("read-only NFS = %+v, %v; want nil", got, err)
		}
	})
	t.Run("the component download is one write through a writer", func(t *testing.T) {
		r, component := componentRig(t)
		r.nfsState(t, proto.StorageStateReady, true)
		r.nfsHoldsModel(t)
		r.h.d.NodeSync = nodeSyncCallbacks{full: func(string) error { return nil }}
		beside, err := r.h.componentSaveTo(nil, r.model, []*store.Model{component}, nil, []*store.Node{r.node})
		if err != nil || beside == nil {
			t.Fatalf("componentSaveTo = %+v, %v", beside, err)
		}
		if _, err := r.h.startSharedModelDownload(component, beside, []*store.Node{r.node}); err != nil {
			t.Fatal(err)
		}
		row, err := r.st.ModelFileAt(store.FileRef{ModelID: component.ID, StorageID: r.nfs.ID})
		if err != nil || row.State != proto.MFPending || row.WriterNodeID != r.node.ID || row.Path != store.SharedModelRelPath(component) {
			t.Fatalf("component row = %+v, %v", row, err)
		}
		// The node now waits for that write instead of acquiring locally.
		src, err := r.st.ModelSourceOn(component.ID, r.node, store.SourceRules{})
		if err != nil || src.Action != store.SourceWaitShared || src.Location.Storage.ID != r.nfs.ID {
			t.Fatalf("source = %+v, %v; want to wait for %s", src, err, r.nfs.Name)
		}
	})
}

func TestTemplateComponentModelsSkipsThePrimary(t *testing.T) {
	r, component := componentRig(t)
	got, err := r.h.templateComponentModels([]deploy.ComponentBinding{
		{Name: "image", Role: "primary", StoreModelID: r.model.ID},
		{Name: "text-encoder", Role: "auxiliary", StoreModelID: component.ID},
		{Name: "unresolved", Role: "auxiliary"},
	})
	if err != nil || len(got) != 1 || got[0].ID != component.ID {
		t.Fatalf("templateComponentModels = %+v, %v", got, err)
	}
	if _, err := r.h.templateComponentModels([]deploy.ComponentBinding{{Name: "gone", Role: "auxiliary", StoreModelID: 9999}}); err == nil {
		t.Fatal("a component whose model row is gone must fail the create")
	}
}
