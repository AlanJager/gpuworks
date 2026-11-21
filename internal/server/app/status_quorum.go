//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
)

const enrollmentCertificatePath = "/api/v1/cluster/enrollment/certificate"

// statusQuorumFetch is the live leader read. Tests replace it with one snapshot.
var statusQuorumFetch = fetchStatusQuorum

func (s *clusterStatus) observeQuorum(ctx context.Context, cfg Config, timeout time.Duration) {
	facts, ok := statusQuorumFetch(ctx, *s, timeout)
	if !ok {
		// No enrollment listener gave the leader's facts; the database
		// processes still know their leader.
		facts, ok = statusQuorumProofRead(ctx, cfg, *s, timeout)
	}
	if !ok {
		return
	}
	s.applyQuorumFacts(facts)
	s.fillPeerOrigins(ctx, facts, timeout)
}

func fetchStatusQuorum(ctx context.Context, s clusterStatus, timeout time.Duration) (enrollment.ClusterQuorumFacts, bool) {
	if s.clusterID == "not recorded" || s.pin == "" || len(s.cert) == 0 || len(s.key) == 0 {
		return enrollment.ClusterQuorumFacts{}, false
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	selfOrigin := s.publicURL
	if selfOrigin == "" && s.enrollmentListen != "" {
		selfOrigin = "https://" + s.enrollmentListen
	}
	for _, endpoint := range s.quorumEndpoints() {
		if callCtx.Err() != nil {
			return enrollment.ClusterQuorumFacts{}, false
		}
		client, err := enrollment.NewClient(endpoint, s.clusterID, s.pin, s.protocol)
		if err != nil {
			continue
		}
		facts, err := client.FetchObservedQuorumFacts(callCtx, s.cert, s.key)
		if err != nil && endpoint == selfOrigin+enrollmentCertificatePath {
			// A follower's local admission view supplies explicit HTTPS voter
			// hints. Bind that contact to our durable key, then require a fresh
			// separately authenticated leader observation before using its facts.
			if ownKey, keyErr := statusMemberKeyFingerprint(s.cert, s.key); keyErr == nil {
				facts, err = client.DiscoverClusterQuorumFacts(callCtx, s.cert, s.key, []string{ownKey})
			}
		}
		client.Close()
		if err == nil {
			return facts, true
		}
	}
	return enrollment.ClusterQuorumFacts{}, false
}

func statusMemberKeyFingerprint(cert, key []byte) (string, error) {
	if _, err := tls.X509KeyPair(cert, key); err != nil {
		return "", err
	}
	csr, err := identity.NewMemberCSR(key)
	if err != nil {
		return "", err
	}
	return identity.CSRKeyFingerprint(csr)
}

func (s clusterStatus) quorumEndpoints() []string {
	var endpoints []string
	add := func(origin string) {
		if !enrollment.CanonicalEnrollmentOrigin(origin) {
			return
		}
		endpoint := origin + enrollmentCertificatePath
		for _, existing := range endpoints {
			if existing == endpoint {
				return
			}
		}
		endpoints = append(endpoints, endpoint)
	}
	if s.publicURL != "" {
		add(s.publicURL)
	} else if s.enrollmentListen != "" {
		add("https://" + s.enrollmentListen)
	}
	if origin, ok := originFromEnrollmentURL(s.joinURL); ok {
		add(origin)
	}
	return endpoints
}

func originFromEnrollmentURL(endpoint string) (string, bool) {
	if strings.HasSuffix(endpoint, enrollmentCertificatePath) {
		origin := strings.TrimSuffix(endpoint, enrollmentCertificatePath)
		if enrollment.CanonicalEnrollmentOrigin(origin) {
			return origin, true
		}
	}
	if enrollment.CanonicalEnrollmentOrigin(endpoint) {
		return endpoint, true
	}
	return "", false
}

func (s *clusterStatus) applyQuorumFacts(facts enrollment.ClusterQuorumFacts) {
	voters := 0
	selfRole := ""
	var leaderAddress string
	for _, member := range facts.Members {
		if member.Role == "voter" {
			voters++
		}
		if member.ID == s.id {
			selfRole = member.Role
		}
		if member.ID == facts.LeaderID {
			leaderAddress = member.Address
		}
	}
	if selfRole != "" {
		s.role = selfRole
		s.roleSource = "live raft"
		s.faultTolerant = voters >= 3 && selfRole == "voter"
	} else {
		s.faultTolerant = false
	}
	s.voters = voters
	if s.sync != "not replicating" {
		s.sync = "observed"
	}
	s.leader = strconv.FormatUint(facts.LeaderID, 10)
	if leaderAddress != "" {
		s.leader += " " + leaderAddress
	}
	if facts.LeaderID == s.id {
		s.leader += " (self)"
	}
	s.term = strconv.FormatUint(facts.Term, 10)
	s.commit = strconv.FormatUint(facts.Index, 10)
	s.configIndex = strconv.FormatUint(facts.ConfigurationIndex, 10)
	live := make(map[uint64]enrollment.ClusterMember, len(facts.Members))
	for _, member := range facts.Members {
		live[member.ID] = member
	}
	for i, member := range s.members {
		got, ok := live[member.id]
		if !ok {
			continue
		}
		s.members[i].role = got.Role
		if got.Address != "" {
			s.members[i].address = got.Address
		}
		s.members[i].source = "live raft"
		delete(live, member.id)
	}
	for _, member := range live {
		s.members = append(s.members, statusMember{id: member.ID, address: member.Address, role: member.Role, source: "live raft"})
	}
	sort.Slice(s.members, func(i, j int) bool { return s.members[i].id < s.members[j].id })
}

func (s *clusterStatus) fillPeerOrigins(ctx context.Context, facts enrollment.ClusterQuorumFacts, timeout time.Duration) {
	index := make(map[uint64]int, len(s.peers))
	for i, peer := range s.peers {
		index[peer.id] = i
	}
	for _, member := range facts.Members {
		if member.ID == s.id {
			continue
		}
		i, ok := index[member.ID]
		if !ok {
			// A member the cache does not list yet has not been probed.
			s.peers = append(s.peers, statusPeer{id: member.ID, database: "not observed", enrollment: "not observed", material: "not observed"})
			i = len(s.peers) - 1
			index[member.ID] = i
		}
		if member.Address != "" && member.Address != s.peers[i].address {
			s.peers[i].address = member.Address
			s.peers[i].database = tcpResult(probeDial(ctx, member.Address, timeout))
		}
		if member.EnrollmentOrigin == "" {
			continue
		}
		s.peers[i].enrollment, s.peers[i].material = probeEnrollmentOrigin(ctx, member.EnrollmentOrigin, s.clusterID, s.pin, s.cert, s.key, timeout)
	}
	sort.Slice(s.peers, func(i, j int) bool { return s.peers[i].id < s.peers[j].id })
}

func probeEnrollmentOrigin(ctx context.Context, origin, clusterID, pin string, cert, key []byte, timeout time.Duration) (string, string) {
	if !enrollment.CanonicalEnrollmentOrigin(origin) {
		return "not observed", "not observed"
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return "not observed", "not observed"
	}
	port := parsed.Port()
	if port == "" {
		port = "443"
	}
	address := net.JoinHostPort(parsed.Hostname(), port)
	switch tcpResult(probeDial(ctx, address, timeout)) {
	case "connection refused":
		return "refused", "not observed"
	case "timed out":
		return "timed out", "not observed"
	case "unreachable":
		return "unreachable", "not observed"
	}
	return probeMaterialHTTPS(ctx, origin, parsed.Hostname(), clusterID, pin, cert, key, timeout)
}

func probeMaterialHTTPS(ctx context.Context, origin, host, clusterID, pin string, cert, key []byte, timeout time.Duration) (string, string) {
	tlsConfig, err := identity.EnrollmentTLSConfig(clusterID, pin, host)
	if err != nil || len(cert) == 0 {
		return "reached process", "not observed"
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return "reached process", "not observed"
	}
	tlsConfig.Certificates = []tls.Certificate{pair}
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true, TLSClientConfig: tlsConfig, ResponseHeaderTimeout: timeout},
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(callCtx, http.MethodGet, origin+materialReplicaPath+"/"+strings.Repeat("0", 64), nil)
	if err != nil {
		return "reached process", "not observed"
	}
	response, err := client.Do(request)
	if err != nil {
		return "reached process", "not observed"
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 128))
	if err != nil {
		return "reached process", "not observed"
	}
	return "reached process", classifyMaterialProbe(response.StatusCode, body)
}

func classifyMaterialProbe(status int, body []byte) string {
	text := strings.Join(strings.Fields(string(body)), " ")
	if len(text) > 80 {
		text = text[:80]
	}
	if status == http.StatusNotFound && (text == "" || strings.Contains(text, "404 page not found")) {
		if text == "" {
			return "HTTP 404"
		}
		return "HTTP 404 " + text
	}
	return "registered"
}

func readStatusMemberKey(dataDir string) ([]byte, []byte) {
	cert, err := readPrivateStatusFile(filepath.Join(dataDir, enrollmentDirectory, "server.pem"))
	if err != nil {
		return nil, nil
	}
	key, err := readPrivateStatusFile(filepath.Join(dataDir, enrollmentDirectory, "server.key"))
	if err != nil {
		return nil, nil
	}
	return cert, key
}

func readPrivateStatusFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() <= 0 || info.Size() > 128<<10 {
		return nil, errors.New("enrollment key is missing or exposed")
	}
	return os.ReadFile(path)
}

func readJoinEnrollmentURL(dbPath string) string {
	raw, err := readPrivateStatusFile(dbPath + ".join/config.json")
	if err != nil {
		return ""
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var info ClusterJoinInfo
	if err := decoder.Decode(&info); err != nil {
		return ""
	}
	return info.EnrollmentURL
}
