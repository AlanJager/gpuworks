//go:build linux && dqlite

package app

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
)

// forwardCluster is a set of members joined by a real mutual-TLS member
// route on loopback. Each member's business handler names the member, so a
// test sees which process served a request.
type forwardCluster struct {
	t       *testing.T
	ca      *x509.Certificate
	caKey   *ecdsa.PrivateKey
	pool    *x509.CertPool
	members map[uint64]*forwardMember
}

type forwardMember struct {
	id         uint64
	forward    *memberForward
	public     *httptest.Server
	route      net.Listener
	cert       tls.Certificate
	leader     atomic.Uint64 // what this member observes; 0 is "no answer"
	sessions   atomic.Int64
	dropped    atomic.Int64
	authorized atomic.Int64
	admit      atomic.Bool
	seen       chan *http.Request
}

func newForwardCluster(t *testing.T, ids ...uint64) *forwardCluster {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "member route test authority"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	c := &forwardCluster{t: t, ca: ca, caKey: key, pool: x509.NewCertPool(), members: map[uint64]*forwardMember{}}
	c.pool.AddCert(ca)
	for _, id := range ids {
		c.members[id] = c.newMember(id)
	}
	for _, member := range c.members {
		for _, peer := range c.members {
			member.forward.SetMemberRoute(peer.id, peer.route.Addr().String())
		}
		go func() { _ = member.forward.ServeMemberSessions(member.route) }()
	}
	t.Cleanup(func() {
		for _, member := range c.members {
			_ = member.forward.CloseMemberSessions(context.Background())
			member.public.Close()
		}
	})
	return c
}

func (c *forwardCluster) certificate(name string, serial int64) tls.Certificate {
	c.t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		c.t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, template, c.ca, &key.PublicKey, c.caKey)
	if err != nil {
		c.t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func (c *forwardCluster) newMember(id uint64) *forwardMember {
	c.t.Helper()
	m := &forwardMember{id: id, cert: c.certificate(fmt.Sprintf("member-%d", id), int64(id)+10), seen: make(chan *http.Request, 16)}
	m.admit.Store(true)
	m.forward = newMemberForward(nil,
		func(ctx context.Context) (uint64, error) {
			if leader := m.leader.Load(); leader != 0 {
				return leader, nil
			}
			return 0, errors.New("no leader observed")
		},
		func() int { return int(m.sessions.Load()) },
		func() { m.dropped.Add(1); m.sessions.Store(0) })
	business := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case m.seen <- r:
		default:
		}
		switch r.URL.Path {
		case "/stream":
			flusher := w.(http.Flusher)
			fmt.Fprintf(w, "first from %d\n", id)
			flusher.Flush()
			<-r.Context().Done()
		case "/upgrade":
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: member-test\r\n\r\n")
			_ = rw.Flush()
			line, _ := rw.ReadString('\n')
			fmt.Fprintf(rw, "member %d echoes %s", id, line)
			_ = rw.Flush()
		case "/die":
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
		default:
			body, _ := io.ReadAll(r.Body)
			w.Header().Set("X-Served-By", fmt.Sprint(id))
			fmt.Fprintf(w, "member=%d remote=%s host=%s tls=%t body=%s spoof=%q", id, r.RemoteAddr, r.Host, r.TLS != nil, body, r.Header.Get(memberForwardRemoteHeader))
		}
	})
	m.forward.local = business
	server := &tls.Config{Certificates: []tls.Certificate{m.cert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: c.pool, MinVersion: tls.VersionTLS13}
	peerTLS := func(context.Context, uint64, string) (*tls.Config, error) {
		return &tls.Config{Certificates: []tls.Certificate{m.cert}, RootCAs: c.pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS13}, nil
	}
	authorize := func(context.Context, *x509.Certificate) error {
		m.authorized.Add(1)
		if !m.admit.Load() {
			return errors.New("not admitted")
		}
		return nil
	}
	if err := m.forward.SetMemberSecureTransport(id, server, peerTLS, authorize); err != nil {
		c.t.Fatal(err)
	}
	route, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		c.t.Fatal(err)
	}
	m.route = route
	m.public = httptest.NewServer(m.forward.wrap(business))
	return m
}

// lead makes every member observe leader, and waits until each acts on it.
func (c *forwardCluster) lead(leader uint64) {
	c.t.Helper()
	for _, member := range c.members {
		member.leader.Store(leader)
	}
	deadline := time.Now().Add(5 * time.Second)
	for _, member := range c.members {
		for {
			if observed, known := member.forward.observedLeader(); known && observed == leader {
				break
			}
			if time.Now().After(deadline) {
				c.t.Fatalf("member %d never observed leader %d", member.id, leader)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func forwardGet(t *testing.T, url string, header ...string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

func TestMemberForwardsBusinessToTheLeaderAsTheClientSentIt(t *testing.T) {
	c := newForwardCluster(t, 1, 2, 3)
	c.lead(2)
	// The leader serves its own requests itself.
	if status, body, _ := forwardGet(t, c.members[2].public.URL+"/api/v1/nodes"); status != 200 || !strings.HasPrefix(body, "member=2 ") {
		t.Fatalf("leader did not serve locally: %d %q", status, body)
	}
	// Any other member hands the request to the leader. The leader sees the
	// client's address and Host, no TLS, and nothing the client claimed
	// about itself in the member route's own headers.
	follower := c.members[1].public
	req, _ := http.NewRequest(http.MethodPost, follower.URL+"/api/v1/settings/x", strings.NewReader("payload"))
	req.Host = "console.example:8816"
	req.Header.Set(memberForwardRemoteHeader, "203.0.113.9:1")
	req.Header.Set(memberForwardTLSHeader, "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	got := string(body)
	if resp.StatusCode != 200 || resp.Header.Get("X-Served-By") != "2" {
		t.Fatalf("follower answer: %d served-by=%q %q", resp.StatusCode, resp.Header.Get("X-Served-By"), got)
	}
	for _, want := range []string{"member=2 ", "remote=127.0.0.1:", "host=console.example:8816", "tls=false", "body=payload", `spoof=""`} {
		if !strings.Contains(got, want) {
			t.Fatalf("forwarded request lost %q: %q", want, got)
		}
	}
	// A probe asks about the member that received it.
	for _, path := range []string{"/healthz", "/api/v1/healthz", "/api/v1/readyz"} {
		if _, body, _ := forwardGet(t, follower.URL+path); !strings.HasPrefix(body, "member=1 ") {
			t.Fatalf("probe %s left the member: %q", path, body)
		}
	}
	// The membership check is paid per connection, not per request.
	for range 5 {
		forwardGet(t, follower.URL+"/again")
	}
	if n := c.members[2].authorized.Load(); n < 1 || n > 2 {
		t.Fatalf("leader authorized the forwarding member %d times for kept-alive requests", n)
	}
}

func TestMemberForwardCarriesStreamsAndUpgrades(t *testing.T) {
	c := newForwardCluster(t, 1, 2)
	c.lead(2)
	// An event stream reaches the client while the handler is still running.
	ctx, cancel := context.WithCancel(t.Context())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.members[1].public.URL+"/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	cancel()
	resp.Body.Close()
	if err != nil || line != "first from 2\n" {
		t.Fatalf("stream through a follower: %q %v", line, err)
	}
	// An upgraded connection (the agent session, a terminal) is spliced to
	// the leader in both directions.
	conn, err := net.Dial("tcp", strings.TrimPrefix(c.members[1].public.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(conn, "GET /upgrade HTTP/1.1\r\nHost: entry\r\nConnection: Upgrade\r\nUpgrade: member-test\r\n\r\n")
	reader := bufio.NewReader(conn)
	status, _ := reader.ReadString('\n')
	if !strings.Contains(status, "101") {
		t.Fatalf("upgrade through a follower answered %q", status)
	}
	for {
		header, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if header == "\r\n" {
			break
		}
	}
	fmt.Fprintf(conn, "ping\n")
	if echo, err := reader.ReadString('\n'); err != nil || echo != "member 2 echoes ping\n" {
		t.Fatalf("upgraded connection: %q %v", echo, err)
	}
}

func TestMemberForwardRefusesInsteadOfGuessing(t *testing.T) {
	c := newForwardCluster(t, 1, 2, 3)
	refused := func(name, url string) {
		t.Helper()
		if status, body, _ := forwardGet(t, url); status != http.StatusServiceUnavailable || !strings.Contains(body, "member business service is not ready") {
			t.Fatalf("%s: %d %q", name, status, body)
		}
	}
	// No member has observed a leader: nobody serves business.
	refused("no leader observed", c.members[1].public.URL+"/api/v1/nodes")
	c.lead(2)
	// The observation ages out when the database stops answering.
	c.members[1].leader.Store(0)
	c.members[1].forward.observed.Store(&memberLeaderObservation{leader: 2, at: time.Now().Add(-memberLeaderFresh - time.Second)})
	refused("stale observation", c.members[1].public.URL+"/api/v1/nodes")

	// Members disagree for a moment after a leader change: 1 still names 2,
	// while 2 already sees 3 lead. Member 2 refuses; it does not forward a
	// forwarded request, so the request never loops and never reaches 3.
	c.members[1].leader.Store(2)
	c.members[1].forward.observed.Store(&memberLeaderObservation{leader: 2, at: time.Now()})
	c.members[2].leader.Store(3)
	c.members[2].forward.observed.Store(&memberLeaderObservation{leader: 3, at: time.Now()})
	for len(c.members[3].seen) > 0 {
		<-c.members[3].seen
	}
	refused("forwarded to a member that no longer leads", c.members[1].public.URL+"/api/v1/nodes")
	if len(c.members[3].seen) != 0 || len(c.members[2].seen) != 0 {
		t.Fatal("a forwarded request was served or forwarded again by a member that does not lead")
	}

	// A member the leader does not admit is refused before business runs.
	c.lead(3)
	c.members[3].admit.Store(false)
	if status, _, _ := forwardGet(t, c.members[1].public.URL+"/api/v1/nodes"); status != http.StatusForbidden {
		t.Fatalf("unadmitted member was answered %d", status)
	}
	if len(c.members[3].seen) != 0 {
		t.Fatal("business ran for a member that is not admitted")
	}
}

func TestMemberForwardSurvivesTheLeaderDyingMidRequest(t *testing.T) {
	c := newForwardCluster(t, 1, 2, 3)
	c.lead(2)
	// The leader drops the connection while answering: the client gets a
	// refusal it may retry, not a hang and not another member's answer.
	if status, _, _ := forwardGet(t, c.members[1].public.URL+"/die"); status != http.StatusServiceUnavailable {
		t.Fatalf("a request the leader dropped was answered %d", status)
	}
	// The leader's route goes away entirely; requests are refused until the
	// members observe the next leader, then served by it.
	_ = c.members[2].forward.CloseMemberSessions(context.Background())
	if status, _, _ := forwardGet(t, c.members[1].public.URL+"/api/v1/nodes"); status != http.StatusServiceUnavailable {
		t.Fatalf("request to a dead leader was answered %d", status)
	}
	c.members[1].leader.Store(3)
	c.members[3].leader.Store(3)
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, body, _ := forwardGet(t, c.members[1].public.URL+"/api/v1/nodes")
		if status == 200 && strings.HasPrefix(body, "member=3 ") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("requests did not follow the new leader: %d %q", status, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMemberThatDoesNotLeadClosesItsAgentSessions(t *testing.T) {
	c := newForwardCluster(t, 1, 2)
	c.members[1].sessions.Store(3)
	c.members[2].sessions.Store(4)
	c.lead(2)
	deadline := time.Now().Add(5 * time.Second)
	for c.members[1].dropped.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a member that does not lead kept its agent sessions")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if c.members[2].dropped.Load() != 0 || c.members[2].sessions.Load() != 4 {
		t.Fatal("the leader closed its own agent sessions")
	}
	// No answer from the database is not a verdict: sessions stay.
	c.members[2].leader.Store(0)
	time.Sleep(3 * memberLeaderObserveInterval)
	if c.members[2].dropped.Load() != 0 {
		t.Fatal("sessions were closed although no other leader was observed")
	}
	// A session that lands here later (the entry does not know the leader)
	// is closed on the next observation too.
	c.members[1].sessions.Store(1)
	before := c.members[1].dropped.Load()
	deadline = time.Now().Add(5 * time.Second)
	for c.members[1].dropped.Load() == before {
		if time.Now().After(deadline) {
			t.Fatal("a later session on a member that does not lead was kept")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestMemberForwardIsInertWithoutARoute(t *testing.T) {
	// A single server, or a store that is not a native cluster, never
	// configures the secure route: every request is served locally.
	served := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "local") })
	for name, configure := range map[string]func(*memberForward){
		"never configured": func(*memberForward) {},
		"identity only":    func(f *memberForward) { f.SetMemberIdentity(7, []byte("key")) },
	} {
		forward := newMemberForward(nil, func(context.Context) (uint64, error) { return 9, nil }, nil, nil)
		configure(forward)
		public := httptest.NewServer(forward.wrap(served))
		if status, body, _ := forwardGet(t, public.URL+"/api/v1/nodes"); status != 200 || body != "local" {
			t.Fatalf("%s: %d %q", name, status, body)
		}
		public.Close()
		if err := forward.CloseMemberSessions(context.Background()); err != nil {
			t.Fatalf("%s: close: %v", name, err)
		}
	}
	forward := newMemberForward(nil, nil, nil, nil)
	if err := forward.SetMemberSecureTransport(0, nil, nil, nil); err == nil {
		t.Fatal("an incomplete secure transport was accepted")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := forward.ServeMemberSessions(ln); err == nil {
		t.Fatal("an unconfigured route was served")
	}
}

// Each change of the leader a member sends business to leaves one line with
// the leader before and after, whether this member leads, how long the last
// observation was ago and how many agent sessions it closed; an unchanged
// leader leaves none.
func TestALeaderChangeLeavesOneBusinessLine(t *testing.T) {
	var out strings.Builder
	log := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey || a.Key == "since_last_observation_ms" {
			return slog.Attr{}
		}
		return a
	}}))
	var leader atomic.Uint64
	held, dropped := 3, 0
	forward := newMemberForward(log, func(context.Context) (uint64, error) { return leader.Load(), nil },
		func() int { return held }, func() { dropped++; held = 0 })
	forward.SetMemberIdentity(7, []byte("key"))
	ctx := context.Background()
	for _, id := range []uint64{7, 7, 9, 9, 0, 7} {
		leader.Store(id)
		forward.observeLeaderOnce(ctx)
	}
	want := `level=INFO msg="business follows a new database leader" new_leader=7 this_member=7 leads=true learned="leader observation" agent_sessions_closed=0
level=INFO msg="business follows a new database leader" new_leader=9 this_member=7 leads=false learned="leader observation" agent_sessions_closed=3 old_leader=7
level=INFO msg="closed agent sessions held by a member that does not lead" sessions=3 leader=9
level=INFO msg="business follows a new database leader" new_leader=7 this_member=7 leads=true learned="leader observation" agent_sessions_closed=0 old_leader=9
`
	if out.String() != want {
		t.Fatalf("logged\n%s\nwant\n%s", out.String(), want)
	}
	if dropped != 1 {
		t.Fatalf("sessions were closed %d times", dropped)
	}
}

// A leader observation is acted on for three seconds at the tiers' own
// timing and longer, in proportion, on a tier that elects later: at an
// election timeout of 3 s a leader's death takes up to two of them to a new
// leader, and three seconds of freshness would make every member refuse
// business on every leader change.
func TestALeaderObservationStaysFreshThroughAnElectionOfTheTier(t *testing.T) {
	at := func(name string) storagetier.Tier {
		tier, err := storagetier.Parse(name)
		if err != nil {
			t.Fatal(err)
		}
		return tier
	}
	for name, want := range map[string]time.Duration{
		"standard": 3 * time.Second, "slow-storage": 3 * time.Second,
		"slow-storage@100ms": 6 * time.Second, "slow-storage@200ms": 12 * time.Second,
	} {
		tier := at(name)
		if got := memberLeaderFreshFor(tier); got != want {
			t.Errorf("%s: fresh for %s, want %s", name, got, want)
		}
		if _, longest := tier.ElectionTimer(); memberLeaderFreshFor(tier) < longest+memberLeaderObserveInterval {
			t.Errorf("%s: an observation is fresh for %s, shorter than the tier's longest election timer %s and one observation", name, memberLeaderFreshFor(tier), longest)
		}
	}
	// The member acts on what the tier of its own process allows.
	cluster.UseStorageTier(at("slow-storage@200ms"))
	t.Cleanup(func() { cluster.UseStorageTier(storagetier.Standard) })
	f := &memberForward{}
	f.observed.Store(&memberLeaderObservation{leader: 2, at: time.Now().Add(-8 * time.Second)})
	if leader, ok := f.observedLeader(); !ok || leader != 2 {
		t.Fatalf("an observation 8 s old is not acted on at an election timeout of 3 s: %d, %v", leader, ok)
	}
	f.observed.Store(&memberLeaderObservation{leader: 2, at: time.Now().Add(-13 * time.Second)})
	if _, ok := f.observedLeader(); ok {
		t.Fatal("an observation 13 s old is acted on")
	}
	cluster.UseStorageTier(storagetier.SlowStorage)
	f.observed.Store(&memberLeaderObservation{leader: 2, at: time.Now().Add(-4 * time.Second)})
	if _, ok := f.observedLeader(); ok {
		t.Fatal("an observation 4 s old is acted on at slow-storage's timing")
	}
}
