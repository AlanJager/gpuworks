//go:build linux && cgo && dqlite

package main

import (
	"context"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/canonical/go-dqlite/v3/client"
	"golang.org/x/sys/unix"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

// testCluster is real cluster identity material: an Ed25519 authority, member
// certificates it issued and the transport policy file the server publishes.
type testCluster struct {
	t         *testing.T
	dir       string
	id        string
	authority *identity.Authority
	caPEM     []byte
	caPath    string
	caPin     string
	policy    string
	revision  uint64
}

type testMember struct {
	address  string
	key      string
	certPath string
	keyPath  string
	certPEM  []byte
	keyPEM   []byte
}

func newTestCluster(t *testing.T, id string) *testCluster {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "identity")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	caPEM, caKey, err := identity.NewAuthority(id, now)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := identity.CertificateFingerprint(caPEM)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := identity.LoadAuthority(caPEM, caKey, id, pin, now)
	if err != nil {
		t.Fatal(err)
	}
	c := &testCluster{t: t, dir: dir, id: id, authority: authority, caPEM: caPEM, caPin: pin,
		caPath: filepath.Join(dir, "ca.pem"), policy: filepath.Join(dir, "transport-policy.json")}
	if err := os.WriteFile(c.caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return c
}

func (c *testCluster) member(name, address string) testMember {
	c.t.Helper()
	keyPEM, err := identity.NewMemberKey()
	if err != nil {
		c.t.Fatal(err)
	}
	csr, err := identity.NewMemberCSR(keyPEM)
	if err != nil {
		c.t.Fatal(err)
	}
	key, err := identity.CSRKeyFingerprint(csr)
	if err != nil {
		c.t.Fatal(err)
	}
	certPEM, err := c.authority.IssueMember(csr, key, address, time.Now().UTC(), time.Hour)
	if err != nil {
		c.t.Fatal(err)
	}
	m := testMember{address: address, key: key, certPEM: certPEM, keyPEM: keyPEM,
		certPath: filepath.Join(c.dir, name+".crt"), keyPath: filepath.Join(c.dir, name+".key")}
	if err := os.WriteFile(m.certPath, certPEM, 0o600); err != nil {
		c.t.Fatal(err)
	}
	if err := os.WriteFile(m.keyPath, keyPEM, 0o600); err != nil {
		c.t.Fatal(err)
	}
	return m
}

// publish writes the next policy revision the way the server does.
func (c *testCluster) publish(allowed, revoked []testMember) {
	c.t.Helper()
	next := identity.TransportPolicy{ClusterID: c.id, CAFingerprint: c.caPin, Revision: c.revision + 1, AllowedKeys: []string{}, RevokedKeys: []string{}}
	for _, m := range allowed {
		next.AllowedKeys = append(next.AllowedKeys, m.key)
	}
	for _, m := range revoked {
		next.RevokedKeys = append(next.RevokedKeys, m.key)
	}
	slices.Sort(next.AllowedKeys)
	slices.Sort(next.RevokedKeys)
	if err := identity.PublishTransportPolicy(c.policy, c.revision, next); err != nil {
		c.t.Fatal(err)
	}
	c.revision++
}

func (c *testCluster) serveArgs(dir string, m testMember, extra ...string) []string {
	args := []string{"--dir", dir, "--address", m.address, "--cert", m.certPath, "--key", m.keyPath, "--ca", c.caPath, "--policy", c.policy}
	return append(args, extra...)
}

// dial is the client side of a member: its certificate, the pinned authority
// and no opinion of its own about which peers are admitted.
func (c *testCluster) dial(m testMember) client.DialFunc {
	c.t.Helper()
	now := time.Now().UTC()
	trust, err := identity.NewTrust(c.caPEM, c.id, c.caPin, now)
	if err != nil {
		c.t.Fatal(err)
	}
	_, config, err := trust.TLSConfigs(m.certPEM, m.keyPEM, now, func(string) error { return nil })
	if err != nil {
		c.t.Fatal(err)
	}
	return func(ctx context.Context, address string) (net.Conn, error) {
		return (&tls.Dialer{Config: config.Clone()}).DialContext(ctx, "tcp", address)
	}
}

// reaches reports whether a member's certificate gets an answer from the node.
// TLS 1.3 reports a refused client certificate on the first read, so only a
// completed request counts as admission.
func (c *testCluster) reaches(address string, m testMember) bool {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := client.New(ctx, address, client.WithDialFunc(c.dial(m)))
	if err != nil {
		return false
	}
	defer conn.Close()
	leader, err := conn.Leader(ctx)
	return err == nil && leader != nil && leader.Address == address
}

func TestTransportPolicyDecidesEveryHandshake(t *testing.T) {
	cluster := newTestCluster(t, "00112233445566778899aabbccddeeff")
	address := freeAddress(t)
	local := cluster.member("local", address)
	admitted := cluster.member("admitted", freeAddress(t))
	later := cluster.member("later", freeAddress(t))
	revoked := cluster.member("revoked", freeAddress(t))
	stranger := cluster.member("stranger", freeAddress(t))
	cluster.publish([]testMember{local, admitted, revoked}, nil)
	cluster.publish([]testMember{local, admitted}, []testMember{revoked})

	node := startServe(t, cluster.serveArgs(dataDir(t), local)...)
	if !cluster.reaches(address, admitted) {
		t.Fatal("an admitted member was refused")
	}
	if cluster.reaches(address, revoked) {
		t.Fatal("a revoked member with a valid certificate reached the database")
	}
	if cluster.reaches(address, stranger) || cluster.reaches(address, later) {
		t.Fatal("a member that was never admitted reached the database")
	}

	// The server publishes while the node runs; the next handshake follows it.
	cluster.publish([]testMember{local, admitted, later}, []testMember{revoked})
	if !cluster.reaches(address, later) {
		t.Fatal("a member admitted while running is still refused")
	}
	cluster.publish([]testMember{local, later}, []testMember{admitted, revoked})
	if cluster.reaches(address, admitted) {
		t.Fatal("a member revoked while running still reaches the database")
	}
	if !cluster.reaches(address, later) {
		t.Fatal("revoking one member refused another")
	}

	other := newTestCluster(t, "ffeeddccbbaa99887766554433221100")
	foreign := other.member("foreign", freeAddress(t))
	// This client does not judge the server at all, so only the node's own
	// check of the foreign certificate can refuse it.
	pair, err := tls.X509KeyPair(foreign.certPEM, foreign.keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	careless := func(ctx context.Context, address string) (net.Conn, error) {
		config := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, InsecureSkipVerify: true}
		return (&tls.Dialer{Config: config}).DialContext(ctx, "tcp", address)
	}
	short, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	if conn, err := client.New(short, address, client.WithDialFunc(careless)); err == nil {
		_, err = conn.Leader(short)
		_ = conn.Close()
		if err == nil {
			t.Fatal("a certificate of another cluster reached the database")
		}
	}
	plain, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if conn, err := client.New(plain, address); err == nil {
		_, err = conn.Leader(plain)
		_ = conn.Close()
		if err == nil {
			t.Fatal("a plaintext client reached the database")
		}
	}
	node.stop(t, syscall.SIGTERM)
}

func TestUnreadablePolicyRefusesHandshakesUntilItReturns(t *testing.T) {
	cluster := newTestCluster(t, "00112233445566778899aabbccddeeff")
	address := freeAddress(t)
	local := cluster.member("local", address)
	peer := cluster.member("peer", freeAddress(t))
	cluster.publish([]testMember{local, peer}, nil)
	good, err := os.ReadFile(cluster.policy)
	if err != nil {
		t.Fatal(err)
	}
	node := startServe(t, cluster.serveArgs(dataDir(t), local)...)
	if !cluster.reaches(address, peer) {
		t.Fatal("an admitted member was refused")
	}
	damage := map[string]func(){
		"missing":        func() {},
		"corrupt":        func() { must(t, os.WriteFile(cluster.policy, good[:len(good)/2], 0o600)) },
		"unknown field":  func() { must(t, os.WriteFile(cluster.policy, append([]byte(`{"extra":1,`), good[1:]...), 0o600)) },
		"world readable": func() { must(t, os.WriteFile(cluster.policy, good, 0o600)); must(t, os.Chmod(cluster.policy, 0o644)) },
	}
	for name, apply := range damage {
		_ = os.Remove(cluster.policy)
		apply()
		if cluster.reaches(address, peer) {
			t.Fatalf("policy %s: the member was still admitted", name)
		}
	}
	// A policy of another authority must not be honored either.
	other := newTestCluster(t, "ffeeddccbbaa99887766554433221100")
	other.publish([]testMember{peer}, nil)
	foreign, err := os.ReadFile(other.policy)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(cluster.policy)
	must(t, os.WriteFile(cluster.policy, foreign, 0o600))
	if cluster.reaches(address, peer) {
		t.Fatal("a policy of another cluster admitted the member")
	}
	_ = os.Remove(cluster.policy)
	must(t, os.WriteFile(cluster.policy, good, 0o600))
	if !cluster.reaches(address, peer) {
		t.Fatal("the restored policy did not admit the member again")
	}
	node.stop(t, syscall.SIGTERM)
}

func TestOlderPolicyRevisionIsRefused(t *testing.T) {
	cluster := newTestCluster(t, "00112233445566778899aabbccddeeff")
	address := freeAddress(t)
	local := cluster.member("local", address)
	peer := cluster.member("peer", freeAddress(t))
	cluster.publish([]testMember{local, peer}, nil)
	first, err := os.ReadFile(cluster.policy)
	if err != nil {
		t.Fatal(err)
	}
	cluster.publish([]testMember{local}, []testMember{peer})
	node := startServe(t, cluster.serveArgs(dataDir(t), local)...)
	if cluster.reaches(address, peer) {
		t.Fatal("a revoked member reached the database")
	}
	// Putting the earlier file back must not resurrect the revoked key.
	must(t, os.Remove(cluster.policy))
	must(t, os.WriteFile(cluster.policy, first, 0o600))
	if cluster.reaches(address, peer) {
		t.Fatal("an older policy revision re-admitted a revoked member")
	}
	node.stop(t, syscall.SIGTERM)
}

func TestTransportMaterialFailsClosedBeforeAnyData(t *testing.T) {
	cluster := newTestCluster(t, "00112233445566778899aabbccddeeff")
	address := freeAddress(t)
	local := cluster.member("local", address)
	peer := cluster.member("peer", freeAddress(t))
	cluster.publish([]testMember{local, peer}, nil)
	other := newTestCluster(t, "ffeeddccbbaa99887766554433221100")
	foreign := other.member("foreign", address)
	other.publish([]testMember{foreign}, nil)
	unlisted := cluster.member("unlisted", address)
	garbage := filepath.Join(cluster.dir, "garbage.pem")
	must(t, os.WriteFile(garbage, []byte("not pem\n"), 0o600))
	absent := filepath.Join(cluster.dir, "absent")
	open := filepath.Join(cluster.dir, "open-policy.json")
	raw, err := os.ReadFile(cluster.policy)
	if err != nil {
		t.Fatal(err)
	}
	must(t, os.WriteFile(open, raw, 0o644))

	// cert, key, ca, policy
	cases := map[string][4]string{
		"missing certificate":      {absent, local.keyPath, cluster.caPath, cluster.policy},
		"missing key":              {local.certPath, absent, cluster.caPath, cluster.policy},
		"missing authority":        {local.certPath, local.keyPath, absent, cluster.policy},
		"missing policy":           {local.certPath, local.keyPath, cluster.caPath, absent},
		"corrupt policy":           {local.certPath, local.keyPath, cluster.caPath, garbage},
		"policy readable by all":   {local.certPath, local.keyPath, cluster.caPath, open},
		"policy of another":        {local.certPath, local.keyPath, cluster.caPath, other.policy},
		"authority of another":     {local.certPath, local.keyPath, other.caPath, cluster.policy},
		"certificate of another":   {foreign.certPath, foreign.keyPath, cluster.caPath, cluster.policy},
		"key of another member":    {local.certPath, peer.keyPath, cluster.caPath, cluster.policy},
		"certificate is not pem":   {garbage, local.keyPath, cluster.caPath, cluster.policy},
		"authority is not pem":     {local.certPath, local.keyPath, garbage, cluster.policy},
		"local member not allowed": {unlisted.certPath, unlisted.keyPath, cluster.caPath, cluster.policy},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			dir := dataDir(t)
			got := runHelper(t, "--dir", dir, "--address", address, "--cert", files[0], "--key", files[1], "--ca", files[2], "--policy", files[3])
			if got.code != 1 || got.stdout != "" || got.stderr == "" {
				t.Fatalf("%+v", got)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("refused transport material still wrote data: %v, %v", entries, err)
			}
		})
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestPeerSocketGetsTheLivenessLimits(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	dialed, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer dialed.Close()
	accepted, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()
	if err := configurePeerSocket(accepted); err != nil {
		t.Fatal(err)
	}
	raw, err := accepted.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	options := map[string]int{"TCP_USER_TIMEOUT": unix.TCP_USER_TIMEOUT, "TCP_KEEPIDLE": unix.TCP_KEEPIDLE, "TCP_KEEPINTVL": unix.TCP_KEEPINTVL, "TCP_KEEPCNT": unix.TCP_KEEPCNT}
	want := map[string]int{"TCP_USER_TIMEOUT": 3000, "TCP_KEEPIDLE": 1, "TCP_KEEPINTVL": 1, "TCP_KEEPCNT": 3}
	must(t, raw.Control(func(fd uintptr) {
		for name, option := range options {
			got, err := unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, option)
			if err != nil || got != want[name] {
				t.Errorf("%s = %d, %v; want %d", name, got, err, want[name])
			}
		}
		if on, err := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_KEEPALIVE); err != nil || on == 0 {
			t.Errorf("SO_KEEPALIVE = %d, %v", on, err)
		}
	}))
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	if err := configurePeerSocket(local); err == nil {
		t.Fatal("a connection that is not TCP was accepted for the database listener")
	}
}
