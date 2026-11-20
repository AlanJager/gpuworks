//go:build linux && dqlite

package app

import (
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func checkEnrollmentTransportPolicy(t *testing.T, st *store.Store, bundle *enrollmentBundle, client *enrollment.Client, cert, key []byte, fingerprint string, request func(*http.Client, string, string, string, int) []byte, anonymous, admitted, unknown *http.Client) {
	t.Helper()
	path := enrollment.TransportPolicyPath
	request(anonymous, http.MethodPost, path, "", http.StatusUnauthorized)
	request(unknown, http.MethodPost, path, "", http.StatusForbidden)
	request(admitted, http.MethodGet, path, "", http.StatusMethodNotAllowed)
	request(admitted, http.MethodPost, path+"?token=x", "", http.StatusBadRequest)
	request(admitted, http.MethodPost, path, "{}", http.StatusBadRequest)
	data := request(admitted, http.MethodPost, path, "", http.StatusOK)
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(data, &wire); err != nil || len(wire) != 6 {
		t.Fatalf("snapshot wire keys: %s %v", data, err)
	}
	expectedFields := []string{"allowed_keys", "ca_fingerprint", "cluster_id", "protocol", "revoked_keys", "source_revision"}
	fields := make([]string, 0, len(wire))
	for name := range wire {
		fields = append(fields, name)
	}
	sort.Strings(fields)
	if !reflect.DeepEqual(fields, expectedFields) {
		t.Fatalf("snapshot keys: %v", fields)
	}
	var revisionText string
	if err := json.Unmarshal(wire["source_revision"], &revisionText); err != nil {
		t.Fatal("source revision is not a decimal wire string", err)
	}
	revision, err := strconv.ParseUint(revisionText, 10, 64)
	if err != nil || revision < 2 {
		t.Fatalf("revision %q %v", revisionText, err)
	}
	var expected enrollment.TransportPolicySnapshot
	if err := json.Unmarshal(data, &expected); err != nil {
		t.Fatal(err)
	}
	if expected.ClusterID != bundle.info.ClusterID || expected.CAFingerprint != bundle.info.CAFingerprint || expected.Protocol != bundle.info.Protocol || len(expected.AllowedKeys) != 2 || len(expected.RevokedKeys) != 0 {
		t.Fatalf("wrong snapshot: %+v", expected)
	}
	received, err := client.FetchTransportPolicy(t.Context(), cert, key)
	if err != nil || !reflect.DeepEqual(received, expected) {
		t.Fatalf("pinned member snapshot %+v %v", received, err)
	}
	// Grants change the caller's write barrier, never the policy's source version.
	repeated, err := client.FetchTransportPolicy(t.Context(), cert, key)
	if err != nil || !reflect.DeepEqual(received, repeated) {
		t.Fatalf("read-only policy content changed across grants: %+v %v", repeated, err)
	}
	var admin store.User
	if err := st.DB().Where("username = ?", "admin").First(&admin).Error; err != nil {
		t.Fatal(err)
	}
	retiredKey := strings.Repeat("e", 64)
	_, token, err := st.IssueClusterInvitation(t.Context(), bundle.info.ClusterID, admin.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimClusterInvitation(t.Context(), bundle.info.ClusterID, token, retiredKey, "retired.example:9444"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RevokeClusterAdmission(t.Context(), bundle.info.ClusterID, retiredKey); err != nil {
		t.Fatal(err)
	}
	next, err := client.FetchTransportPolicy(t.Context(), cert, key)
	if err != nil || next.SourceRevision != received.SourceRevision+2 || !reflect.DeepEqual(next.AllowedKeys, received.AllowedKeys) || !reflect.DeepEqual(next.RevokedKeys, []string{retiredKey}) {
		t.Fatalf("versioned tombstone snapshot %+v %v", next, err)
	}
	// The first shared material grant can become stale before the snapshot grant.
	// Emulate a concurrent committed revoke at precisely that boundary.
	if err := st.DB().Exec(`CREATE TRIGGER revoke_during_material_grant AFTER UPDATE OF material_grant_sequence ON cluster_admissions
 WHEN NEW.key_fingerprint = '` + fingerprint + `' AND NEW.revoked_at IS NULL
 BEGIN UPDATE cluster_admissions SET revoked_at = '2026-01-01T00:00:00Z' WHERE key_fingerprint = NEW.key_fingerprint;
 UPDATE cluster_authorizations SET revision = revision + 1 WHERE id = 1; END`).Error; err != nil {
		t.Fatal(err)
	}
	request(admitted, http.MethodPost, path, "", http.StatusForbidden)
	if err := st.DB().Exec("DROP TRIGGER revoke_during_material_grant").Error; err != nil {
		t.Fatal(err)
	}
	// Restore only this fault-injection fixture for the surrounding rejection cases.
	if err := st.DB().Model(&store.ClusterAdmission{}).Where("key_fingerprint = ?", fingerprint).Update("revoked_at", nil).Error; err != nil {
		t.Fatal(err)
	}
}
