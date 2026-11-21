//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"maps"
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
	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Reserve every actual alias listener before opening any native or HTTPS node.
func reserveBootstrapAliasPorts(t *testing.T, hosts []string) ([]net.Listener, int) {
	t.Helper()
	var listeners []net.Listener
	port := 0
	for _, host := range hosts {
		listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		listeners = append(listeners, listener)
		port = listener.Addr().(*net.TCPAddr).Port
	}
	return listeners, port
}

// All identities, admissions, origins, snapshots, replicas and installations
// below use actual native databases and production mutually authenticated TLS.
func newAliasBootstrapMaterialFixture(t *testing.T, ctx context.Context, hosts []string) (*candidateGrantFixture, int, int) {
	t.Helper()
	f := &candidateGrantFixture{}
	databaseListeners, databasePort := reserveBootstrapAliasPorts(t, hosts)
	enrollmentListeners, enrollmentPort := reserveBootstrapAliasPorts(t, hosts)
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
	trust, err := identity.NewTrust(ca, clusterID, pin, now)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := identity.LoadAuthority(ca, caKey, clusterID, pin, now)
	if err != nil {
		t.Fatal(err)
	}
	expected := store.ClusterIdentity{ClusterID: clusterID, CAFingerprint: pin, Protocol: store.ClusterProtocolVersion}
	var configs []cluster.DatabaseConfig
	var certificates, privateKeys [][]byte
	policy := identity.TransportPolicy{ClusterID: clusterID, CAFingerprint: pin, Revision: 1}
	for i := range hosts {
		address := net.JoinHostPort(hosts[i], strconv.Itoa(databasePort))
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
		certificate, err := authority.IssueMember(csr, fingerprint, address, now, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		dataDir := t.TempDir()
		if err := os.Chmod(dataDir, 0700); err != nil {
			t.Fatal(err)
		}
		configs = append(configs, cluster.DatabaseConfig{Dir: filepath.Join(dataDir, "database"), Address: address, Name: "gpuworks.db"})
		certificates, privateKeys = append(certificates, certificate), append(privateKeys, key)
		f.keys = append(f.keys, fingerprint)
		policy.AllowedKeys = append(policy.AllowedKeys, fingerprint)
	}
	slices.Sort(policy.AllowedKeys)
	for i, cfg := range configs {
		policyPath := filepath.Join(filepath.Dir(cfg.Dir), "transport.json")
		if err := identity.PublishTransportPolicy(policyPath, 0, policy); err != nil {
			t.Fatal(err)
		}
		if i > 0 {
			cfg.Seeds = []string{configs[0].Address}
		}
		if err := databaseListeners[i].Close(); err != nil {
			t.Fatal(err)
		}
		db, err := cluster.OpenAuthorizedDatabase(ctx, cfg, policyPath, trust, certificates[i], privateKeys[i])
		if err != nil {
			t.Fatal(err)
		}
		f.databases = append(f.databases, db)
		t.Cleanup(func() {
			if err := db.Close(); err != nil {
				t.Error(err)
			}
		})
		if i == 0 {
			st, err := store.OpenReplicated(ctx, db.SQL, db.Scheduling, bus.New(), store.ReplicatedBackend())
			if err != nil {
				t.Fatal(err)
			}
			if err := st.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := st.InitializeClusterIdentity(ctx, expected); err != nil {
				t.Fatal(err)
			}
			if _, err := st.InitializeClusterSeedAdmission(ctx, clusterID, f.keys[0], configs[0].Address); err != nil {
				t.Fatal(err)
			}
			admin := store.User{Username: "grant-proof-admin", Role: "admin"}
			if err := st.DB().Create(&admin).Error; err != nil {
				t.Fatal(err)
			}
			for index := 1; index < 3; index++ {
				_, token, err := st.IssueClusterInvitation(ctx, clusterID, admin.ID, time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := st.ClaimClusterInvitation(ctx, clusterID, token, f.keys[index], configs[index].Address); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	for {
		members, err := f.databases[0].Members(ctx)
		if err == nil && len(members) == 3 && slices.ContainsFunc(members, func(m cluster.Member) bool { return m.Role == "voter" }) {
			voters := 0
			for _, m := range members {
				if m.Role == "voter" {
					voters++
				}
			}
			if voters == 3 {
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	for i, db := range f.databases {
		st, err := store.OpenJoinedMemberStore(ctx, db, bus.New())
		if err != nil {
			t.Fatal(err)
		}
		f.stores = append(f.stores, st)
		pair, err := tls.X509KeyPair(append(bytes.Clone(certificates[i]), ca...), privateKeys[i])
		if err != nil {
			t.Fatal(err)
		}
		serverTLS, err := enrollmentClientTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}, ca)
		if err != nil {
			t.Fatal(err)
		}
		bundle := &enrollmentBundle{info: EnrollmentInfo{ClusterID: clusterID, CAFingerprint: pin, Protocol: store.ClusterProtocolVersion}, tls: serverTLS, trust: trust, ca: ca, caKey: caKey, issuer: authority, dataDir: filepath.Dir(configs[i].Dir)}
		f.bundles = append(f.bundles, bundle)
		receiverRoot := filepath.Join(filepath.Dir(configs[i].Dir), "material-replicas")
		if err := os.MkdirAll(receiverRoot, 0700); err != nil {
			t.Fatal(err)
		}
		receiver, err := newMaterialReceiver(receiverRoot, db.ID)
		if err != nil {
			t.Fatal(err)
		}
		f.receivers = append(f.receivers, receiver)
		handler := enrollmentMaterialHandlerWithReceiver(bundle, st, http.NotFoundHandler(), nil, receiver)
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/installed") {
				f.requests.Add(1)
				f.mu.Lock()
				before := f.before
				f.mu.Unlock()
				if before != nil {
					before()
				}
			}
			handler.ServeHTTP(w, r)
		}))
		if err := server.Listener.Close(); err != nil {
			t.Fatal(err)
		}
		server.Listener = enrollmentListeners[i]
		server.TLS = serverTLS.Clone()
		server.StartTLS()
		t.Cleanup(server.Close)
		bundle.info.PublicURL = server.URL
		bundle.info.ListenAddress = server.Listener.Addr().String()
		if err := st.SetClusterEnrollmentOriginForVerifiedKey(ctx, expected, f.keys[i], server.URL); err != nil {
			t.Fatal(err)
		}
	}
	master, override := []byte("shared tiny master"), []byte("tiny agent override")
	for i, receiver := range f.receivers {
		dataDir := filepath.Dir(receiver.root)
		if err := os.MkdirAll(filepath.Join(dataDir, "dl"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dataDir, "master.key"), master, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dataDir, "dl", "tiny-agent"), override, 0600); err != nil {
			t.Fatal(err)
		}
		bundle := f.bundles[i]
		raw, err := json.Marshal(bundle.info)
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(dataDir, enrollmentDirectory)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string][]byte{"config.json": raw, "ca.pem": ca, "ca.key": caKey, "server.pem": certificates[i], "server.key": privateKeys[i], "master.binding": enrollmentMasterBinding(caKey, master)} {
			if err := os.WriteFile(filepath.Join(dir, name), body, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := readEnrollmentBundle(dataDir, false); err != nil {
			t.Fatal(err)
		}
	}
	return f, databasePort, enrollmentPort
}

func bootstrapNativeGrantSequences(t *testing.T, ctx context.Context, f *candidateGrantFixture) map[string]int64 {
	t.Helper()
	var admissions []store.ClusterAdmission
	if err := f.stores[0].DB().WithContext(ctx).Where("key_fingerprint IN ?", f.keys).Find(&admissions).Error; err != nil {
		t.Fatal(err)
	}
	if len(admissions) != 3 {
		t.Fatalf("actual admissions=%d want3", len(admissions))
	}
	result := make(map[string]int64, 3)
	for _, admission := range admissions {
		result[admission.KeyFingerprint] = admission.MaterialGrantSequence
	}
	return result
}

func bootstrapNativeReader(t *testing.T, ctx context.Context, f *candidateGrantFixture, index int, afterHTTP func() error) (func(context.Context, store.ClusterIdentity, string) (store.MaintenancePhaseState, []store.ClusterTransportMember, []store.MaterialManifestRecord, error), *atomic.Int32) {
	t.Helper()
	bundle := f.bundles[index]
	dir := filepath.Join(bundle.dataDir, enrollmentDirectory)
	certificate, err := os.ReadFile(filepath.Join(dir, "server.pem"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := identity.LoadMemberKey(filepath.Join(dir, "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	policy, err := identity.LoadTransportPolicy(filepath.Join(bundle.dataDir, "transport.json"))
	if err != nil {
		t.Fatal(err)
	}
	authorize, err := bundle.trust.PolicyAuthorizer(policy)
	if err != nil {
		t.Fatal(err)
	}
	_, config, err := bundle.trust.TLSConfigs(certificate, key, time.Now().UTC(), authorize)
	if err != nil {
		t.Fatal(err)
	}
	dial := func(ctx context.Context, address string) (net.Conn, string, error) {
		if !slices.ContainsFunc(f.databases, func(db *cluster.Database) bool { return db.Address == address }) {
			return nil, "", errors.New("native bootstrap dial left the actual fixture members")
		}
		conn, err := (&tls.Dialer{Config: config.Clone()}).DialContext(ctx, "tcp", address)
		if err != nil {
			return nil, "", err
		}
		state := conn.(*tls.Conn).ConnectionState()
		peerKey, err := bundle.trust.VerifyMember(state.PeerCertificates[0], time.Now().UTC())
		if err != nil {
			_ = conn.Close()
			return nil, "", err
		}
		return conn, peerKey, nil
	}
	leader, leaderKey, err := cluster.ObserveRaftLeader(ctx, f.databases[index].Address, dial)
	if err != nil {
		t.Fatal(err)
	}
	pinned := func(ctx context.Context, address string) (net.Conn, error) {
		if address != leader.Address {
			return nil, errors.New("bootstrap snapshot changed its pinned leader address")
		}
		conn, peerKey, err := dial(ctx, address)
		if err == nil && peerKey != leaderKey {
			err = errors.New("bootstrap snapshot changed its pinned leader key")
			_ = conn.Close()
		}
		return conn, err
	}
	reads := &atomic.Int32{}
	return func(ctx context.Context, expected store.ClusterIdentity, requesterKey string) (store.MaintenancePhaseState, []store.ClusterTransportMember, []store.MaterialManifestRecord, error) {
		if expected.ClusterID != bundle.info.ClusterID || expected.CAFingerprint != bundle.info.CAFingerprint || expected.Protocol != bundle.info.Protocol || requesterKey != f.keys[index] {
			return store.MaintenancePhaseState{}, nil, nil, errors.New("snapshot caller differs from its real persisted certificate")
		}
		if reads.Add(1) == 2 && afterHTTP != nil {
			if err := afterHTTP(); err != nil {
				return store.MaintenancePhaseState{}, nil, nil, err
			}
		}
		before := bootstrapNativeGrantSequences(t, ctx, f)
		phase, members, pointers, err := store.ReadRemoteClusterMaterials(ctx, expected, leader, leaderKey, requesterKey, "gpuworks.db", pinned)
		if after := bootstrapNativeGrantSequences(t, ctx, f); !maps.Equal(before, after) {
			t.Error("read-only native snapshots modified durable material grants")
		}
		return phase, members, pointers, err
	}, reads
}

func bootstrapNativeCheck(ctx context.Context, f *candidateGrantFixture, index int, hosts []string, databasePort, enrollmentPort int, read func(context.Context, store.ClusterIdentity, string) (store.MaintenancePhaseState, []store.ClusterTransportMember, []store.MaterialManifestRecord, error)) error {
	return CheckClusterMaterialBootstrap(ctx, Config{DataDir: f.bundles[index].dataDir}, hosts[index], f.databases[index].ID, hosts, databasePort, enrollmentPort, read)
}

func TestNativeClusterMaterialBootstrapActualInstalledPeersAndClosedSnapshots(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()
	hosts := []string{"127.0.0.1", "127.0.0.2", "127.0.0.3"}
	f, databasePort, enrollmentPort := newAliasBootstrapMaterialFixture(t, ctx, hosts)
	for i, bundle := range f.bundles {
		if err := replicateRequiredMaterials(ctx, bundle.dataDir, f.stores[i], bundle, "", true, func() {}, enrollmentNativeLog()); err != nil {
			t.Fatalf("actual source%d publication and consumer installation: %v", i, err)
		}
	}
	before := bootstrapNativeGrantSequences(t, ctx, f)
	for i := range hosts {
		read, reads := bootstrapNativeReader(t, ctx, f, i, nil)
		if err := bootstrapNativeCheck(ctx, f, i, hosts, databasePort, enrollmentPort, read); err != nil {
			t.Fatalf("actual host%d bootstrap failed: %v", i, err)
		}
		if reads.Load() != 2 {
			t.Fatalf("closed snapshots=%d want2", reads.Load())
		}
	}
	if f.requests.Load() != 6 {
		t.Fatalf("actual installed POSTs=%d want6", f.requests.Load())
	}
	for key, value := range bootstrapNativeGrantSequences(t, ctx, f) {
		if delta := value - before[key]; delta != 14 {
			t.Fatalf("source durable grants=%d want14 (two installed peers, each one private and six membership grants)", delta)
		}
	}

	t.Run("missing-own-pointer-denies-before-HTTP", func(t *testing.T) {
		read, _ := bootstrapNativeReader(t, ctx, f, 0, nil)
		if err := f.stores[0].DB().WithContext(ctx).Where("source_member_id = ?", int64(f.databases[0].ID)).Delete(&store.MaterialManifestRecord{}).Error; err != nil {
			t.Fatal(err)
		}
		requests := f.requests.Load()
		if err := bootstrapNativeCheck(ctx, f, 0, hosts, databasePort, enrollmentPort, read); err == nil || f.requests.Load() != requests {
			t.Fatalf("missing pointer accepted or issued HTTP: err=%v requests=%d", err, f.requests.Load()-requests)
		}
		bundle := f.bundles[0]
		if err := replicateRequiredMaterials(ctx, bundle.dataDir, f.stores[0], bundle, "", true, func() {}, enrollmentNativeLog()); err != nil {
			t.Fatalf("restore actual publication through production replication: %v", err)
		}
	})
	t.Run("corrupt-consumer-canonical-copy-intact", func(t *testing.T) {
		path := filepath.Join(f.bundles[1].dataDir, "dl", "tiny-agent")
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.WriteFile(path, body, 0600); err != nil {
				t.Error(err)
			}
		})
		digest := materialTestDigest(body)
		canonical := filepath.Join(f.receivers[1].root, digest[:2], digest)
		if got, err := hashFile(canonical); err != nil || got != digest {
			t.Fatal("receiver canonical bytes were not intact", err)
		}
		if err := os.WriteFile(path, []byte("corrupt current consumer"), 0600); err != nil {
			t.Fatal(err)
		}
		read, reads := bootstrapNativeReader(t, ctx, f, 0, nil)
		requests := f.requests.Load()
		if err := bootstrapNativeCheck(ctx, f, 0, hosts, databasePort, enrollmentPort, read); err == nil || !strings.Contains(err.Error(), "HTTP 503") || reads.Load() != 1 || f.requests.Load() == requests {
			t.Fatalf("actual consumer corruption was not rejected by installed HTTP: %v", err)
		}
		if got, err := hashFile(canonical); err != nil || got != digest {
			t.Fatal("consumer-only corruption changed canonical bytes", err)
		}
	})
	t.Run("pointer-change-after-HTTP", func(t *testing.T) {
		var pointer store.MaterialManifestRecord
		if err := f.stores[0].DB().WithContext(ctx).Where("source_member_id = ?", int64(f.databases[0].ID)).First(&pointer).Error; err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := f.stores[0].DB().WithContext(ctx).Model(&store.MaterialManifestRecord{}).Where("source_member_id = ?", pointer.SourceMemberID).UpdateColumn("recorded_at", pointer.RecordedAt).Error; err != nil {
				t.Error(err)
			}
		})
		requests := f.requests.Load()
		read, reads := bootstrapNativeReader(t, ctx, f, 0, func() error {
			if f.requests.Load()-requests != 2 {
				t.Fatal("pointer fault preceded completed installed HTTP")
			}
			result := f.stores[0].DB().WithContext(ctx).Model(&store.MaterialManifestRecord{}).Where("source_member_id = ?", pointer.SourceMemberID).UpdateColumn("recorded_at", pointer.RecordedAt.Add(time.Second))
			if result.Error != nil || result.RowsAffected != 1 {
				t.Fatalf("actual pointer fault did not commit: rows=%d err=%v", result.RowsAffected, result.Error)
			}
			return nil
		})
		if err := bootstrapNativeCheck(ctx, f, 0, hosts, databasePort, enrollmentPort, read); err == nil || !strings.Contains(err.Error(), "phase, members, or pointers changed") || reads.Load() != 2 || f.requests.Load()-requests != 2 {
			t.Fatalf("actual pointer change escaped final native snapshot: err=%v reads=%d HTTP=%d", err, reads.Load(), f.requests.Load()-requests)
		}
	})
	t.Run("phase-change-after-HTTP", func(t *testing.T) {
		leader, err := f.databases[0].Leader(ctx)
		if err != nil {
			t.Fatal(err)
		}
		leaderIndex := slices.IndexFunc(f.databases, func(db *cluster.Database) bool { return db.ID == leader.ID })
		if leaderIndex < 0 {
			t.Fatal("actual leader is not a fixture member")
		}
		st := f.stores[leaderIndex]
		phase, err := st.ReadMaintenancePhase(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			current, err := st.ReadMaintenancePhase(ctx)
			if err == nil && current.Phase == store.MaintenanceFreezing {
				_, err = st.TransitionMaintenancePhase(ctx, current, store.MaintenanceServing)
			}
			if err != nil {
				t.Error(err)
			}
		})
		requests := f.requests.Load()
		read, reads := bootstrapNativeReader(t, ctx, f, 0, func() error {
			if f.requests.Load()-requests != 2 {
				t.Fatal("phase fault preceded completed installed HTTP")
			}
			changed, err := st.TransitionMaintenancePhase(ctx, phase, store.MaintenanceFreezing)
			if err != nil || changed.Phase != store.MaintenanceFreezing || changed.Revision != phase.Revision+1 {
				t.Fatalf("actual phase fault did not commit: phase=%+v err=%v", changed, err)
			}
			return nil
		})
		if err := bootstrapNativeCheck(ctx, f, 0, hosts, databasePort, enrollmentPort, read); !errors.Is(err, store.ErrMaintenanceNotServing) || reads.Load() != 2 || f.requests.Load()-requests != 2 {
			t.Fatalf("actual phase change escaped final native snapshot: err=%v reads=%d HTTP=%d", err, reads.Load(), f.requests.Load()-requests)
		}
	})
	t.Run("caller-cancel-during-real-installed-HTTP", func(t *testing.T) {
		workCtx, stop := context.WithCancel(ctx)
		defer stop()
		read, _ := bootstrapNativeReader(t, ctx, f, 0, nil)
		f.mu.Lock()
		f.before = stop
		f.mu.Unlock()
		t.Cleanup(func() { f.mu.Lock(); f.before = nil; f.mu.Unlock() })
		requests := f.requests.Load()
		if err := bootstrapNativeCheck(workCtx, f, 0, hosts, databasePort, enrollmentPort, read); !errors.Is(err, context.Canceled) || f.requests.Load()-requests != 1 {
			t.Fatalf("actual HTTPS cancellation lost: err=%v HTTP=%d", err, f.requests.Load()-requests)
		}
	})
}
