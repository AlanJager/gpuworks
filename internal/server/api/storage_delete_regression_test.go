package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestStorageDeleteOtherCopyState(t *testing.T) {
	for _, state := range []string{
		proto.MFPresent, proto.MFRegistered, proto.MFMissing, proto.MFAbsent,
		proto.MFFailed, proto.MFDownloadRequired, proto.MFInsufficientDisk,
	} {
		t.Run(state, func(t *testing.T) {
			f := newStorageDeleteFixture(t)
			other := f.storage("other")
			keep := state == proto.MFPresent || state == proto.MFRegistered
			locations := []*store.Storage{f.target}
			if keep {
				locations = append(locations, other)
			}
			m := f.model("model", locations...)
			file, err := f.st.UpsertModelFile(&store.ModelFile{
				ModelID: m.ID, StorageID: other.ID, Path: m.Name, State: proto.MFPresent,
			})
			if err != nil {
				t.Fatal(err)
			}
			if state == proto.MFMissing {
				// Exercise the real transition: the scan confirms that the
				// other storage no longer contains this model's directory.
				scan, err := f.st.ApplyStorageScan(other.ID, store.ScanSighting{
					Covers: func(path string) bool { return path == m.Name },
					Gone:   func(path string) bool { return path == m.Name },
				})
				if err != nil || scan.Missing != 1 {
					t.Fatalf("confirm missing copy: %+v, %v", scan, err)
				}
			} else if err := f.st.SetModelFileState(file.ID, state, ""); err != nil {
				t.Fatal(err)
			}
			f.delete(http.StatusNoContent)
			f.expectStorage(f.target, http.StatusNotFound)
			f.expectStorage(other, http.StatusOK)
			f.expectModel(m, keep)
			if keep {
				f.expectFileStorages(m, other.ID)
			} else if files, err := f.st.ModelFilesForModel(m.ID); err != nil || len(files) != 0 {
				t.Fatalf("removed model retains file registrations: %+v, %v", files, err)
			}
		})
	}
}

func TestStorageDeletePreservesOtherUnfinishedOperations(t *testing.T) {
	for _, state := range []string{proto.MFPending, proto.MFDownloading, proto.MFSyncing, proto.MFDeleting} {
		t.Run(state, func(t *testing.T) {
			f := newStorageDeleteFixture(t)
			other := f.storage("other")
			m := f.model("in-progress", f.target)
			file, err := f.st.UpsertModelFile(&store.ModelFile{
				ModelID: m.ID, StorageID: other.ID, Path: m.Name, State: state, TransferID: "unfinished",
			})
			if err != nil {
				t.Fatal(err)
			}
			f.delete(http.StatusNoContent)
			f.expectModel(m, true)
			f.expectFileStorages(m, other.ID)
			got, err := f.st.ModelFileByID(file.ID)
			if err != nil || got.State != state || got.TransferID != file.TransferID {
				t.Fatalf("other storage's unfinished operation changed: %+v, %v", got, err)
			}
		})
	}
}

func TestStorageDeleteIdentityFailure(t *testing.T) {
	for _, operation := range []string{"read", "write"} {
		t.Run(operation, func(t *testing.T) {
			var calls, syncs int
			var logs bytes.Buffer
			f := newStorageDeleteFixture(t, func(d *Deps) {
				d.Log = slog.New(slog.NewTextHandler(&logs, nil))
				d.CallNode = func(context.Context, string, string, any) (json.RawMessage, error) {
					calls++
					return json.RawMessage(`{"peers":[]}`), nil
				}
				d.NodeSync = nodeSyncCallbacks{full: func(string) error { syncs++; return nil }}
			})
			m := f.model("only", f.target)
			identity := f.st.ControlPlaneIdentity()
			failure := "injected identity " + operation + " failure"
			if operation == "read" {
				if err := f.st.DB().Callback().Query().Before("gorm:query").Register("test:identity-read", func(tx *gorm.DB) {
					if tx.Statement.Table == "settings" {
						tx.AddError(errors.New(failure))
					}
				}); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := f.st.DeleteSetting(store.SettingControlPlane); err != nil {
					t.Fatal(err)
				}
				sql := fmt.Sprintf("CREATE TRIGGER reject_identity BEFORE INSERT ON settings WHEN NEW.key = '%s' BEGIN SELECT RAISE(ABORT, 'injected identity write failure'); END", store.SettingControlPlane)
				if err := f.st.DB().Exec(sql).Error; err != nil {
					t.Fatal(err)
				}
			}
			body := f.request(http.MethodDelete, fmt.Sprintf("/api/v1/storages/%d", f.target.ID), http.StatusNoContent)
			if len(body) != 0 || !strings.Contains(logs.String(), failure) || !strings.Contains(logs.String(), fmt.Sprintf("storage_id=%d", f.target.ID)) {
				t.Errorf("committed delete must log identity failure and return empty 204: body=%s log=%s", body, logs.String())
			}
			if operation == "read" {
				if err := f.st.DB().Callback().Query().Remove("test:identity-read"); err != nil {
					t.Fatal(err)
				}
				if got := f.st.ControlPlaneIdentity(); got != identity {
					t.Errorf("failed identity read replaced persisted identity: %+v, want %+v", got, identity)
				}
			} else if raw, err := f.st.LookupSetting(store.SettingControlPlane); err != nil || raw != "" {
				t.Errorf("failed identity write left a setting: %q, %v", raw, err)
			}
			if calls != 0 {
				t.Errorf("RPC called %d times without a persisted identity", calls)
			}
			if syncs != 1 {
				t.Errorf("committed storage deletion needs one node sync, got %d", syncs)
			}
			f.expectStorage(f.target, http.StatusNotFound)
			f.expectModel(m, false)
		})
	}
}

func TestStorageDeleteUsesPersistedIdentity(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
			var calls int
			f := newStorageDeleteFixture(t, func(d *Deps) {
				d.CallNode = func(_ context.Context, _ string, method string, params any) (json.RawMessage, error) {
					calls++
					p, ok := params.(proto.StoragePeersParams)
					if method != proto.RPCStoragePeers || !ok || !p.Remove {
						t.Fatalf("unexpected RPC: %s, %+v", method, params)
					}
					raw, err := d.Store.LookupSetting(store.SettingControlPlane)
					var identity store.ControlPlaneIdentity
					if err != nil || json.Unmarshal([]byte(raw), &identity) != nil || identity.ID == "" {
						t.Fatalf("RPC requires a persisted identity: %q, %v", raw, err)
					}
					if p.ControlPlaneID != identity.ID || p.ControlPlaneName != identity.Name {
						t.Errorf("RPC identity = %+v, persisted = %+v", p, identity)
					}
					return json.RawMessage(`{"peers":[]}`), nil
				}
			})
			m := f.model("only", f.target)
			var identity store.ControlPlaneIdentity
			var err error
			if existing {
				identity, err = f.st.SetControlPlaneName("custom-control-plane")
			} else {
				err = f.st.DeleteSetting(store.SettingControlPlane)
			}
			if err != nil {
				t.Fatal(err)
			}
			f.delete(http.StatusNoContent)
			if calls != 1 {
				t.Errorf("cleanup RPC attempts = %d, want one", calls)
			}
			if existing {
				if got := f.st.ControlPlaneIdentity(); got != identity {
					t.Errorf("delete changed identity: %+v, want %+v", got, identity)
				}
			}
			f.expectStorage(f.target, http.StatusNotFound)
			f.expectModel(m, false)
		})
	}
}

func TestStorageControlPlanesPreservesQueryFailures(t *testing.T) {
	for _, tc := range []struct {
		name, firstReply  string
		firstErr, lastErr error
		status, calls     int
	}{
		{"bad_reply", `{"peers":`, nil, nil, http.StatusConflict, 1},
		{"disconnected", "", errors.New("injected first disconnect"), nil, http.StatusOK, 2},
		{"all_disconnected", "", errors.New("injected first disconnect"), errors.New("injected last disconnect"), http.StatusConflict, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			f := newStorageDeleteFixture(t, func(d *Deps) {
				d.CallNode = func(_ context.Context, _ string, method string, params any) (json.RawMessage, error) {
					if method != proto.RPCStoragePeers || params.(proto.StoragePeersParams).Remove {
						t.Fatalf("query sent removal RPC: %s %+v", method, params)
					}
					calls++
					if calls == 1 {
						return json.RawMessage(tc.firstReply), tc.firstErr
					}
					return json.RawMessage(`{"peers":[]}`), tc.lastErr
				}
			})
			second, err := f.st.RegisterNode("second-writer", store.HashToken("second"), "second")
			if err != nil {
				t.Fatal(err)
			}
			if err := f.st.ApplyStorageStatus(second.ID, proto.StorageStatusReport{Storages: f.statuses}); err != nil {
				t.Fatal(err)
			}
			body := f.request(http.MethodGet, fmt.Sprintf("/api/v1/storages/%d/control-planes", f.target.ID), tc.status)
			if calls != tc.calls {
				t.Fatalf("query tried %d writers, want %d", calls, tc.calls)
			}
			if tc.lastErr != nil && (!strings.Contains(string(body), "no online node can write to this storage") || strings.Contains(string(body), "injected")) {
				t.Fatalf("query changed its existing failure response: %s", body)
			}
		})
	}
}

func TestStorageControlPlanesKeepsBestEffortIdentity(t *testing.T) {
	for _, operation := range []string{"read", "write"} {
		t.Run(operation, func(t *testing.T) {
			calls := 0
			f := newStorageDeleteFixture(t, func(d *Deps) {
				d.CallNode = func(_ context.Context, _ string, _ string, params any) (json.RawMessage, error) {
					p := params.(proto.StoragePeersParams)
					if p.Remove || p.ControlPlaneID == "" {
						t.Fatalf("legacy query lost its identity: %+v", p)
					}
					calls++
					return json.RawMessage(`{"peers":[]}`), nil
				}
			})
			if err := f.st.DeleteSetting(store.SettingControlPlane); err != nil {
				t.Fatal(err)
			}
			if operation == "read" {
				if err := f.st.DB().Callback().Query().Before("gorm:query").Register(t.Name(), func(tx *gorm.DB) {
					if tx.Statement.Table == "settings" {
						tx.AddError(errors.New("injected identity read failure"))
					}
				}); err != nil {
					t.Fatal(err)
				}
			} else {
				sql := fmt.Sprintf("CREATE TRIGGER reject_query_identity BEFORE INSERT ON settings WHEN NEW.key = '%s' BEGIN SELECT RAISE(ABORT, 'injected identity write failure'); END", store.SettingControlPlane)
				if err := f.st.DB().Exec(sql).Error; err != nil {
					t.Fatal(err)
				}
			}
			f.request(http.MethodGet, fmt.Sprintf("/api/v1/storages/%d/control-planes", f.target.ID), http.StatusOK)
			if calls != 1 {
				t.Fatalf("legacy query made %d RPC calls, want 1", calls)
			}
		})
	}
}

func TestStorageControlPlanesSkipsUnreadableNode(t *testing.T) {
	var called string
	f := newStorageDeleteFixture(t, func(d *Deps) {
		d.CallNode = func(_ context.Context, node string, _ string, _ any) (json.RawMessage, error) {
			called = node
			return json.RawMessage(`{"peers":[]}`), nil
		}
	})
	second, err := f.st.RegisterNode("second-writer", store.HashToken("second"), "second")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.ApplyStorageStatus(second.ID, proto.StorageStatusReport{Storages: f.statuses}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.DB().Callback().Query().After("gorm:query").Register(t.Name(), func(tx *gorm.DB) {
		if node, ok := tx.Statement.Dest.(*store.Node); ok && node.ID == f.node.ID {
			tx.AddError(errors.New("injected node lookup failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	f.request(http.MethodGet, fmt.Sprintf("/api/v1/storages/%d/control-planes", f.target.ID), http.StatusOK)
	if called != second.UUID {
		t.Fatalf("query used %q, want readable node %q", called, second.UUID)
	}
}

func TestStorageDeleteTriesAvailableWriters(t *testing.T) {
	for _, tc := range []struct {
		name       string
		firstReply string
		firstError error
		lastError  error
		status     int
		wantCalls  int
	}{
		{"first_disconnect", "", errors.New("disconnected"), nil, http.StatusNoContent, 2},
		{"first_bad_reply", `{"peers":`, nil, nil, http.StatusNoContent, 1},
		{"all_fail", "", errors.New("first disconnected"), errors.New("second disconnected"), http.StatusNoContent, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			var logs bytes.Buffer
			f := newStorageDeleteFixture(t, func(d *Deps) {
				d.Log = slog.New(slog.NewTextHandler(&logs, nil))
				d.CallNode = func(_ context.Context, node, method string, params any) (json.RawMessage, error) {
					calls = append(calls, node)
					if method != proto.RPCStoragePeers || !params.(proto.StoragePeersParams).Remove {
						t.Fatalf("unexpected physical-file operation: %s %+v", method, params)
					}
					if len(calls) == 1 {
						return json.RawMessage(tc.firstReply), tc.firstError
					}
					return json.RawMessage(`{"peers":[]}`), tc.lastError
				}
			})
			second, err := f.st.RegisterNode("second-writer", store.HashToken("second"), "second")
			if err != nil {
				t.Fatal(err)
			}
			if err := f.st.ApplyStorageStatus(second.ID, proto.StorageStatusReport{Storages: f.statuses}); err != nil {
				t.Fatal(err)
			}
			model := f.model("only", f.target)
			f.delete(tc.status)
			f.expectStorage(f.target, http.StatusNotFound)
			f.expectModel(model, false)
			if !slices.Equal(calls, []string{f.node.UUID, second.UUID}[:tc.wantCalls]) {
				t.Fatalf("attempted writers %v", calls)
			}
			if tc.name == "first_disconnect" && logs.Len() != 0 {
				t.Errorf("successful fallback reported cleanup failure: %s", logs.String())
			}
			if tc.lastError != nil {
				for _, text := range []string{tc.firstError.Error(), tc.lastError.Error(), f.node.UUID, second.UUID} {
					if !strings.Contains(logs.String(), text) {
						t.Errorf("all-writer failure lost %q: %s", text, logs.String())
					}
				}
			}
		})
	}
}

func TestStorageDeleteDatabaseFailureRollsBackAllRecords(t *testing.T) {
	for _, table := range []string{"profiles", "models", "storages"} {
		t.Run(table, func(t *testing.T) {
			f := newStorageDeleteFixture(t)
			first := f.model("first", f.target)
			second := f.model("second", f.target)
			unrelated := f.model("unrelated")
			modelIDs := []uint{first.ID, second.ID}
			profiles := []store.Profile{{ModelID: first.ID, Fingerprint: "first"}, {ModelID: second.ID, Fingerprint: "second"}}
			if err := f.st.DB().Create(&profiles).Error; err != nil {
				t.Fatal(err)
			}
			dep, err := f.st.CreateDeployment(&store.Deployment{Name: "download-only", ServedName: "download-only",
				ModelID: unrelated.ID, SaveToStorageID: f.target.ID})
			if err != nil {
				t.Fatal(err)
			}
			failedID := second.ID
			if table == "profiles" {
				failedID = profiles[1].ID
			} else if table == "storages" {
				failedID = f.target.ID
			}
			f.failDelete(table, failedID)
			sub := f.st.Bus.Subscribe(8, "storage", "model", "modelfile")
			defer sub.Close()
			f.delete(http.StatusInternalServerError)
			f.expectStorage(f.target, http.StatusOK)
			for _, model := range []*store.Model{first, second} {
				f.expectModel(model, true)
				f.expectFileStorages(model, f.target.ID)
			}
			var count int64
			if err := f.st.DB().Model(&store.Profile{}).Where("model_id IN ?", modelIDs).Count(&count).Error; err != nil || count != 2 {
				t.Fatalf("failed deletion changed profiles: count=%d, error=%v", count, err)
			}
			if got, err := f.st.GetDeployment(dep.ID); err != nil || got.SaveToStorageID != f.target.ID {
				t.Fatalf("failed deletion changed download destination: %+v, %v", got, err)
			}
			select {
			case event := <-sub.C():
				t.Fatalf("rolled back deletion published a change: %+v", event)
			default:
			}
			// A new user request can finish after the database fault is removed.
			if err := f.st.DB().Exec("DROP TRIGGER reject_delete").Error; err != nil {
				t.Fatal(err)
			}
			f.delete(http.StatusNoContent)
			f.expectStorage(f.target, http.StatusNotFound)
			f.expectModel(first, false)
			f.expectModel(second, false)
			f.expectModel(unrelated, true)
			if err := f.st.DB().Model(&store.Profile{}).Where("model_id IN ?", modelIDs).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("completed deletion retained profiles: count=%d, error=%v", count, err)
			}
			if got, err := f.st.GetDeployment(dep.ID); err != nil || got.SaveToStorageID != 0 {
				t.Fatalf("completed deletion retained download destination: %+v, %v", got, err)
			}
		})
	}
}

type storageDeleteTransactionCounter struct {
	gorm.ConnPool
	count int
}

func (p *storageDeleteTransactionCounter) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	p.count++
	return p.ConnPool.(gorm.TxBeginner).BeginTx(ctx, opts)
}

func TestStorageDeleteBatchesNodeRegistrations(t *testing.T) {
	for _, copies := range []int{1, 1000} {
		t.Run(fmt.Sprintf("nodes_%d", copies), func(t *testing.T) {
			f := newStorageDeleteFixture(t)
			if err := f.st.DB().Model(f.target).Update("type", proto.StorageLocal).Error; err != nil {
				t.Fatal(err)
			}
			nodeIDs := []uint{f.node.ID}
			var nodes []store.Node
			for i := 1; i < copies; i++ {
				nodes = append(nodes, store.Node{UUID: fmt.Sprintf("fleet-%d", i), Hostname: fmt.Sprintf("node-%d", i)})
			}
			if len(nodes) > 0 {
				if err := f.st.DB().Select("uuid", "hostname").CreateInBatches(&nodes, 100).Error; err != nil {
					t.Fatal(err)
				}
			}
			for _, node := range nodes {
				nodeIDs = append(nodeIDs, node.ID)
			}
			const modelCount = 20
			var modelIDs []uint
			var files []store.ModelFile
			for i := 0; i < modelCount; i++ {
				name := fmt.Sprintf("model-%d", i)
				model, err := f.st.UpsertModel(&store.Model{Name: name, Source: "local", SourceID: name})
				if err != nil {
					t.Fatal(err)
				}
				modelIDs = append(modelIDs, model.ID)
				for _, nodeID := range nodeIDs {
					files = append(files, store.ModelFile{ModelID: model.ID, StorageID: f.target.ID,
						NodeID: nodeID, Path: "/models/" + name, State: proto.MFPresent})
				}
			}
			if err := f.st.DB().CreateInBatches(files, 100).Error; err != nil {
				t.Fatal(err)
			}
			pool := &storageDeleteTransactionCounter{ConnPool: f.st.DB().Statement.ConnPool}
			f.st.DB().Statement.ConnPool = pool
			sub := f.st.Bus.Subscribe(modelCount+8, "storage", "model", "modelfile")
			defer sub.Close()
			f.delete(http.StatusNoContent)
			for _, entity := range []any{&store.Model{}, &store.ModelFile{}} {
				key := "model_id"
				if _, ok := entity.(*store.Model); ok {
					key = "id"
				}
				var remaining int64
				if err := f.st.DB().Model(entity).Where(key+" IN ?", modelIDs).Count(&remaining).Error; err != nil || remaining != 0 {
					t.Fatalf("remaining %T: %d, %v", entity, remaining, err)
				}
			}
			// One session renewal and one transaction for the entire deletion.
			wantTransactions := 2
			if pool.count != wantTransactions {
				t.Fatalf("%d registrations caused %d transactions, want %d", len(files), pool.count, wantTransactions)
			}
			counts := map[string]int{}
			for {
				select {
				case event := <-sub.C():
					counts[event.Topic]++
				default:
					if counts["storage"] != 1 || counts["model"] != modelCount || counts["modelfile"] != 0 {
						t.Fatalf("unexpected change notifications: %v", counts)
					}
					t.Logf("nodes=%d registrations=%d transactions=%d events=%v", copies, len(files), pool.count, counts)
					return
				}
			}
		})
	}
}
