package store

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestDeleteStorageSelectedModelsPreserveGuards(t *testing.T) {
	for _, reason := range []string{"referenced_model", "pending_deletion_on_other_storage", "missing_selected_model"} {
		t.Run(reason, func(t *testing.T) {
			f := newStorageFixture(t)
			file, err := f.st.UpsertModelFile(&ModelFile{ModelID: f.model.ID, StorageID: f.nfs.ID,
				Path: "model", State: proto.MFPresent})
			if err != nil {
				t.Fatal(err)
			}
			profile := Profile{ModelID: f.model.ID, Fingerprint: "selected-model"}
			if err := f.st.DB().Create(&profile).Error; err != nil {
				t.Fatal(err)
			}
			modelIDs := []uint{f.model.ID}
			var users []uint
			switch reason {
			case "referenced_model":
				dep, err := f.st.CreateDeployment(&Deployment{Name: "reader", ServedName: "reader",
					ModelID: f.model.ID, State: "stopped"})
				if err != nil {
					t.Fatal(err)
				}
				users = []uint{dep.ID}
			case "pending_deletion_on_other_storage":
				if _, err := f.st.UpsertModelFile(&ModelFile{ModelID: f.model.ID, StorageID: f.cache.ID,
					NodeID: f.n1.ID, Path: "/cache/model", State: proto.MFDeleting, TransferID: "pending"}); err != nil {
					t.Fatal(err)
				}
			case "missing_selected_model":
				removed, err := f.st.UpsertModel(&Model{Name: "removed", Source: "local", SourceID: "removed"})
				if err != nil {
					t.Fatal(err)
				}
				if err := f.st.DeleteModel(removed.ID); err != nil {
					t.Fatal(err)
				}
				modelIDs = append(modelIDs, removed.ID)
			}
			sub := f.st.Bus.Subscribe(8, "storage", "model", "modelfile")
			defer sub.Close()
			err = f.st.DeleteStorage(f.nfs.ID, modelIDs...)
			switch reason {
			case "referenced_model":
				var inUse *StorageInUseError
				if !errors.As(err, &inUse) || !slices.Equal(inUse.DeploymentIDs, users) {
					t.Fatalf("selected model references must block the transaction: %v", err)
				}
			case "pending_deletion_on_other_storage":
				if !errors.Is(err, ErrCacheDeletionPending) {
					t.Fatalf("selected model's unfinished deletion must be protected: %v", err)
				}
			case "missing_selected_model":
				if err == nil || !strings.Contains(err.Error(), "model cleanup deleted") {
					t.Fatalf("incomplete model cleanup must roll back: %v", err)
				}
			}
			if _, err := f.st.StorageByID(f.nfs.ID); err != nil {
				t.Fatalf("failed transaction removed the storage: %v", err)
			}
			if err := f.st.DB().First(&Model{}, f.model.ID).Error; err != nil {
				t.Fatalf("failed transaction removed the model: %v", err)
			}
			if got, err := f.st.ModelFileByID(file.ID); err != nil || got.StorageID != f.nfs.ID {
				t.Fatalf("failed transaction changed the registration: %+v, %v", got, err)
			}
			if err := f.st.DB().First(&Profile{}, profile.ID).Error; err != nil {
				t.Fatalf("failed transaction removed the profile: %v", err)
			}
			select {
			case event := <-sub.C():
				t.Fatalf("failed transaction published a removal: %+v", event)
			default:
			}
		})
	}
}
