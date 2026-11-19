package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/provider"
	cloudprovider "gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud"
	"gpuworks.ai/gpuworks/internal/server/store"
	cloudstore "gpuworks.ai/gpuworks/internal/server/store/zstack/cloud"
)

func TestDeleteProviderRemovesCachedCloudHostsThroughHTTP(t *testing.T) {
	for _, scenario := range []string{"success", "snapshot-delete-fails", "linked-node", "unreleased-lease"} {
		t.Run(scenario, func(t *testing.T) {
			st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			box, err := auth.NewSecretbox("snapshot-delete-fixture")
			if err != nil {
				t.Fatal(err)
			}
			svc := provider.NewService(st, box)
			svc.SetDriver(cloudprovider.New().NewDriver(st, svc, nil))
			account := func(name string) *store.Provider {
				t.Helper()
				p, err := st.CreateProvider(&store.Provider{Name: name, Kind: "zstack", CredsCipher: "unneeded-for-local-deletion"})
				if err != nil {
					t.Fatal(err)
				}
				if err := st.UpsertProviderInstance(&store.ProviderInstance{ProviderID: p.ID, ExternalID: "host-1"}); err != nil {
					t.Fatal(err)
				}
				if err := st.CloudLedger().SaveCloudHostSnapshot(cloudstore.CloudHost{ProviderID: p.ID, ExternalID: "host-1", Snapshot: `{"name":"cached"}`}); err != nil {
					t.Fatal(err)
				}
				return p
			}
			p, other := account("remove"), account("keep")
			hosts := func(id uint) []cloudstore.CloudHost {
				t.Helper()
				rows, err := st.CloudLedger().CloudHosts(id)
				if err != nil {
					t.Fatal(err)
				}
				return rows
			}
			snapshot := hosts(p.ID)[0]
			if scenario == "linked-node" {
				node, _, err := st.CreateNodePending("linked-cloud-node")
				if err != nil {
					t.Fatal(err)
				}
				snapshot.NodeUUID = node.UUID
				if err := st.CloudLedger().SaveCloudHostSnapshot(snapshot); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "unreleased-lease" {
				if _, err := st.CloudLedger().CreateCloudResourceLease(&cloudstore.CloudResourceLease{OperationID: "active-claim", ProviderID: p.ID, CloudHostID: snapshot.ID, ResourceType: "cpu", ResourceID: "host-1", Amount: 1, Unit: "core", State: cloudstore.CloudLeaseReserved}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "snapshot-delete-fails" {
				if err := st.DB().Callback().Delete().Before("gorm:delete").Register("test:fail_snapshot_cleanup", func(db *gorm.DB) {
					if db.Statement.Table == "cloud_hosts" {
						_ = db.AddError(errors.New("injected cloud snapshot deletion failure"))
					}
				}); err != nil {
					t.Fatal(err)
				}
			}
			signer, err := auth.NewTokenSigner("snapshot-delete-session", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
			if err != nil {
				t.Fatal(err)
			}
			router := Router(Deps{Store: st, Providers: svc, AuthSigner: signer})
			remove := func(bearer string) *httptest.ResponseRecorder {
				t.Helper()
				r := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/v1/providers/%d", p.ID), nil)
				if bearer != "" {
					r.Header.Set("Authorization", "Bearer "+bearer)
				}
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, r)
				return rec
			}
			if rec := remove(""); rec.Code != http.StatusUnauthorized || len(hosts(p.ID)) != 1 {
				t.Fatalf("unauthorized deletion: %d %s", rec.Code, rec.Body)
			}
			rec := remove(token)
			want := http.StatusOK
			switch scenario {
			case "snapshot-delete-fails":
				want = http.StatusInternalServerError
			case "linked-node", "unreleased-lease":
				want = http.StatusConflict
			}
			if rec.Code != want {
				t.Fatalf("delete: %d %s, want%d", rec.Code, rec.Body, want)
			}
			if got := hosts(other.ID); len(got) != 1 || got[0].ExternalID != "host-1" || got[0].Snapshot != snapshot.Snapshot {
				t.Fatalf("another account's snapshot changed: %+v", got)
			}
			instances, err := st.ProviderInstances(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			_, lookupErr := st.GetProvider(p.ID)
			if scenario == "success" {
				if strings.TrimSpace(rec.Body.String()) != `{"status":"deleted"}` || !errors.Is(lookupErr, store.ErrNotFound) || len(instances) != 0 || len(hosts(p.ID)) != 0 {
					t.Fatalf("deleted account leaked cached rows: providerErr=%v instances=%d snapshots=%d body=%s", lookupErr, len(instances), len(hosts(p.ID)), rec.Body)
				}
				if retry := remove(token); retry.Code != http.StatusOK || len(hosts(p.ID)) != 0 {
					t.Fatalf("idempotent retry: %d %s", retry.Code, retry.Body)
				}
			} else if lookupErr != nil || len(instances) != 1 || len(hosts(p.ID)) != 1 {
				t.Fatalf("failed/refused deletion changed account: providerErr=%v instances=%d snapshots=%d", lookupErr, len(instances), len(hosts(p.ID)))
			}
		})
	}
}
