//go:build linux && dqlite

package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"encoding/json"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

// These bytes model the real native protocol independently of its decoder.
// Native members are not obtained from an HTTP status or persisted raft cache.
func installNativePacket(leader uint64, address string, proof bool, legacy bool) []byte {
	return installNativeMembersPacket(leader, address, proof, legacy, []cluster.Member{{ID: leader, Address: address, Role: "voter"}})
}

func installNativeMembersPacket(leader uint64, address string, proof bool, legacy bool, members []cluster.Member) []byte {
	word := func(buf *[]byte, value uint64) {
		var raw [8]byte
		binary.LittleEndian.PutUint64(raw[:], value)
		*buf = append(*buf, raw[:]...)
	}
	text := func(buf *[]byte, value string) {
		*buf = append(*buf, []byte(value)...)
		*buf = append(*buf, 0)
		for len(*buf)%8 != 0 {
			*buf = append(*buf, 0)
		}
	}
	var body []byte
	if proof {
		for _, value := range []uint64{1, 7, 21, 20, leader, uint64(len(members))} {
			word(&body, value)
		}
		for _, member := range members {
			word(&body, member.ID)
			text(&body, member.Address)
			role := uint64(0)
			if member.Role == "stand-by" {
				role = 1
			}
			if member.Role == "spare" {
				role = 2
			}
			word(&body, role)
		}
	} else {
		word(&body, leader)
		text(&body, address)
	}
	header := make([]byte, 8)
	binary.LittleEndian.PutUint32(header, uint32(len(body)/8))
	if proof {
		header[4] = 3
		header[5] = 2
		if legacy {
			header[5] = 0
		}
	} else {
		header[4] = 1
	}
	return append(header, body...)
}

func serveInstallNativePacket(conn net.Conn, address string, leader uint64, legacy bool) error {
	defer conn.Close()
	request := make([]byte, 24)
	if _, err := io.ReadFull(conn, request); err != nil {
		return err
	}
	leaderRequest := []byte{1, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	proofRequest := []byte{1, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 16, 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0}
	proof := bytes.Equal(request, proofRequest)
	if !proof && !bytes.Equal(request, leaderRequest) {
		return errors.New("unexpected native request bytes")
	}
	_, err := conn.Write(installNativePacket(leader, address, proof, legacy))
	return err
}

func TestClusterInstallNativeObservationUsesLiteralFreshBarrierAndPinnedLeader(t *testing.T) {
	for _, failure := range []string{"", "leader-key", "leader-id", "old-wire", "policy-change", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			req := clusterInstallNodeRequest{Host: "192.0.2.1", Database: 9001}
			address := "192.0.2.1:9001"
			dials := 0
			done := make(chan error, 3)
			dial := func(ctx context.Context, target string) (net.Conn, string, error) {
				dials++
				if target != address {
					t.Errorf("redirected outside pinned fixture: %s", target)
				}
				local, remote := net.Pipe()
				t.Cleanup(func() { local.Close(); remote.Close() })
				leader := uint64(7)
				if failure == "leader-id" && dials == 3 {
					leader = 8
				}
				go func() { done <- serveInstallNativePacket(remote, address, leader, failure == "old-wire") }()
				key := strings.Repeat("a", 64)
				if failure == "leader-key" && dials == 3 {
					key = strings.Repeat("b", 64)
				}
				return local, key, nil
			}
			ctx := t.Context()
			if failure == "cancel" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			proof, err := clusterInstallReadNative(ctx, req, dial, func() error {
				if failure == "policy-change" {
					return errors.New("source policy changed")
				}
				return nil
			})
			if failure == "" {
				if err != nil || proof.LeaderID != 7 || len(proof.Members) != 1 || proof.Members[0].Role != "voter" {
					t.Fatalf("live barrier observation failed: %#v %v", proof, err)
				}
				if dials != 3 {
					t.Fatal("leader hint was not followed by two independent native proofs")
				}
			} else if err == nil || proof.LeaderID != 0 {
				t.Fatalf("failed authority observation returned a proof: %#v %v", proof, err)
			}
		})
	}
}

func TestClusterInstallNativeTLSUsesActualClusterTrustAndCurrentAuthorization(t *testing.T) {
	for _, failure := range []string{"", "wrong-ca", "revoked-local", "wrong-local-peer", "joined-serving", "joined-serving-corrupt", "joined-serving-revoked", "seed-serving", "seed-serving-corrupt"} {
		t.Run(failure, func(t *testing.T) {
			cid := "00112233445566778899aabbccddeeff"
			now := time.Now().UTC()
			ca, caKey, err := identity.NewAuthority(cid, now)
			if err != nil {
				t.Fatal(err)
			}
			pin, err := identity.CertificateFingerprint(ca)
			if err != nil {
				t.Fatal(err)
			}
			authority, err := identity.LoadAuthority(ca, caKey, cid, pin, now)
			if err != nil {
				t.Fatal(err)
			}
			key, err := identity.NewMemberKey()
			if err != nil {
				t.Fatal(err)
			}
			csr, _ := identity.NewMemberCSR(key)
			memberKey, _ := identity.CSRKeyFingerprint(csr)
			cert, err := authority.IssueMember(csr, memberKey, "127.0.0.1:9001", now, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			serverCert, serverKey := cert, key
			allowed := []string{memberKey}
			if failure == "wrong-local-peer" {
				serverKey, _ = identity.NewMemberKey()
				serverCSR, _ := identity.NewMemberCSR(serverKey)
				serverID, _ := identity.CSRKeyFingerprint(serverCSR)
				serverCert, err = authority.IssueMember(serverCSR, serverID, "127.0.0.1:9001", now, time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				allowed = append(allowed, serverID)
				sort.Strings(allowed)
			}
			policy := identity.TransportPolicy{ClusterID: cid, CAFingerprint: pin, Revision: 1, SourceRevision: 1, AllowedKeys: allowed}
			trust, err := identity.NewTrust(ca, cid, pin, now)
			if err != nil {
				t.Fatal(err)
			}
			authorize, err := trust.PolicyAuthorizer(policy)
			if err != nil {
				t.Fatal(err)
			}
			listenerTLS, _, err := trust.TLSConfigs(serverCert, serverKey, now, authorize)
			if err != nil {
				t.Fatal(err)
			}
			listener, err := tls.Listen("tcp", "127.0.0.1:0", listenerTLS)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			address := listener.Addr().String()
			_, port, _ := net.SplitHostPort(address)
			number := listener.Addr().(*net.TCPAddr).Port
			serverDone := make(chan struct{})
			go func() {
				defer close(serverDone)
				for {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					go func() { _ = serveInstallNativePacket(conn, address, 7, false) }()
				}
			}()
			defer func() { listener.Close(); <-serverDone }()
			dir := t.TempDir()
			enrollment := dir + "/data/cluster-enrollment"
			policyDir := dir + "/data/gpuworks.db.join"
			seedServing := strings.HasPrefix(failure, "seed-serving")
			if seedServing {
				policyDir = dir + "/data/gpuworks.db.dqlite/.transport"
			}
			if err := os.MkdirAll(enrollment, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(policyDir, 0700); err != nil {
				t.Fatal(err)
			}
			for name, raw := range map[string][]byte{"ca.pem": ca, "server.pem": cert, "server.key": key} {
				if err := os.WriteFile(filepath.Join(enrollment, name), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if failure == "revoked-local" || failure == "joined-serving-revoked" {
				other, _ := identity.NewMemberKey()
				otherCSR, _ := identity.NewMemberCSR(other)
				otherKey, _ := identity.CSRKeyFingerprint(otherCSR)
				policy.AllowedKeys = []string{otherKey}
				policy.RevokedKeys = []string{memberKey}
			}
			policyFile := policyDir + "/transport-policy.json"
			if seedServing {
				policyFile = policyDir + "/policy.json"
			}
			if err := identity.PublishTransportPolicy(policyFile, 0, policy); err != nil {
				t.Fatal(err)
			}
			req := clusterInstallNodeRequest{Host: "127.0.0.1", Hosts: []string{"127.0.0.1"}, Database: number, Dir: dir, ClusterID: cid, CAFingerprint: pin}
			if !seedServing {
				writeInstallJoinedIdentity(t, req, key)
			}
			if failure == "wrong-ca" {
				req.CAFingerprint = strings.Repeat("0", 64)
			}
			if failure == "joined-serving-corrupt" || failure == "seed-serving-corrupt" {
				if err := os.WriteFile(policyFile, []byte(`{"broken":`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if failure == "joined-serving-corrupt" || failure == "joined-serving-revoked" {
				// A valid seed policy must never rescue a corrupt/revoked selected
				// joined policy. Runtime identity chooses the path before loading.
				alternate := dir + "/data/gpuworks.db.dqlite/.transport"
				if err := os.MkdirAll(alternate, 0700); err != nil {
					t.Fatal(err)
				}
				valid := identity.TransportPolicy{ClusterID: cid, CAFingerprint: pin, Revision: 1, SourceRevision: 1, AllowedKeys: allowed}
				if err := identity.PublishTransportPolicy(alternate+"/policy.json", 0, valid); err != nil {
					t.Fatal(err)
				}
			}
			if failure == "joined-serving" {
				if _, err := os.Stat(dir + "/data/gpuworks.db.dqlite/.transport"); !os.IsNotExist(err) {
					t.Fatal("joined-serving fixture accidentally created seed transport")
				}
			}
			joining := !seedServing && !strings.HasPrefix(failure, "joined-serving")
			proof, err := clusterInstallObserveNative(t.Context(), req, joining)
			if failure == "" || failure == "joined-serving" || failure == "seed-serving" {
				if err != nil || proof.LeaderID != 7 || proof.Members[0].Address != "127.0.0.1:"+port {
					t.Fatalf("actual authenticated native observation failed: %#v %v", proof, err)
				}
			} else if err == nil || proof.LeaderID != 0 {
				t.Fatalf("invalid transport authority accepted: %#v %v", proof, err)
			}
		})
	}
}

func TestClusterInstallFinalNativeThreeVotersRejectsCacheShapeAndWrongTuple(t *testing.T) {
	req := clusterInstallNodeRequest{Hosts: []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"}, Database: 9001}
	for _, failure := range []string{"", "spare", "duplicate-id", "wrong-address", "missing", "cache-only"} {
		t.Run(failure, func(t *testing.T) {
			proof := cluster.QuorumProof{Term: 7, Index: 21, ConfigurationIndex: 20, LeaderID: 7}
			for index, host := range req.Hosts {
				proof.Members = append(proof.Members, cluster.Member{ID: uint64(index + 7), Address: fmt.Sprintf("%s:9001", host), Role: "voter"})
			}
			switch failure {
			case "spare":
				proof.Members[1].Role = "spare"
			case "duplicate-id":
				proof.Members[1].ID = 7
			case "wrong-address":
				proof.Members[1].Address = "192.0.2.99:9001"
			case "cache-only":
				proof.Term = 0
			case "missing":
				proof.Members = proof.Members[:2]
			}
			if got := clusterInstallThreeVoters(req, proof); got != (failure == "") {
				t.Fatalf("unexpected native membership result %t", got)
			}
		})
	}
}

func TestClusterInstallJoiningSpareUsesInitialVoterNativeHint(t *testing.T) {
	cid := "00112233445566778899aabbccddeeff"
	now := time.Now().UTC()
	ca, caKey, err := identity.NewAuthority(cid, now)
	if err != nil {
		t.Fatal(err)
	}
	pin, _ := identity.CertificateFingerprint(ca)
	authority, err := identity.LoadAuthority(ca, caKey, cid, pin, now)
	if err != nil {
		t.Fatal(err)
	}
	makeMember := func(host string) ([]byte, []byte, string) {
		key, err := identity.NewMemberKey()
		if err != nil {
			t.Fatal(err)
		}
		csr, _ := identity.NewMemberCSR(key)
		fingerprint, _ := identity.CSRKeyFingerprint(csr)
		cert, err := authority.IssueMember(csr, fingerprint, host+":9001", now, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return cert, key, fingerprint
	}
	seedCert, seedKey, seedFP := makeMember("127.0.0.1")
	spareCert, spareKey, spareFP := makeMember("127.0.0.2")
	allowed := []string{seedFP, spareFP}
	sort.Strings(allowed)
	policy := identity.TransportPolicy{ClusterID: cid, CAFingerprint: pin, Revision: 1, SourceRevision: 1, AllowedKeys: allowed}
	trust, err := identity.NewTrust(ca, cid, pin, now)
	if err != nil {
		t.Fatal(err)
	}
	authorize, _ := trust.PolicyAuthorizer(policy)
	seedTLS, _, err := trust.TLSConfigs(seedCert, seedKey, now, authorize)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := tls.Listen("tcp", "127.0.0.1:0", seedTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Close()
	port := seed.Addr().(*net.TCPAddr).Port
	seedAddress := fmt.Sprintf("127.0.0.1:%d", port)
	spareAddress := fmt.Sprintf("127.0.0.2:%d", port)
	spareTLS, _, err := trust.TLSConfigs(spareCert, spareKey, now, authorize)
	if err != nil {
		t.Fatal(err)
	}
	spare, err := tls.Listen("tcp", spareAddress, spareTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer spare.Close()
	members := []cluster.Member{{ID: 7, Address: seedAddress, Role: "voter"}, {ID: 8, Address: spareAddress, Role: "spare"}}
	var mu sync.Mutex
	seedCalls, spareCalls := 0, 0
	serve := func(listener net.Listener, isSpare bool) {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer connection.Close()
				request := make([]byte, 24)
				if _, err := io.ReadFull(connection, request); err != nil {
					return
				}
				leaderRequest := []byte{1, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
				proofRequest := []byte{1, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 16, 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0}
				mu.Lock()
				if isSpare {
					spareCalls++
				} else {
					seedCalls++
				}
				mu.Unlock()
				if isSpare {
					// Pinned native gateway handle_leader only reports a hint for voters.
					// A running authenticated spare returns id=0,address="", not a hint.
					if !bytes.Equal(request, leaderRequest) {
						t.Error("spare was asked to prove quorum")
					}
					_, _ = connection.Write(installNativeMembersPacket(0, "", false, false, nil))
					return
				}
				proof := bytes.Equal(request, proofRequest)
				if !proof && !bytes.Equal(request, leaderRequest) {
					t.Error("unexpected native request")
				}
				_, _ = connection.Write(installNativeMembersPacket(7, seedAddress, proof, false, members))
			}()
		}
	}
	go serve(seed, false)
	go serve(spare, true)
	dir := t.TempDir()
	enrollment := dir + "/data/cluster-enrollment"
	policyDir := dir + "/data/gpuworks.db.join"
	if err := os.MkdirAll(enrollment, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(policyDir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{"ca.pem": ca, "server.pem": spareCert, "server.key": spareKey} {
		if err := os.WriteFile(filepath.Join(enrollment, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := identity.PublishTransportPolicy(policyDir+"/transport-policy.json", 0, policy); err != nil {
		t.Fatal(err)
	}
	req := clusterInstallNodeRequest{Host: "127.0.0.2", Hosts: []string{"127.0.0.1", "127.0.0.2", "127.0.0.3"}, Database: port, Dir: dir, ClusterID: cid, CAFingerprint: pin}
	writeInstallJoinedIdentity(t, req, spareKey)
	// The old self-contact contract cannot complete even though the spare is
	// alive, mutually authenticated and already in the seed's native membership.
	selfContact := req
	selfContact.Hosts = []string{"127.0.0.2", "127.0.0.1", "127.0.0.3"}
	if proof, err := clusterInstallObserveNative(t.Context(), selfContact, true); err == nil || proof.LeaderID != 0 || !strings.Contains(err.Error(), "no canonical leader") {
		t.Fatalf("faithful spare response was accepted: %#v %v", proof, err)
	}
	proof, err := clusterInstallObserveNative(t.Context(), req, true)
	if err != nil || proof.LeaderID != 7 || len(proof.Members) != 2 || proof.Members[1] != members[1] {
		t.Fatalf("initial voter could not independently observe admitted spare: %#v %v", proof, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if spareCalls != 1 || seedCalls != 3 {
		t.Fatalf("joining observation did not avoid spare hint: spare=%d seed=%d", spareCalls, seedCalls)
	}
}

func writeInstallJoinedIdentity(t *testing.T, req clusterInstallNodeRequest, key []byte) {
	t.Helper()
	join := req.Dir + "/data/gpuworks.db.join"
	if err := os.MkdirAll(join, 0700); err != nil {
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
	host := req.Host
	if len(req.Hosts) > 0 {
		host = req.Hosts[0]
	}
	info := app.ClusterJoinInfo{ClusterJoinTarget: app.ClusterJoinTarget{ClusterID: req.ClusterID, CAFingerprint: req.CAFingerprint, Protocol: proto.ClusterProtocolVersion, Address: fmt.Sprintf("%s:%d", req.Host, req.Database), EnrollmentURL: "https://" + host + ":9443/api/v1/cluster/enrollment/certificate", EnrollmentOrigin: "https://" + req.Host + ":9443", EnrollmentListen: req.Host + ":9443"}, KeyFingerprint: fingerprint}
	raw, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(join+"/config.json", raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(join+"/member.key", key, 0600); err != nil {
		t.Fatal(err)
	}
}
