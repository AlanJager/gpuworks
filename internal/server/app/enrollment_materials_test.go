//go:build linux && dqlite

package app

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestEnrollmentMaterialsRequireRealTLSAndCommittedAdmission(t *testing.T) {
	now := time.Now()
	clusterID := strings.Repeat("a", 32)
	ca, caKey, err := identity.NewAuthority(clusterID, now)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := identity.CertificateFingerprint(ca)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := identity.LoadAuthority(ca, caKey, clusterID, pin, now)
	if err != nil {
		t.Fatal(err)
	}
	trust, err := identity.NewTrust(ca, clusterID, pin, now)
	if err != nil {
		t.Fatal(err)
	}
	certificate := func(host string) (tls.Certificate, string, []byte) {
		t.Helper()
		key, err := identity.NewMemberKey()
		if err != nil {
			t.Fatal(err)
		}
		csr, err := identity.NewMemberCSR(key)
		if err != nil {
			t.Fatal(err)
		}
		fp, err := identity.CSRKeyFingerprint(csr)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := authority.IssueMember(csr, fp, host, now, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		pair, err := tls.X509KeyPair(append(cert, ca...), key)
		if err != nil {
			t.Fatal(err)
		}
		return pair, fp, key
	}
	serverPair, serverFP, _ := certificate("127.0.0.1:9910")
	clientPair, fp, memberKey := certificate("127.0.0.1:9911")
	unknownPair, _, _ := certificate("127.0.0.1:9912")
	config, err := enrollmentClientTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverPair}}, ca)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "materials.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_, err = st.InitializeClusterIdentity(t.Context(), store.ClusterIdentity{ClusterID: clusterID, CAFingerprint: pin, Protocol: store.ClusterProtocolVersion})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.InitializeClusterSeedAdmission(t.Context(), clusterID, serverFP, "127.0.0.1:9910"); err != nil {
		t.Fatal(err)
	}
	admin := store.User{Username: "admin", Role: "admin"}
	if err := st.DB().Create(&admin).Error; err != nil {
		t.Fatal(err)
	}
	_, token, err := st.IssueClusterInvitation(t.Context(), clusterID, admin.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimClusterInvitation(t.Context(), clusterID, token, fp, "127.0.0.1:9911"); err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewTokenSigner("test-master-material", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bundle := &enrollmentBundle{info: EnrollmentInfo{ClusterID: clusterID, CAFingerprint: pin, Protocol: store.ClusterProtocolVersion, ListenAddress: "127.0.0.1:0", PublicURL: "https://127.0.0.1:9910"}, issuer: authority, tls: config, master: "test-master-material\xff\x80\x00", trust: trust, ca: ca, caKey: caKey}
	public, err := api.ClusterEnrollmentRouter(t.Context(), api.ClusterEnrollmentDeps{Store: st, Signer: signer, Issuer: authority, PublicURL: bundle.info.PublicURL})
	if err != nil {
		t.Fatal(err)
	}
	catalogRoot := filepath.Join(t.TempDir(), "catalog")
	catalog, err := packages.New(catalogRoot)
	if err != nil {
		t.Fatal(err)
	}
	var routes []string
	if err := chi.Walk(enrollmentMaterialHandler(bundle, st, public, catalog).(chi.Routes), func(method, path string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routes = append(routes, method+" "+path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(routes)
	wantRoutes := []string{"POST /_cluster/bootstrap-secrets", "POST /_cluster/catalog-packages", "POST /_cluster/catalog-packages/{digest}", "POST /_cluster/enrollment-origin", "POST /_cluster/members", "POST /_cluster/transport-authorization", "POST /_cluster/transport-policy", "POST /api/v1/cluster/enrollment/certificate", "POST /api/v1/cluster/enrollment/certificate/recover", "POST /api/v1/cluster/invitations"}
	if !slices.Equal(routes, wantRoutes) {
		t.Fatalf("enrollment route inventory changed: %v", routes)
	}
	materialReceiver, err := newMaterialReceiver(t.TempDir(), 7)
	if err != nil {
		t.Fatal(err)
	}
	var materialRoutes []string
	if err := chi.Walk(enrollmentMaterialHandlerWithReceiver(bundle, st, public, catalog, materialReceiver).(chi.Routes), func(method, path string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		materialRoutes = append(materialRoutes, method+" "+path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(materialRoutes)
	wantMaterialRoutes := append(slices.Clone(wantRoutes), "GET "+materialReplicaPath+"/{digest}", "HEAD "+materialReplicaPath+"/{digest}", "POST "+materialReplicaPath+"/{digest}", "POST "+materialBatchPath, "POST "+materialManifestPath+"/{digest}/verify", "POST "+materialManifestPath+"/{digest}/materialize", "POST "+materialManifestPath+"/{digest}/installed")
	sort.Strings(wantMaterialRoutes)
	if !slices.Equal(materialRoutes, wantMaterialRoutes) {
		t.Fatalf("material receiver route inventory changed: %v", materialRoutes)
	}
	bundle.dataDir = t.TempDir()
	st.NoteClusterSessionRoute(store.ClusterSessionRoute{ID: 7, Key: []byte("material-member-route"), Listen: "127.0.0.1:0"})
	server, err := prepareEnrollmentServer(bundle, st, signer, slog.New(slog.NewTextHandler(io.Discard, nil)), catalog)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(bundle.dataDir, "material-replicas")); err != nil || !info.IsDir() {
		t.Fatalf("production enrollment did not attach a durable material receiver: %v", err)
	}
	if err := server.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Close(ctx)
	})
	client := func(pair *tls.Certificate) *http.Client {
		t.Helper()
		cfg, err := identity.EnrollmentTLSConfig(clusterID, pin, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		if pair != nil {
			cfg.Certificates = []tls.Certificate{*pair}
		}
		transport := &http.Transport{TLSClientConfig: cfg}
		t.Cleanup(transport.CloseIdleConnections)
		return &http.Client{Transport: transport, Timeout: 5 * time.Second}
	}
	anonymous, admitted, unknown := client(nil), client(&clientPair), client(&unknownPair)
	probe, err := anonymous.Get("https://" + server.Addr() + materialReplicaPath + "/" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	_ = probe.Body.Close()
	if probe.StatusCode != http.StatusUnauthorized {
		t.Fatalf("production material route status=%d want=%d", probe.StatusCode, http.StatusUnauthorized)
	}
	request := func(c *http.Client, method, path, body string, want int) []byte {
		t.Helper()
		req, err := http.NewRequest(method, "https://"+server.Addr()+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Client-Cert", "claimed-member-certificate")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want {
			t.Fatalf("status=%d want=%d", resp.StatusCode, want)
		}
		if path == enrollment.SecretsPath && resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("missing no-store")
		}
		if want != http.StatusOK && (strings.Contains(string(data), bundle.master) || strings.Contains(string(data), "PRIVATE KEY")) {
			t.Fatal("secret in failure response")
		}
		return data
	}
	// Public bootstrap remains accessible without a member certificate.
	request(anonymous, http.MethodPost, "/api/v1/cluster/enrollment/certificate", "{}", http.StatusConflict)
	request(anonymous, http.MethodPost, "/_cluster/unknown", "", http.StatusNotFound)
	request(anonymous, http.MethodPost, enrollment.SecretsPath, "", http.StatusUnauthorized)
	request(unknown, http.MethodPost, enrollment.SecretsPath, "", http.StatusForbidden)
	request(admitted, http.MethodGet, enrollment.SecretsPath, "", http.StatusMethodNotAllowed)
	request(admitted, http.MethodPost, enrollment.SecretsPath+"?token=x", "", http.StatusBadRequest)
	request(admitted, http.MethodPost, enrollment.SecretsPath, "{}", http.StatusBadRequest)
	data := request(admitted, http.MethodPost, enrollment.SecretsPath, "", http.StatusOK)
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(data, &wire); err != nil || len(wire) != 5 {
		t.Fatal("invalid secrets wire contract")
	}
	var secrets enrollment.BootstrapSecrets
	if err := json.Unmarshal(data, &secrets); err != nil {
		t.Fatal(err)
	}
	if secrets.ClusterID != clusterID || secrets.Protocol != store.ClusterProtocolVersion || secrets.CACertificatePEM != string(ca) || secrets.CAKeyPEM != string(caKey) || string(secrets.MasterSecret) != bundle.master {
		t.Fatal("wrong material snapshot")
	}
	row, err := st.ClusterAdmissionForVerifiedKey(t.Context(), clusterID, fp)
	if err != nil || row.MaterialGrantSequence != 1 {
		t.Fatal("material response did not commit a fresh grant")
	}
	joining, err := enrollment.NewClient("https://"+server.Addr()+"/api/v1/cluster/enrollment/certificate", clusterID, pin, store.ClusterProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	defer joining.Close()
	memberCertificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientPair.Certificate[0]})
	received, err := joining.FetchSecrets(t.Context(), memberCertificate, memberKey)
	if err != nil || !reflect.DeepEqual(received, secrets) {
		t.Fatal("authenticated client did not receive validated materials", err)
	}
	checkEnrollmentCatalogTransfer(t, st, catalog, catalogRoot, joining, memberCertificate, memberKey, request, anonymous, admitted, unknown)
	checkEnrollmentTransportPolicy(t, st, bundle, joining, memberCertificate, memberKey, fp, request, anonymous, admitted, unknown)
	// A durable SQL authorization snapshot alone cannot confirm a live transport.
	request(anonymous, http.MethodPost, enrollment.TransportAuthorizationPath, "", http.StatusUnauthorized)
	request(unknown, http.MethodPost, enrollment.TransportAuthorizationPath, "", http.StatusForbidden)
	request(admitted, http.MethodGet, enrollment.TransportAuthorizationPath, "", http.StatusMethodNotAllowed)
	request(admitted, http.MethodPost, enrollment.TransportAuthorizationPath+"?version=1", "", http.StatusBadRequest)
	request(admitted, http.MethodPost, enrollment.TransportAuthorizationPath, "{}", http.StatusBadRequest)
	request(admitted, http.MethodPost, enrollment.TransportAuthorizationPath, "", http.StatusServiceUnavailable)
	request(anonymous, http.MethodPost, enrollment.ClusterMembershipPath, "", http.StatusUnauthorized)
	request(unknown, http.MethodPost, enrollment.ClusterMembershipPath, "", http.StatusForbidden)
	request(admitted, http.MethodGet, enrollment.ClusterMembershipPath, "", http.StatusMethodNotAllowed)
	request(admitted, http.MethodPost, enrollment.ClusterMembershipPath+"?members=all", "", http.StatusBadRequest)
	request(admitted, http.MethodPost, enrollment.ClusterMembershipPath, "{}", http.StatusBadRequest)
	// An admissions-only SQL fixture must not invent live Raft membership.
	request(admitted, http.MethodPost, enrollment.ClusterMembershipPath, "", http.StatusServiceUnavailable)
	request(anonymous, http.MethodPost, enrollment.ClusterEnrollmentOriginPath, `{"origin":"https://127.0.0.1:9443"}`, http.StatusUnauthorized)
	request(unknown, http.MethodPost, enrollment.ClusterEnrollmentOriginPath, `{"origin":"https://127.0.0.1:9443"}`, http.StatusForbidden)
	request(admitted, http.MethodGet, enrollment.ClusterEnrollmentOriginPath, "", http.StatusMethodNotAllowed)
	request(admitted, http.MethodPost, enrollment.ClusterEnrollmentOriginPath+"?x=1", `{"origin":"https://127.0.0.1:9443"}`, http.StatusBadRequest)
	request(admitted, http.MethodPost, enrollment.ClusterEnrollmentOriginPath, `{"origin":"https://127.0.0.1:9443","key_fingerprint":"claimed"}`, http.StatusBadRequest)
	request(admitted, http.MethodPost, enrollment.ClusterEnrollmentOriginPath, `{"origin":"https://other.example:9443"}`, http.StatusForbidden)
	request(admitted, http.MethodPost, enrollment.ClusterEnrollmentOriginPath, `{"origin":"http://127.0.0.1:9443"}`, http.StatusBadRequest)
	var originRevision int64
	if err := st.DB().Raw("SELECT revision FROM cluster_authorizations WHERE id = 1").Scan(&originRevision).Error; err != nil {
		t.Fatal(err)
	}
	request(admitted, http.MethodPost, enrollment.ClusterEnrollmentOriginPath, `{"origin":"https://127.0.0.1:9443"}`, http.StatusNoContent)
	request(admitted, http.MethodPost, enrollment.ClusterEnrollmentOriginPath, `{"origin":"https://127.0.0.1:9443"}`, http.StatusNoContent)
	var registeredOriginRevision int64
	if err := st.DB().Raw("SELECT revision FROM cluster_authorizations WHERE id = 1").Scan(&registeredOriginRevision).Error; err != nil || registeredOriginRevision != originRevision+1 {
		t.Fatalf("origin registration revision before=%d after=%d err=%v", originRevision, registeredOriginRevision, err)
	}
	if err := joining.RegisterClusterEnrollmentOrigin(t.Context(), memberCertificate, memberKey, "https://127.0.0.1:9443", serverFP); err != nil {
		t.Fatalf("authenticated registration client failed: %v", err)
	}
	if err := joining.RegisterClusterEnrollmentOrigin(t.Context(), memberCertificate, memberKey, "https://127.0.0.1:9443", fp); err == nil {
		t.Fatal("registration accepted the wrong enrollment server key")
	}
	registered, err := st.ClusterAdmissionForVerifiedKey(t.Context(), clusterID, fp)
	if err != nil || registered.EnrollmentOrigin != "https://127.0.0.1:9443" {
		t.Fatalf("origin not bound to authenticated member: %+v %v", registered, err)
	}
	eligibility := newMaterialEligibility(time.Now, func(context.Context) (bool, error) { return true, nil }, time.Hour)
	closed := &App{ha: &memberRuntime{}, memberState: memberState{joinedClosed: true, materialEligibility: eligibility}}
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready {
		t.Fatalf("initial material proof: ready=%v err=%v", ready, err)
	}
	originRouter := chi.NewRouter()
	registerEnrollmentOriginWithChange(originRouter, bundle, st, func() { closed.invalidateCandidateMaterialEligibility(closeOriginRegistered) })
	originServer := httptest.NewUnstartedServer(originRouter)
	originServer.TLS = config
	originServer.StartTLS()
	t.Cleanup(originServer.Close)
	writeOrigin := func(c *http.Client, body string, want int) {
		t.Helper()
		response, err := c.Post(originServer.URL+enrollment.ClusterEnrollmentOriginPath, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("observed enrollment origin status=%d want=%d", response.StatusCode, want)
		}
	}
	writeOrigin(anonymous, `{"origin":"https://127.0.0.1:9444"}`, http.StatusUnauthorized)
	writeOrigin(admitted, `{"origin":"https://127.0.0.1:9443"}`, http.StatusNoContent)
	if !eligibility.Eligible() {
		t.Fatal("rejected or same-origin registration closed eligibility")
	}
	writeOrigin(admitted, `{"origin":"https://127.0.0.1:9444"}`, http.StatusNoContent)
	if eligibility.Eligible() {
		t.Fatal("committed enrollment origin change retained old proof")
	}
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready {
		t.Fatalf("fresh material proof: ready=%v err=%v", ready, err)
	}
	writeOrigin(admitted, `{"origin":"https://127.0.0.1:9444"}`, http.StatusNoContent)
	if !eligibility.Eligible() {
		t.Fatal("idempotent enrollment origin retry closed eligibility")
	}
	// A valid key bound to another address cannot receive materials.
	if err := st.DB().Model(&store.ClusterAdmission{}).Where("key_fingerprint = ?", fp).Update("address", "192.0.2.7:9911").Error; err != nil {
		t.Fatal(err)
	}
	request(admitted, http.MethodPost, enrollment.SecretsPath, "", http.StatusForbidden)
	request(admitted, http.MethodPost, enrollment.CatalogPackagesPath, "", http.StatusForbidden)
	request(admitted, http.MethodPost, enrollment.TransportPolicyPath, "", http.StatusForbidden)
	request(admitted, http.MethodPost, enrollment.ClusterMembershipPath, "", http.StatusForbidden)
	request(admitted, http.MethodPost, enrollment.TransportAuthorizationPath, "", http.StatusForbidden)
	if err := st.DB().Model(&store.ClusterAdmission{}).Where("key_fingerprint = ?", fp).Update("address", "127.0.0.1:9911").Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Exec(`CREATE TRIGGER reject_grant BEFORE UPDATE OF material_grant_sequence ON cluster_admissions BEGIN SELECT RAISE(ABORT, 'private failure text'); END`).Error; err != nil {
		t.Fatal(err)
	}
	request(admitted, http.MethodPost, enrollment.SecretsPath, "", http.StatusServiceUnavailable)
	request(admitted, http.MethodPost, enrollment.CatalogPackagesPath, "", http.StatusServiceUnavailable)
	request(admitted, http.MethodPost, enrollment.TransportPolicyPath, "", http.StatusServiceUnavailable)
	request(admitted, http.MethodPost, enrollment.ClusterMembershipPath, "", http.StatusServiceUnavailable)
	request(admitted, http.MethodPost, enrollment.TransportAuthorizationPath, "", http.StatusServiceUnavailable)
	if err := st.DB().Exec("DROP TRIGGER reject_grant").Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&store.ClusterAdmission{}).Where("key_fingerprint = ?", fp).Update("revoked_at", time.Now()).Error; err != nil {
		t.Fatal(err)
	}
	request(admitted, http.MethodPost, enrollment.SecretsPath, "", http.StatusForbidden)
	request(admitted, http.MethodPost, enrollment.CatalogPackagesPath, "", http.StatusForbidden)
	request(admitted, http.MethodPost, enrollment.TransportPolicyPath, "", http.StatusForbidden)
	request(admitted, http.MethodPost, enrollment.ClusterMembershipPath, "", http.StatusForbidden)
	request(admitted, http.MethodPost, enrollment.TransportAuthorizationPath, "", http.StatusForbidden)
}
