//go:build linux && dqlite

package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"time"

	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

// Direct native observations work on joining listeners that deliberately expose
// no HTTP quorum-facts route. They do not open SQL, assign roles, or grant any
// material/business authority; ordinary serve and business checks follow.
func clusterInstallObserveNative(ctx context.Context, req clusterInstallNodeRequest, joining bool) (cluster.QuorumProof, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	dial, finalPolicy, _, err := clusterInstallObserverTransport(req, joining)
	if err != nil {
		return cluster.QuorumProof{}, err
	}
	contact := req
	if joining {
		// Native LEADER replies intentionally contain no hint on a spare.
		// The initial seed is already a voter; use its authenticated hint
		// until all joins finish, then ordinary serve can contact itself.
		if len(req.Hosts) == 0 {
			return cluster.QuorumProof{}, errors.New("joining native observation requires the initial seed")
		}
		contact.Host = req.Hosts[0]
	}
	return clusterInstallReadNative(ctx, contact, dial, finalPolicy)
}

func clusterInstallReadNative(ctx context.Context, req clusterInstallNodeRequest, dial func(context.Context, string) (net.Conn, string, error), finalPolicy func() error) (cluster.QuorumProof, error) {
	own := net.JoinHostPort(req.Host, fmt.Sprint(req.Database))
	leader, leaderKey, err := cluster.ObserveRaftLeader(ctx, own, dial)
	if err != nil {
		return cluster.QuorumProof{}, err
	}
	proof, err := cluster.ReadQuorumProof(ctx, leader.Address, func(ctx context.Context, target string) (net.Conn, error) {
		if target != leader.Address {
			return nil, errors.New("native proof changed its pinned leader address")
		}
		connection, key, err := dial(ctx, target)
		if err == nil && key != leaderKey {
			err = errors.New("native leader key changed between independent observations")
		}
		return connection, err
	})
	if err != nil {
		return cluster.QuorumProof{}, err
	}
	if proof.LeaderID != leader.ID {
		return cluster.QuorumProof{}, errors.New("native leader changed during observation")
	}
	if err := finalPolicy(); err != nil {
		return cluster.QuorumProof{}, err
	}
	return proof, nil
}

func clusterInstallThreeVoters(req clusterInstallNodeRequest, proof cluster.QuorumProof) bool {
	if proof.Term == 0 || proof.Index == 0 || proof.ConfigurationIndex == 0 || proof.LeaderID == 0 || len(proof.Members) != 3 {
		return false
	}
	expected := map[string]bool{}
	for _, host := range req.Hosts {
		expected[net.JoinHostPort(host, fmt.Sprint(req.Database))] = true
	}
	ids := map[uint64]bool{}
	for _, member := range proof.Members {
		if member.ID == 0 || ids[member.ID] || member.Role != "voter" || !expected[member.Address] {
			return false
		}
		ids[member.ID] = true
		delete(expected, member.Address)
	}
	return len(expected) == 0 && ids[proof.LeaderID]
}

// This transport borrows only persisted identity files; it starts no runtime.
func clusterInstallObserverTransport(req clusterInstallNodeRequest, joining bool) (func(context.Context, string) (net.Conn, string, error), func() error, string, error) {
	data := req.Dir + "/data"
	policyPath, joinedIdentity, err := app.ClusterTransportPolicyPath(app.Config{DataDir: data, DBPath: data + "/gpuworks.db"})
	if err != nil {
		return nil, nil, "", err
	}
	if joining && joinedIdentity == nil {
		return nil, nil, "", errors.New("joining native observation requires a persisted join identity")
	}
	if joinedIdentity != nil && (joinedIdentity.ClusterID != req.ClusterID || joinedIdentity.CAFingerprint != req.CAFingerprint || joinedIdentity.Address != net.JoinHostPort(req.Host, fmt.Sprint(req.Database))) {
		return nil, nil, "", errors.New("persisted joined transport identity differs from the installation")
	}
	policy, err := identity.LoadTransportPolicy(policyPath)
	if err != nil {
		return nil, nil, "", err
	}
	ca, err := os.ReadFile(data + "/cluster-enrollment/ca.pem")
	if err != nil {
		return nil, nil, "", err
	}
	cert, err := os.ReadFile(data + "/cluster-enrollment/server.pem")
	if err != nil {
		return nil, nil, "", err
	}
	key, err := identity.LoadMemberKey(data + "/cluster-enrollment/server.key")
	if err != nil {
		return nil, nil, "", err
	}
	trust, err := identity.NewTrust(ca, req.ClusterID, req.CAFingerprint, time.Now().UTC())
	if err != nil {
		return nil, nil, "", err
	}
	authorize, err := trust.PolicyAuthorizer(policy)
	if err != nil {
		return nil, nil, "", err
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return nil, nil, "", err
	}
	local, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, nil, "", err
	}
	localKey, err := trust.VerifyMember(local, time.Now().UTC())
	if err != nil {
		return nil, nil, "", err
	}
	if joinedIdentity != nil && joinedIdentity.KeyFingerprint != localKey {
		return nil, nil, "", errors.New("joined enrollment key differs from its persisted join identity")
	}
	if err := authorize(localKey); err != nil {
		return nil, nil, "", err
	}
	_, config, err := trust.TLSConfigs(cert, key, time.Now().UTC(), authorize)
	if err != nil {
		return nil, nil, "", err
	}
	allowed := map[string]bool{}
	for _, host := range req.Hosts {
		allowed[net.JoinHostPort(host, fmt.Sprint(req.Database))] = true
	}
	dial := func(ctx context.Context, address string) (net.Conn, string, error) {
		if !allowed[address] {
			return nil, "", errors.New("native observation refused address outside this installation")
		}
		connection, err := (&tls.Dialer{Config: config.Clone()}).DialContext(ctx, "tcp", address)
		if err != nil {
			return nil, "", err
		}
		state := connection.(*tls.Conn).ConnectionState()
		peerKey, err := trust.VerifyMember(state.PeerCertificates[0], time.Now().UTC())
		if err != nil {
			_ = connection.Close()
			return nil, "", err
		}
		if address == net.JoinHostPort(req.Host, fmt.Sprint(req.Database)) && peerKey != localKey {
			_ = connection.Close()
			return nil, "", errors.New("local native endpoint key differs from this installation member")
		}
		return connection, peerKey, nil
	}
	finalPolicy := func() error {
		current, err := identity.LoadTransportPolicy(policyPath)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(policy, current) {
			return errors.New("transport policy changed during native observation")
		}
		return nil
	}
	return dial, finalPolicy, localKey, nil
}

func clusterInstallWaitJoin(ctx context.Context, req clusterInstallNodeRequest) error {
	return clusterInstallWaitJoinObserved(ctx, req, func(ctx context.Context) (cluster.QuorumProof, error) {
		return clusterInstallObserveNative(ctx, req, true)
	})
}
