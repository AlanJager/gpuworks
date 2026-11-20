//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func materialTestDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func TestMaterialReceiverRecoveryRemovesOnlyOrphanedTemporaryFiles(t *testing.T) {
	root := t.TempDir()
	receiver, err := newMaterialReceiver(root, 7)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("published bytes stay in place")
	digest := materialTestDigest(body)
	if _, err := receiver.receive(t.Context(), digest, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(root, digest[:2])
	orphan := filepath.Join(prefix, ".receiving-abandoned")
	if err := os.WriteFile(orphan, []byte("interrupted bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(prefix, "unrelated")
	if err := os.WriteFile(unrelated, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "other"), 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "other", ".receiving-untouched")
	if err := os.WriteFile(outside, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := recoverOrphanedMaterialTransfers(t.Context(), receiver); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Lstat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphaned material temporary file remained: %v", err)
	}
	for path, want := range map[string]string{
		filepath.Join(prefix, digest): string(body),
		unrelated:                     "keep",
		outside:                       "keep",
	} {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Fatalf("material recovery changed %s: %q %v", path, got, err)
		}
	}
}

func TestMaterialReceiverRejectsInterruptedCorruptAndExistingBadBytes(t *testing.T) {
	root := t.TempDir()
	receiver, err := newMaterialReceiver(root, 7)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("durable material source bytes")
	digest := materialTestDigest(body)
	final := filepath.Join(root, digest[:2], digest)
	if _, err := receiver.receive(t.Context(), digest, strings.NewReader("wrong bytes")); err == nil {
		t.Fatal("corrupt bytes were acknowledged")
	}
	if _, err := os.Stat(final); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("corrupt bytes published: %v", err)
	}
	if _, err := receiver.receive(t.Context(), digest, &materialBrokenReader{data: body[:5]}); err == nil {
		t.Fatal("interrupted transfer was acknowledged")
	}
	if _, err := os.Stat(final); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted transfer published: %v", err)
	}
	receiver.syncDir = func(path string) error {
		if path == filepath.Dir(final) {
			return errors.New("simulated directory sync failure")
		}
		return syncMaterialDir(path)
	}
	if _, err := receiver.receive(t.Context(), digest, strings.NewReader(string(body))); err == nil {
		t.Fatal("directory sync failure was acknowledged")
	}
	if err := os.Remove(final); err != nil {
		t.Fatal(err)
	}
	receiver.syncDir = syncMaterialDir
	ack, err := receiver.receive(t.Context(), digest, strings.NewReader(string(body)))
	if err != nil || ack.Digest != digest || ack.MemberID != 7 || ack.Bytes != int64(len(body)) {
		t.Fatalf("valid bytes rejected: %+v %v", ack, err)
	}
	if got, err := os.ReadFile(final); err != nil || string(got) != string(body) {
		t.Fatalf("target member has no correct bytes: %q %v", got, err)
	}
	if _, err := receiver.receive(t.Context(), digest, strings.NewReader(string(body))); err != nil {
		t.Fatalf("idempotent delivery failed: %v", err)
	}
	if err := os.WriteFile(final, []byte("tampered bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.receive(t.Context(), digest, strings.NewReader(string(body))); err == nil {
		t.Fatal("corrupt existing receiver file was acknowledged")
	}
	if err := os.Remove(final); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, digest[:2])); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, digest[:2]), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.receive(t.Context(), digest, strings.NewReader(string(body))); err == nil {
		t.Fatal("receiver disk path failure was acknowledged")
	}
}

func TestMaterialReceiverSyncsTemporaryRemovalBeforeReceipt(t *testing.T) {
	root := t.TempDir()
	receiver, err := newMaterialReceiver(root, 7)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("material with a durable final name")
	digest := materialTestDigest(body)
	parent := filepath.Join(root, digest[:2])
	receiver.syncDir = func(name string) error {
		if name == parent {
			entries, err := os.ReadDir(parent)
			if err != nil {
				return err
			}
			if len(entries) != 1 || entries[0].Name() != digest {
				return errors.New("directory was synced before the temporary material name was removed")
			}
		}
		return syncMaterialDir(name)
	}
	if _, err := receiver.receive(t.Context(), digest, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
}

type materialBrokenReader struct{ data []byte }

type cancelingTransferReader struct {
	cancel    context.CancelFunc
	remaining int
	reads     int
}

func (reader *cancelingTransferReader) Read(p []byte) (int, error) {
	if reader.remaining == 0 {
		return 0, io.EOF
	}
	reader.reads++
	n := len(p)
	if n > reader.remaining {
		n = reader.remaining
	}
	for i := range p[:n] {
		p[i] = 'x'
	}
	reader.remaining -= n
	reader.cancel()
	return n, nil
}

func TestMaterialReceiverStopsReadingAfterCancellation(t *testing.T) {
	root := t.TempDir()
	receiver, err := newMaterialReceiver(root, 7)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	reader := &cancelingTransferReader{cancel: cancel, remaining: 1 << 20}
	digest := strings.Repeat("a", 64)
	if _, err := receiver.receive(ctx, digest, reader); !errors.Is(err, context.Canceled) || reader.reads != 1 {
		t.Fatalf("receiver continued copying after cancellation: reads=%d err=%v", reader.reads, err)
	}
	if entries, err := os.ReadDir(filepath.Join(root, digest[:2])); err != nil || len(entries) != 0 {
		t.Fatalf("cancelled transfer left published or temporary bytes: entries=%v err=%v", entries, err)
	}
}

func TestMaterialReceiverDoesNotPublishAfterSyncCancellation(t *testing.T) {
	root := t.TempDir()
	receiver, err := newMaterialReceiver(root, 7)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("complete bytes canceled after durable staging")
	digest := materialTestDigest(body)
	ctx, cancel := context.WithCancel(t.Context())
	receiver.syncFile = func(file *os.File) error {
		if err := file.Sync(); err != nil {
			return err
		}
		cancel()
		return nil
	}
	if receipt, err := receiver.receive(ctx, digest, bytes.NewReader(body)); !errors.Is(err, context.Canceled) || receipt != (materialReceipt{}) {
		t.Fatalf("canceled transfer returned a receipt: %+v %v", receipt, err)
	}
	entries, err := os.ReadDir(filepath.Join(root, digest[:2]))
	if err != nil || len(entries) != 0 {
		t.Fatalf("canceled post-sync transfer published bytes or kept staging: %v %v", entries, err)
	}
}

func (reader *materialBrokenReader) Read(p []byte) (int, error) {
	if len(reader.data) == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(p, reader.data)
	reader.data = reader.data[n:]
	return n, nil
}

func TestMaterialTransferUsesRealMemberTLSAndPersistsOnReceiver(t *testing.T) {
	now := time.Now().UTC()
	clusterID := strings.Repeat("b", 32)
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
	issue := func(address string) (tls.Certificate, string) {
		t.Helper()
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
		cert, err := authority.IssueMember(csr, fingerprint, address, now, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		pair, err := tls.X509KeyPair(append(cert, ca...), key)
		if err != nil {
			t.Fatal(err)
		}
		return pair, fingerprint
	}
	serverPair, serverKey := issue("127.0.0.1:9910")
	senderPair, senderKey := issue("127.0.0.1:9911")
	strangerPair, _ := issue("127.0.0.1:9912")
	tlsConfig, err := enrollmentClientTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverPair}}, ca)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "materials.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.InitializeClusterIdentity(t.Context(), store.ClusterIdentity{ClusterID: clusterID, CAFingerprint: pin, Protocol: store.ClusterProtocolVersion}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InitializeClusterSeedAdmission(t.Context(), clusterID, serverKey, "127.0.0.1:9910"); err != nil {
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
	if _, err := st.ClaimClusterInvitation(t.Context(), clusterID, token, senderKey, "127.0.0.1:9911"); err != nil {
		t.Fatal(err)
	}
	bundle := &enrollmentBundle{info: EnrollmentInfo{ClusterID: clusterID, CAFingerprint: pin, Protocol: store.ClusterProtocolVersion}, tls: tlsConfig, trust: trust}
	targetRoot := t.TempDir()
	receiver, err := newMaterialReceiver(targetRoot, 7)
	if err != nil {
		t.Fatal(err)
	}
	eligibility := newMaterialEligibility(time.Now, func(context.Context) (bool, error) { return true, nil }, time.Hour)
	closed := &App{ha: &memberRuntime{}, memberState: memberState{joinedClosed: true, materialEligibility: eligibility}}
	receiver.onChange = closed.invalidateCandidateMaterialEligibility
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready {
		t.Fatalf("initial material proof: ready=%v err=%v", ready, err)
	}
	var uploadedBytes atomic.Int64
	handler := enrollmentMaterialHandlerWithReceiver(bundle, st, http.NotFoundHandler(), nil, receiver)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, materialReplicaPath+"/") {
			uploadedBytes.Add(r.ContentLength)
		}
		handler.ServeHTTP(w, r)
	}))
	server.TLS = tlsConfig
	server.StartTLS()
	t.Cleanup(server.Close)
	clientFor := func(pair tls.Certificate) *http.Client {
		pinned, err := identity.EnrollmentTLSConfig(clusterID, pin, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		pinned.Certificates = []tls.Certificate{pair}
		return &http.Client{Transport: &http.Transport{TLSClientConfig: pinned}, Timeout: 10 * time.Second}
	}
	sourceDir := t.TempDir()
	source := filepath.Join(sourceDir, "bytes")
	body := []byte("source member bytes, delivered to the target member")
	if err := os.WriteFile(source, body, 0600); err != nil {
		t.Fatal(err)
	}
	digest := materialTestDigest(body)
	senderBundle := *bundle
	senderBundle.tls = &tls.Config{Certificates: []tls.Certificate{senderPair}}
	client, err := memberMaterialClient(&senderBundle, server.URL, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	assertMaterialHandshakeBlackhole(t, client, digest, int64(len(body)))
	wrongPeer, err := memberMaterialClient(&senderBundle, server.URL, senderKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sendMaterialReplica(t.Context(), wrongPeer, server.URL, source, digest, 7); err == nil {
		t.Fatal("same-CA peer with the wrong member key received material")
	}
	unpinned := clientFor(senderPair)
	unpinned.Transport.(*http.Transport).TLSClientConfig.VerifyConnection = nil
	if _, err := sendMaterialReplica(t.Context(), unpinned, server.URL, source, digest, 7); err == nil {
		t.Fatal("unverified HTTPS client was allowed to send material")
	}
	if !eligibility.Eligible() {
		t.Fatal("rejected transfer closed material eligibility")
	}
	ack, err := sendMaterialReplica(t.Context(), client, server.URL, source, digest, 7)
	if err != nil || ack.MemberID != 7 || ack.Bytes != int64(len(body)) {
		t.Fatalf("member transfer failed: %+v %v", ack, err)
	}
	if eligibility.Eligible() {
		t.Fatal("published receiver bytes retained old material proof")
	}
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready {
		t.Fatalf("fresh material proof: ready=%v err=%v", ready, err)
	}
	target := filepath.Join(targetRoot, digest[:2], digest)
	if got, err := os.ReadFile(target); err != nil || string(got) != string(body) {
		t.Fatalf("receiving member lacks bytes: %q %v", got, err)
	}
	recorder := &candidateProofRecorder{}
	proofCtx := context.WithValue(t.Context(), candidateProofMetricsKey{}, recorder)
	if err := verifyCandidateRemoteReplica(proofCtx, client, server.URL, digest, int64(len(body)), 7); err != nil {
		t.Fatalf("authenticated remote read failed: %v", err)
	}
	if got := recorder.snapshot().RemoteGet; got.Calls != 1 || got.VerifiedBytes != int64(len(body)) || got.Duration <= 0 {
		t.Fatalf("remote GET metrics=%+v", got)
	}
	if err := probeRemoteMaterialReplica(t.Context(), client, server.URL, digest, int64(len(body)), 7); err != nil {
		t.Fatalf("authenticated current-byte probe failed: %v", err)
	}
	beforeFirst := uploadedBytes.Load()
	changes := 0
	onTransfer := func() { changes++ }
	transferred, err := ensureRemoteMaterialReplica(t.Context(), st, client, server.URL, source, digest, int64(len(body)), 7, onTransfer)
	if err != nil || !transferred || uploadedBytes.Load() != beforeFirst+int64(len(body)) || changes != 1 {
		t.Fatalf("unrecorded remote bytes bypassed transfer notice: transferred=%v bytes=%d changes=%d err=%v", transferred, uploadedBytes.Load()-beforeFirst, changes, err)
	}
	if !eligibility.Eligible() {
		t.Fatal("idempotent receiver upload closed material eligibility")
	}
	if err := st.RecordMaterialRemoteReceipt(t.Context(), digest, 7, int64(len(body))); err != nil {
		t.Fatal(err)
	}
	beforeReuse := uploadedBytes.Load()
	transferred, err = ensureRemoteMaterialReplica(t.Context(), st, client, server.URL, source, digest, int64(len(body)), 7, onTransfer)
	if err != nil || transferred || uploadedBytes.Load() != beforeReuse || changes != 1 {
		t.Fatalf("current remote bytes triggered transfer notice: transferred=%v bytes=%d changes=%d err=%v", transferred, uploadedBytes.Load()-beforeReuse, changes, err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	transferred, err = ensureRemoteMaterialReplica(t.Context(), st, client, server.URL, source, digest, int64(len(body)), 7, onTransfer)
	if err != nil || !transferred || uploadedBytes.Load() != beforeReuse+int64(len(body)) || changes != 2 {
		t.Fatalf("missing remote bytes lacked repair notice: transferred=%v bytes=%d changes=%d err=%v", transferred, uploadedBytes.Load()-beforeReuse, changes, err)
	}
	if eligibility.Eligible() {
		t.Fatal("repaired receiver bytes retained old material proof")
	}
	if err := probeRemoteMaterialReplica(t.Context(), wrongPeer, server.URL, digest, int64(len(body)), 7); err == nil {
		t.Fatal("probe accepted a same-CA peer with the wrong key")
	}
	if err := probeRemoteMaterialReplica(t.Context(), client, server.URL, digest, int64(len(body)), 8); err == nil {
		t.Fatal("probe accepted the wrong member ID")
	}
	if err := verifyRemoteMaterialReplica(t.Context(), client, server.URL, digest, int64(len(body)), 8); err == nil {
		t.Fatal("remote read accepted the wrong member")
	}
	if _, err := sendMaterialReplica(t.Context(), client, server.URL, source, digest, 8); err == nil {
		t.Fatal("wrong receiving member identity was accepted")
	}
	corrupt := []byte("wire body differs")
	corruptDigest := materialTestDigest([]byte("expected wire body"))
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+materialReplicaPath+"/"+corruptDigest, bytes.NewReader(corrupt))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("corrupt wire body returned HTTP %d", response.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(targetRoot, corruptDigest[:2], corruptDigest)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("corrupt wire body was published: %v", err)
	}
	var rows int64
	if err := st.DB().Model(&store.MaterialReplica{}).Count(&rows).Error; err != nil || rows != 1 {
		t.Fatalf("receiver did not index exactly one durable local replica: %d %v", rows, err)
	}
	var local store.MaterialReplica
	if err := st.DB().Where("digest = ? AND member_id = ?", digest, int64(7)).First(&local).Error; err != nil || local.Path != target {
		t.Fatalf("receiver indexed a noncanonical path: %+v %v", local, err)
	}
	stranger := clientFor(strangerPair)
	other := []byte("unadmitted source")
	otherSource := filepath.Join(sourceDir, "other")
	if err := os.WriteFile(otherSource, other, 0600); err != nil {
		t.Fatal(err)
	}
	otherDigest := materialTestDigest(other)
	if _, err := sendMaterialReplica(context.Background(), stranger, server.URL, otherSource, otherDigest, 7); err == nil {
		t.Fatal("unadmitted TLS member was accepted")
	}
	if _, err := os.Stat(filepath.Join(targetRoot, otherDigest[:2], otherDigest)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unadmitted source wrote receiver bytes: %v", err)
	}
	if err := verifyRemoteMaterialReplica(t.Context(), stranger, server.URL, digest, int64(len(body)), 7); err == nil {
		t.Fatal("unadmitted member read remote bytes")
	}
	if err := os.WriteFile(target, bytes.Repeat([]byte("x"), len(body)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyRemoteMaterialReplica(t.Context(), client, server.URL, digest, int64(len(body)), 7); err == nil {
		t.Fatal("remote read accepted corrupted bytes")
	}
	if err := probeRemoteMaterialReplica(t.Context(), client, server.URL, digest, int64(len(body)), 7); err == nil {
		t.Fatal("remote probe accepted corrupted current bytes")
	}
	rejectedTransfers := 0
	if transferred, err := ensureRemoteMaterialReplica(t.Context(), st, client, server.URL, source, digest, int64(len(body)), 7, func() { rejectedTransfers++ }); err == nil || transferred || rejectedTransfers != 0 {
		t.Fatalf("rejected remote transfer changed eligibility: transferred=%t notices=%d err=%v", transferred, rejectedTransfers, err)
	}
	if err := st.DB().Migrator().DropTable(&store.MaterialReplica{}); err != nil {
		t.Fatal(err)
	}
	retryBody := []byte("durable bytes outlive an indexing failure")
	retryDigest := materialTestDigest(retryBody)
	retrySource := filepath.Join(sourceDir, "retry")
	if err := os.WriteFile(retrySource, retryBody, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := sendMaterialReplica(t.Context(), client, server.URL, retrySource, retryDigest, 7); err == nil {
		t.Fatal("receiver acknowledged bytes before the local index committed")
	}
	if received, err := os.ReadFile(filepath.Join(targetRoot, retryDigest[:2], retryDigest)); err != nil || !bytes.Equal(received, retryBody) {
		t.Fatalf("index failure removed durable candidate bytes: %v", err)
	}
	if err := st.DB().AutoMigrate(&store.MaterialReplica{}); err != nil {
		t.Fatal(err)
	}
	if _, err := sendMaterialReplica(t.Context(), client, server.URL, retrySource, retryDigest, 7); err != nil {
		t.Fatalf("receiver could not index durable bytes on retry: %v", err)
	}
	var recovered store.MaterialReplica
	if err := st.DB().Where("digest = ? AND member_id = ?", retryDigest, int64(7)).First(&recovered).Error; err != nil {
		t.Fatalf("retry did not record the local member copy: %v", err)
	}
	if err := os.WriteFile(target, body, 0600); err != nil {
		t.Fatal(err)
	}
	batch := []materialBatchItem{{Digest: digest, Bytes: int64(len(body))}, {Digest: retryDigest, Bytes: int64(len(retryBody))}}
	beforeGrant, err := st.ClusterAdmissionForVerifiedKey(t.Context(), clusterID, senderKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyRemoteMaterialBatch(t.Context(), client, server.URL, batch, 7); err != nil {
		t.Fatalf("authenticated batch read failed: %v", err)
	}
	afterGrant, err := st.ClusterAdmissionForVerifiedKey(t.Context(), clusterID, senderKey)
	if err != nil || afterGrant.MaterialGrantSequence != beforeGrant.MaterialGrantSequence+1 {
		t.Fatalf("two-file batch used %d grants: %v", afterGrant.MaterialGrantSequence-beforeGrant.MaterialGrantSequence, err)
	}
	if err := os.WriteFile(target, bytes.Repeat([]byte("x"), len(body)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyRemoteMaterialBatch(t.Context(), client, server.URL, batch, 7); err == nil {
		t.Fatal("batch accepted damaged remote bytes")
	}
	if err := os.WriteFile(target, body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(recovered.Path); err != nil {
		t.Fatal(err)
	}
	if err := verifyRemoteMaterialBatch(t.Context(), client, server.URL, batch, 7); err == nil {
		t.Fatal("batch accepted a missing remote member file")
	}
	if err := os.WriteFile(recovered.Path, retryBody, 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyRemoteMaterialBatch(t.Context(), client, server.URL, batch, 8); err == nil {
		t.Fatal("batch accepted wrong receiver identity")
	}
	canceled, stop := context.WithCancel(t.Context())
	stop()
	if err := verifyRemoteMaterialBatch(canceled, client, server.URL, batch, 7); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled batch returned %v", err)
	}
	assertLargeMaterialStreamProgress(t, &senderBundle, bundle, st, serverKey)
	if _, err := st.RevokeClusterAdmission(t.Context(), clusterID, senderKey); err != nil {
		t.Fatal(err)
	}
	if err := verifyRemoteMaterialBatch(t.Context(), client, server.URL, batch, 7); err == nil {
		t.Fatal("revoked source read batch material")
	}
	assertMaterialBodyStall(t, client, tlsConfig, digest, int64(len(body)))
}
