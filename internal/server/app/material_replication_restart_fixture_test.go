//go:build linux && dqlite

package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type restartMaterialFixture struct {
	root       string
	st         *store.Store
	bundle     *enrollmentBundle
	expected   store.ClusterIdentity
	members    []store.ClusterTransportMember
	pointer    store.MaterialManifestRecord
	receivers  []*materialReceiver
	requests   atomic.Int32
	transfers  atomic.Int32
	grantCalls int
	afterHTTP  func()
	fault      string
}

// Offline observations derive admissions from real confirmed SQLite grants;
// only the unavailable native configuration accessor is represented by the
// fixture's literal topology. Actual native tests separately own quorum proof.
func (f *restartMaterialFixture) grant(ctx context.Context) (uint64, []store.ClusterTransportMember, error) {
	f.grantCalls++
	revision, admissions, err := f.st.GrantClusterTransportAdmissions(ctx, f.expected, f.members[0].KeyFingerprint)
	if err != nil {
		return 0, nil, err
	}
	var result []store.ClusterTransportMember
	for _, member := range f.members {
		for _, admission := range admissions {
			if admission.KeyFingerprint == member.KeyFingerprint && admission.RevokedAt == nil && admission.Address == member.Address {
				member.EnrollmentOrigin = admission.EnrollmentOrigin
				result = append(result, member)
			}
		}
	}
	return revision, result, nil
}

func (f *restartMaterialFixture) check(ctx context.Context) (store.MaterialManifestRecord, error) {
	return checkMaterialReplicationRestartWithGrant(ctx, f.root, f.st, f.bundle, f.members[0].ID, f.members[0].KeyFingerprint, f.expected, slices.Clone(f.members), f.grant)
}

func newRestartMaterialFixture(t *testing.T, fault string) *restartMaterialFixture {
	t.Helper()
	f := &restartMaterialFixture{root: t.TempDir(), fault: fault}
	var err error
	f.st, err = store.Open(filepath.Join(f.root, "gpuworks.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.st.Close() })
	now := time.Now().UTC()
	clusterID := strings.Repeat("a", 32)
	ca, caKey, err := identity.NewAuthority(clusterID, now)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := identity.CertificateFingerprint(ca)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := identity.LoadAuthority(ca, caKey, clusterID, pin, now)
	if err != nil {
		t.Fatal(err)
	}
	trust, err := identity.NewTrust(ca, clusterID, pin, now)
	if err != nil {
		t.Fatal(err)
	}
	f.expected = store.ClusterIdentity{ClusterID: clusterID, CAFingerprint: pin, Protocol: store.ClusterProtocolVersion}
	first, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	port := first.Addr().(*net.TCPAddr).Port
	second, err := net.Listen("tcp", net.JoinHostPort("127.0.0.3", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	listeners := []net.Listener{first, second}
	var pairs []tls.Certificate
	var identityBytes []byte
	for i, host := range []string{"127.0.0.1", "127.0.0.2", "127.0.0.3"} {
		key, err := identity.NewMemberKey()
		if err != nil {
			t.Fatal(err)
		}
		csr, err := identity.NewMemberCSR(key)
		if err != nil {
			t.Fatal(err)
		}
		fingerprint, err := identity.CSRKeyFingerprint(csr)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := issuer.IssueMember(csr, fingerprint, net.JoinHostPort(host, strconv.Itoa(port)), now, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		pair, err := tls.X509KeyPair(append(cert, ca...), key)
		if err != nil {
			t.Fatal(err)
		}
		pair.Leaf, err = x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		pairs = append(pairs, pair)
		f.members = append(f.members, store.ClusterTransportMember{ID: uint64(11 + i), Address: net.JoinHostPort(host, "9001"), Role: "voter", KeyFingerprint: fingerprint, EnrollmentOrigin: "https://" + net.JoinHostPort(host, strconv.Itoa(port))})
		if i == 0 {
			identityBytes = cert
		}
	}
	config, err := enrollmentClientTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pairs[0]}}, ca)
	if err != nil {
		t.Fatal(err)
	}
	f.bundle = &enrollmentBundle{info: EnrollmentInfo{ClusterID: clusterID, CAFingerprint: pin, Protocol: store.ClusterProtocolVersion, PublicURL: f.members[0].EnrollmentOrigin}, tls: config, trust: trust}
	if _, err := f.st.InitializeClusterIdentity(t.Context(), f.expected); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.InitializeClusterSeedAdmission(t.Context(), clusterID, f.members[0].KeyFingerprint, f.members[0].Address); err != nil {
		t.Fatal(err)
	}
	admin := store.User{Username: "restart-fixture-admin", Role: "admin"}
	if err := f.st.DB().Create(&admin).Error; err != nil {
		t.Fatal(err)
	}
	for _, member := range f.members[1:] {
		_, token, err := f.st.IssueClusterInvitation(t.Context(), clusterID, admin.ID, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.st.ClaimClusterInvitation(t.Context(), clusterID, token, member.KeyFingerprint, member.Address); err != nil {
			t.Fatal(err)
		}
	}
	for _, member := range f.members {
		if err := f.st.SetClusterEnrollmentOriginForVerifiedKey(t.Context(), f.expected, member.KeyFingerprint, member.EnrollmentOrigin); err != nil {
			t.Fatal(err)
		}
	}
	bodies := map[string][]byte{"master.key": []byte("restart master secret"), "dl/tiny-agent": []byte("restart agent bytes"), enrollmentDirectory + "/server.pem": identityBytes}
	write := func(path string, body []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range bodies {
		write(filepath.Join(f.root, name), body)
	}
	inventory, err := InspectRequiredMaterials(t.Context(), f.root, f.st)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := persistLocalMaterialInventory(t.Context(), f.root, f.st, 11, inventory)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := buildMaterialManifest(f.root, inventory, paths, f.expected, 11, f.members[0].KeyFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	digest := materialTestDigest(raw)
	manifest, err := parseMaterialManifest(raw, f.expected, 11, f.members[0].KeyFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := persistLocalMaterialBytes(t.Context(), f.root, f.st, 11, digest, raw); err != nil {
		t.Fatal(err)
	}
	// Fixture publication establishes initial durable data; production/native
	// tests own PublishMaterialManifest's actual live-membership grant chain.
	f.pointer = store.MaterialManifestRecord{SourceMemberID: 11, ClusterID: clusterID, SourceKey: f.members[0].KeyFingerprint, Digest: digest, Bytes: int64(len(raw)), RecordedAt: now}
	if err := f.st.DB().Create(&f.pointer).Error; err != nil {
		t.Fatal(err)
	}
	for i, listener := range listeners {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "material-replicas"), 0700); err != nil {
			t.Fatal(err)
		}
		receiver, err := newMaterialReceiver(filepath.Join(root, "material-replicas"), f.members[i+1].ID)
		if err != nil {
			t.Fatal(err)
		}
		f.receivers = append(f.receivers, receiver)
		replicas := map[string]string{}
		for _, file := range manifest.Files {
			body, err := os.ReadFile(paths[file.SHA256])
			if err != nil {
				t.Fatal(err)
			}
			canonical := filepath.Join(receiver.root, file.SHA256[:2], file.SHA256)
			write(canonical, body)
			replicas[file.SHA256] = canonical
			if file.Class != MaterialMemberIdentity {
				write(filepath.Join(root, file.Path), body)
			}
		}
		canonical := filepath.Join(receiver.root, digest[:2], digest)
		write(canonical, raw)
		replicas[digest] = canonical
		if err := f.st.RecordLocalMaterialReplicaReferences(t.Context(), receiver.memberID, replicas); err != nil {
			t.Fatal(err)
		}
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/installed") {
				f.requests.Add(1)
			} else {
				f.transfers.Add(1)
			}
			if r.Method != http.MethodPost || r.URL.Path != materialManifestPath+"/"+digest+"/installed" || r.URL.RawQuery != "" || r.ContentLength > 0 || r.Header.Get("Authorization") != "" {
				http.Error(w, "invalid installed request", 400)
				return
			}
			if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
				http.Error(w, "authentication required", 401)
				return
			}
			key, err := trust.VerifyMember(r.TLS.PeerCertificates[0], time.Now().UTC())
			if err != nil || key != f.members[0].KeyFingerprint || f.fault == "peer-auth" {
				http.Error(w, "source rejected", 403)
				return
			}
			if _, err := f.st.GrantClusterMaterials(r.Context(), clusterID, key); err != nil {
				http.Error(w, "grant denied", 503)
				return
			}
			if f.fault == "http-cancel" {
				if f.afterHTTP != nil {
					f.afterHTTP()
				}
				<-r.Context().Done()
				return
			}
			var current store.MaterialManifestRecord
			if err := f.st.DB().WithContext(r.Context()).Where("source_member_id=?", 11).First(&current).Error; err != nil || current != f.pointer || f.fault == "peer-pointer" {
				http.Error(w, "pointer differs", 503)
				return
			}
			checked, err := verifyReceivedMaterialManifestBytes(r.Context(), f.st, receiver, f.expected, current)
			if err == nil {
				err = checkMaterialConsumerReferences(r.Context(), f.st, receiver, checked)
			}
			if err == nil {
				err = checkInstalledMaterialManifest(r.Context(), receiver, checked)
			}
			if err != nil {
				http.Error(w, "current bytes missing", 503)
				return
			}
			if f.afterHTTP != nil {
				f.afterHTTP()
			}
			w.Header().Set("X-Gpuworks-Material-Source", "11")
			w.Header().Set("X-Gpuworks-Material-Member", fmt.Sprint(receiver.memberID))
			w.Header().Set("X-Gpuworks-Material-Digest", digest)
			if f.fault == "peer-receipt" {
				w.Header().Set("X-Gpuworks-Material-Member", "99")
			}
			w.WriteHeader(204)
		}))
		_ = server.Listener.Close()
		server.Listener = listener
		pair := pairs[i+1]
		if f.fault == "peer-tls" {
			pair = pairs[0]
		}
		server.TLS, err = enrollmentClientTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}, ca)
		if err != nil {
			t.Fatal(err)
		}
		server.StartTLS()
		t.Cleanup(server.Close)
	}
	return f
}

func restartFixtureSequence(t *testing.T, f *restartMaterialFixture) int64 {
	t.Helper()
	var admission store.ClusterAdmission
	if err := f.st.DB().Where("key_fingerprint=?", f.members[0].KeyFingerprint).First(&admission).Error; err != nil {
		t.Fatal(err)
	}
	return admission.MaterialGrantSequence
}

// Keep fixtures honest: the source inventory must contain real identity bytes
// and a managed agent consumer, rather than only a permissive master-key stub.
func assertRestartFixtureCoverage(t *testing.T, f *restartMaterialFixture) {
	t.Helper()
	inventory, err := InspectRequiredMaterials(t.Context(), f.root, f.st)
	if err != nil {
		t.Fatal(err)
	}
	for _, class := range []MaterialClass{MaterialMasterSecret, MaterialMemberIdentity, MaterialAgentOverride} {
		if !slices.ContainsFunc(inventory.Files, func(file MaterialFile) bool { return file.Class == class }) {
			t.Fatalf("missing real %s fixture bytes", class)
		}
	}
	raw, err := os.ReadFile(filepath.Join(f.root, "material-replicas", f.pointer.Digest[:2], f.pointer.Digest))
	if err != nil {
		t.Fatal(err)
	}
	var manifest materialManifest
	if err := json.Unmarshal(raw, &manifest); err != nil || len(manifest.Classes) != len(requiredMaterialClasses) {
		t.Fatalf("incomplete fixture manifest: %v", err)
	}
}
