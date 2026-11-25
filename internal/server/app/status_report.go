//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
)

// ErrBusinessStatusNotReady accompanies a complete report when an explicit
// business probe cannot confirm admission, including transport failures.
var ErrBusinessStatusNotReady = errors.New("business service readiness was not confirmed")

// ObserveClusterStatus reads the persisted raft directory and probes listeners.
// It does not join, promote a member, open business routes, or start a database.
func ObserveClusterStatus(ctx context.Context, cfg Config, httpAddr string, timeout time.Duration) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if timeout <= 0 {
		return "", errors.New("status requires a positive timeout")
	}
	report, err := observeClusterStatus(ctx, cfg, httpAddr, timeout)
	if err != nil {
		return "", err
	}
	// Peer/quorum discovery can outlast an earlier material eligibility result.
	// Observe business admission last, through the same gate as real requests.
	if httpAddr != "" && report.httpState == "listening" {
		report.business, report.businessObservation = observeBusiness(ctx, httpAddr, timeout)
	} else if httpAddr != "" {
		report.businessObservation = "readyz skipped: HTTP listener " + report.httpState
	}
	if httpAddr != "" && report.business != "open" {
		return report.format(), fmt.Errorf("%w: %s", ErrBusinessStatusNotReady, report.business)
	}
	return report.format(), nil
}

// observeClusterStatus is the observation a status report is written from,
// short of the business probe: the persisted membership, this member's
// listeners, the other members' database ports, the leader's view when one
// answers, and the transport policy.
func observeClusterStatus(ctx context.Context, cfg Config, httpAddr string, timeout time.Duration) (clusterStatus, error) {
	report, err := readClusterStatus(cfg)
	if err != nil {
		return clusterStatus{}, err
	}
	report.probeLocal(ctx, httpAddr, timeout)
	report.peers = probePeerDatabases(ctx, report, timeout)
	report.observeQuorum(ctx, cfg, timeout)
	report.observeTransportPolicy(cfg)
	report.observedAt, report.probeTimeout = time.Now(), timeout
	return report, nil
}

type clusterStatus struct {
	id                  uint64
	address             string
	clusterID           string
	role                string
	roleSource          string
	voters              int
	faultTolerant       bool
	directory           string
	sync                string
	httpState           string
	enrollment          string
	database            string
	business            string
	businessObservation string
	materials           string
	leader              string
	leaderID            uint64
	term                string
	commit              string
	configIndex         string
	members             []statusMember
	peers               []statusPeer
	enrollmentListen    string
	httpAddr            string
	pin                 string
	publicURL           string
	protocol            uint
	storageTier         storagetier.Tier
	joinURL             string
	cert                []byte
	key                 []byte
	// enrolledKeys and revokedKeys count the member keys the local transport
	// policy names, policyChanged is when it last changed, observedAt is when
	// this report was taken and probeTimeout the deadline each of its probes
	// had; see membershipShortfall.
	enrolledKeys  int
	revokedKeys   int
	policyChanged time.Time
	observedAt    time.Time
	probeTimeout  time.Duration
}

type statusMember struct {
	id      uint64
	address string
	role    string
	voter   bool
	source  string
}

type statusPeer struct {
	id         uint64
	address    string
	database   string
	enrollment string
	material   string
	// origin is the peer's enrollment listener and key its member key
	// fingerprint, as the leader reported them; empty when no leader was
	// read.
	origin, key string
}

type persistedRaftNode struct {
	ID      uint64 `yaml:"ID"`
	Address string `yaml:"Address"`
	Role    *int   `yaml:"Role"`
}

func readClusterStatus(cfg Config) (clusterStatus, error) {
	if cfg.DataDir == "" {
		return clusterStatus{}, errors.New("status requires a data directory")
	}
	dbPath := cfg.DBPath
	if dbPath == "" {
		dbPath = filepath.Join(cfg.DataDir, "gpuworks.db")
	}
	raftDir := dbPath + ".dqlite"
	info, members, err := readPersistedRaft(raftDir)
	if err != nil {
		return clusterStatus{}, err
	}
	report := clusterStatus{
		id: info.ID, address: info.Address, clusterID: "not recorded",
		role: "not recorded", roleSource: "cluster.yaml cache",
		directory: "persisted raft directory has metadata and a log or snapshot",
		sync:      "not observed", httpState: "not observed", enrollment: "not recorded",
		database: "not recorded", business: "not observed", materials: "not observed",
		leader: "not observed", term: "not observed", commit: "not observed", configIndex: "not observed",
	}
	if notice := joinedDatabaseListenNotice(raftDir); notice != "" {
		report.directory = notice
		report.sync = "not replicating"
	}
	for _, member := range members {
		role, voter := persistedRaftRole(member.Role)
		if voter {
			report.voters++
		}
		report.members = append(report.members, statusMember{id: member.ID, address: member.Address, role: role, voter: voter, source: "cluster.yaml cache"})
		if member.ID == info.ID {
			report.role = role
		}
	}
	report.faultTolerant = report.voters >= 3 && report.role == "voter"
	sort.Slice(report.members, func(i, j int) bool { return report.members[i].id < report.members[j].id })
	enrollment, ok, err := readStatusEnrollment(cfg.DataDir)
	if err != nil {
		return clusterStatus{}, err
	}
	if ok {
		report.clusterID = enrollment.ClusterID
		report.enrollmentListen = enrollment.ListenAddress
		report.pin = enrollment.CAFingerprint
		report.publicURL = enrollment.PublicURL
		report.protocol = enrollment.Protocol
		report.storageTier = storagetier.Tier(enrollment.StorageTier)
		report.cert, report.key = readStatusMemberKey(cfg.DataDir)
	}
	report.joinURL = readJoinEnrollmentURL(dbPath)
	return report, nil
}

func readPersistedRaft(dir string) (persistedRaftNode, []persistedRaftNode, error) {
	// Each refusal names the file it is about: the text is what a
	// diagnostics collection shows as the reason it holds no members.
	identity, membership := filepath.Join(dir, "info.yaml"), filepath.Join(dir, "cluster.yaml")
	info, err := readRaftNode(identity)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return persistedRaftNode{}, nil, fmt.Errorf("database identity is not in the data directory (%s)", identity)
		}
		return persistedRaftNode{}, nil, err
	}
	if info.ID == 0 || info.Address == "" {
		return persistedRaftNode{}, nil, fmt.Errorf("database identity is incomplete (%s)", identity)
	}
	raw, err := readStatusFile(membership)
	if err != nil {
		return persistedRaftNode{}, nil, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var members []persistedRaftNode
	if err := decoder.Decode(&members); err != nil {
		return persistedRaftNode{}, nil, fmt.Errorf("invalid persisted membership (%s)", membership)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return persistedRaftNode{}, nil, fmt.Errorf("invalid persisted membership (%s)", membership)
	}
	if len(members) == 0 {
		return persistedRaftNode{}, nil, fmt.Errorf("persisted membership is empty (%s)", membership)
	}
	return info, members, nil
}

func readRaftNode(path string) (persistedRaftNode, error) {
	raw, err := readStatusFile(path)
	if err != nil {
		return persistedRaftNode{}, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var info persistedRaftNode
	if err := decoder.Decode(&info); err != nil {
		return persistedRaftNode{}, fmt.Errorf("invalid database identity (%s)", path)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return persistedRaftNode{}, fmt.Errorf("invalid database identity (%s)", path)
	}
	return info, nil
}

func readStatusFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 64<<10 {
		return nil, errors.New("persisted raft file is missing or too large")
	}
	return os.ReadFile(path)
}

func readStatusEnrollment(dataDir string) (EnrollmentInfo, bool, error) {
	path := filepath.Join(dataDir, enrollmentDirectory, "config.json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return EnrollmentInfo{}, false, nil
	}
	if err != nil {
		return EnrollmentInfo{}, false, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 128<<10 {
		return EnrollmentInfo{}, false, errors.New("invalid enrollment configuration")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return EnrollmentInfo{}, false, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var settings EnrollmentInfo
	if err := decoder.Decode(&settings); err != nil {
		return EnrollmentInfo{}, false, errors.New("invalid enrollment configuration")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return EnrollmentInfo{}, false, errors.New("invalid enrollment configuration")
	}
	if tier, err := storagetier.Parse(settings.StorageTier); settings.ClusterID == "" || err != nil || tier.Recorded() != settings.StorageTier {
		return EnrollmentInfo{}, false, errors.New("invalid enrollment configuration")
	}
	return settings, true, nil
}

func persistedRaftRole(role *int) (string, bool) {
	if role == nil {
		return "not recorded", false
	}
	switch *role {
	case 0:
		return "voter", true
	case 1:
		return "stand-by", false
	case 2:
		return "spare", false
	default:
		return fmt.Sprintf("role %d", *role), false
	}
}

func (s *clusterStatus) probeLocal(ctx context.Context, httpAddr string, timeout time.Duration) {
	s.httpAddr = httpAddr
	if s.address != "" {
		s.database = classifyListener(probeDial(ctx, s.address, timeout))
	}
	if s.enrollmentListen != "" {
		s.enrollment = s.enrollmentListen + ": " + classifyListener(probeDial(ctx, s.enrollmentListen, timeout))
	}
	if httpAddr == "" {
		return
	}
	s.httpState = classifyListener(probeDial(ctx, httpAddr, timeout))
	if s.httpState != "listening" {
		return
	}
}

func probePeerDatabases(ctx context.Context, report clusterStatus, timeout time.Duration) []statusPeer {
	peers := make([]statusPeer, 0, len(report.members))
	for _, member := range report.members {
		if member.id == report.id {
			continue
		}
		peer := statusPeer{id: member.id, address: member.address, enrollment: "not observed", material: "not observed"}
		if member.address == "" {
			peer.database = "not observed"
			peers = append(peers, peer)
			continue
		}
		peer.database = tcpResult(probeDial(ctx, member.address, timeout))
		peers = append(peers, peer)
	}
	return peers
}

func probeDial(ctx context.Context, address string, timeout time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", address)
	if conn != nil {
		_ = conn.Close()
	}
	return err
}

func classifyListener(err error) string {
	switch tcpResult(err) {
	case "open":
		return "listening"
	case "connection refused":
		return "not listening"
	default:
		return tcpResult(err)
	}
}

func tcpResult(err error) string {
	if err == nil {
		return "open"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "connection refused"
	}
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return "timed out"
	}
	return "unreachable"
}

func (s clusterStatus) format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Status report\n")
	shortfall := s.membershipShortfall(s.observedAt)
	switch {
	case s.business == "open" && shortfall != "":
		// Business answers, but the next member to fail may stop it.
		fmt.Fprintf(&b, "Overall: degraded (business reads verified; %s)\n", shortfall)
	case s.business == "open":
		fmt.Fprintln(&b, "Overall: ready (business reads verified)")
	case s.httpAddr == "":
		fmt.Fprintln(&b, "Overall: unverified (supply --http-addr to check business readiness)")
	default:
		fmt.Fprintf(&b, "Overall: not ready (business: %s)\n", s.business)
	}
	fmt.Fprintf(&b, "Member: %d %s\n", s.id, s.address)
	fmt.Fprintf(&b, "Cluster: %s\n", s.clusterID)
	if s.clusterID != "" {
		tier, _ := storagetier.Parse(string(s.storageTier))
		shortest, longest := tier.ElectionTimer()
		fmt.Fprintf(&b, "Storage tier: %s (leader election after %s to %s without a leader)\n", tier, shortest, longest)
	}
	fmt.Fprintf(&b, "Role: %s (%s)\n", s.role, s.roleSource)
	fmt.Fprintf(&b, "Voters: %d\n", s.voters)
	fmt.Fprintf(&b, "Fault tolerant: %s\n", s.faultTolerance())
	fmt.Fprintf(&b, "Database directory: %s\n", s.directory)
	fmt.Fprintf(&b, "Sync: %s\n", s.sync)
	if s.httpAddr == "" {
		fmt.Fprintf(&b, "HTTP: %s\n", s.httpState)
	} else {
		fmt.Fprintf(&b, "HTTP %s: %s\n", s.httpAddr, s.httpState)
	}
	if s.enrollmentListen == "" {
		fmt.Fprintf(&b, "Enrollment: %s\n", s.enrollment)
	} else {
		fmt.Fprintf(&b, "Enrollment %s\n", s.enrollment)
	}
	fmt.Fprintf(&b, "Database %s: %s\n", s.address, s.database)
	fmt.Fprintf(&b, "Business: %s\n", s.business)
	if s.businessObservation != "" {
		fmt.Fprintf(&b, "Business observation: %s\n", s.businessObservation)
	}
	fmt.Fprintf(&b, "materials_ready: %s\n", s.materials)
	fmt.Fprintf(&b, "Leader: %s\n", s.leader)
	fmt.Fprintf(&b, "Term: %s\n", s.term)
	fmt.Fprintf(&b, "Commit index: %s\n", s.commit)
	fmt.Fprintf(&b, "Configuration index: %s\n", s.configIndex)
	fmt.Fprintf(&b, "Members:\n")
	for _, member := range s.members {
		fmt.Fprintf(&b, "  %d %s %s (%s)\n", member.id, member.address, member.role, member.source)
	}
	fmt.Fprintf(&b, "Reachability:\n")
	for _, peer := range s.peers {
		fmt.Fprintf(&b, "  %d %s database: %s enrollment: %s material route: %s\n", peer.id, peer.address, peer.database, peer.enrollment, peer.material)
	}
	return b.String()
}
