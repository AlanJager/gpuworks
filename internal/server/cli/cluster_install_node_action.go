//go:build linux && dqlite

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// clusterInstallNodeActionLimit bounds one member-side action when the client
// that started it is gone. The first start of a member applies the whole
// migration history as replicated commits, each waiting for a majority to
// sync, which takes minutes on slow storage; the client's own --timeout is
// the deadline an operator sets.
const clusterInstallNodeActionLimit = 15 * time.Minute

func clusterInstallNodeAction(ctx context.Context, req clusterInstallNodeRequest, output io.Writer) (resultErr error) {
	if err := validateClusterInstallDir(req.Dir); err != nil {
		return err
	}
	account, err := user.Current()
	if err != nil || account.Username != req.User {
		return errors.New("installation account differs from SSH account")
	}
	root, err := filepath.EvalSymlinks(req.Dir)
	if err != nil {
		return err
	}
	if root != req.Dir {
		return errors.New("installation directory contains a symlink")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if self != filepath.Join(root, "gpuworks") {
		return errors.New("node helper must run from its copied installation executable")
	}
	marker := filepath.Join(root, "cluster-install.json")
	if req.Action == "prepare" {
		entries, err := os.ReadDir(root)
		if err != nil {
			return err
		}
		if !clusterInstallFreshEntries(entries) {
			return errors.New("prepare requires only the freshly copied executables")
		}
		defer func() { clusterInstallLogNodeError(root+"/install-preflight-errors.log", req, resultErr) }()
		if err := clusterInstallProcessSupport(); err != nil {
			return err
		}
		saved := req
		saved.Password = ""
		saved.Invitation = nil
		saved.Action = ""
		saved.ClusterID = ""
		saved.CAFingerprint = ""
		raw, _ := json.Marshal(saved)
		if err := clusterInstallPrivateFile(marker, raw); err != nil {
			return err
		}
		if err := os.Mkdir(filepath.Join(root, "data"), 0700); err != nil {
			return err
		}
		if err := os.Mkdir(filepath.Join(root, "logs"), 0700); err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(map[string]bool{"prepared": true})
	}
	var saved clusterInstallNodeRequest
	raw, err := os.ReadFile(marker)
	if err != nil {
		return errors.New("installation ownership marker is missing")
	}
	if err = json.Unmarshal(raw, &saved); err != nil {
		return err
	}
	if saved.Dir != req.Dir || saved.Host != req.Host || saved.User != req.User || saved.HTTP != req.HTTP || saved.Enrollment != req.Enrollment || saved.Database != req.Database || saved.NoService != req.NoService || strings.Join(saved.Hosts, ",") != strings.Join(req.Hosts, ",") {
		return errors.New("installation ownership settings differ")
	}
	// Only an authenticated ownership marker allows retaining operation errors
	// inside this directory. Password/invitation values are redacted even in
	// the private diagnostic log.
	defer func() { clusterInstallLogNodeError(root+"/logs/install-node-errors.log", req, resultErr) }()
	ctx, cancel := context.WithTimeout(ctx, clusterInstallNodeActionLimit)
	defer cancel()
	var result any = map[string]bool{"completed": true}
	switch req.Action {
	case "seed":
		entries, err := os.ReadDir(filepath.Join(root, "data"))
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			return errors.New("seed requires empty data")
		}
		if err := clusterInstallStartContext(ctx, req, false, nil); err != nil {
			return err
		}
		if _, err := clusterInstallWaitLogin(ctx, req); err != nil {
			return err
		}
		if err := clusterInstallStop(ctx, req, "serve"); err != nil {
			return err
		}
		var buffer bytes.Buffer
		enrollment := net.JoinHostPort(req.Host, fmt.Sprint(req.Enrollment))
		if err := clusterInitCommand(ctx, []string{"--offline", "--data", root + "/data", "--db", root + "/data/gpuworks.db", "--listen", enrollment, "--public-url", "https://" + enrollment, "--storage-tier", requestStorageTier(req)}, &buffer); err != nil {
			return err
		}
		if err := json.Unmarshal(buffer.Bytes(), &result); err != nil {
			return err
		}
		if err := clusterAddressCommand(ctx, []string{"--offline", "--data", root + "/data", "--db", root + "/data/gpuworks.db", "--address", net.JoinHostPort(req.Host, fmt.Sprint(req.Database))}, io.Discard); err != nil {
			return err
		}
		if err := clusterInstallStartContext(ctx, req, false, nil); err != nil {
			return err
		}
		if _, err := clusterInstallWaitLogin(ctx, req); err != nil {
			return err
		}
	case "invite":
		token, err := clusterInstallLogin(ctx, req)
		if err != nil {
			return err
		}
		var target struct {
			Host string `json:"host"`
		}
		if err := json.Unmarshal(req.Invitation, &target); err != nil {
			return err
		}
		if ip := net.ParseIP(target.Host); ip == nil || ip.To4() == nil {
			return errors.New("invalid invitation target")
		}
		invitation, err := clusterInstallInvite(ctx, req, token, target.Host)
		if err != nil {
			return err
		}
		result = invitation
	case "join":
		var invitation dto.ClusterInvitationResponse
		if err := json.Unmarshal(req.Invitation, &invitation); err != nil {
			return err
		}
		if invitation.ClusterID != req.ClusterID || invitation.CAFingerprint != req.CAFingerprint || invitation.Token == "" || invitation.ClusterID == "" || invitation.CAFingerprint == "" || invitation.Protocol != proto.ClusterProtocolVersion || invitation.EnrollmentURL == "" || invitation.AuthorizedEnrollmentOrigin != "https://"+net.JoinHostPort(req.Host, fmt.Sprint(req.Enrollment)) {
			return errors.New("invalid invitation identity or authorized origin")
		}
		tokenPath := root + "/invite.token"
		if err := clusterInstallPrivateFile(tokenPath, []byte(invitation.Token)); err != nil {
			return err
		}
		args := []string{"cluster-join", "--data", root + "/data", "--db", root + "/data/gpuworks.db", "--address", net.JoinHostPort(req.Host, fmt.Sprint(req.Database)), "--cluster-id", invitation.ClusterID, "--ca-fingerprint", invitation.CAFingerprint, "--enrollment-url", invitation.EnrollmentURL, "--public-url", invitation.AuthorizedEnrollmentOrigin, "--public-listen", net.JoinHostPort(req.Host, fmt.Sprint(req.Enrollment)), "--token-file", tokenPath, "--timeout", "5m"}
		if err := clusterInstallStartContext(ctx, req, true, args); err != nil {
			return err
		}
		if err := clusterInstallWaitJoin(ctx, req); err != nil {
			return err
		}
	case "serve-joined":
		if err := clusterInstallStop(ctx, req, "join"); err != nil {
			return err
		}
		if err := clusterInstallStartContext(ctx, req, false, nil); err != nil {
			return err
		}
		if _, err := clusterInstallWaitLogin(ctx, req); err != nil {
			return err
		}
		// Admission has consumed the invitation; remove only this owned file.
		if err := os.Remove(root + "/invite.token"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	case "service":
		if !req.NoService {
			if err := clusterInstallService(ctx, req); err != nil {
				return err
			}
			if _, err := clusterInstallWaitLogin(ctx, req); err != nil {
				return err
			}
		}
	case "bootstrap":
		if err := clusterInstallWaitMaterialBootstrap(ctx, func(ctx context.Context) error {
			return clusterInstallMaterialBootstrap(ctx, req)
		}); err != nil {
			return err
		}
		result = map[string]bool{"bootstrap_ready": true}
	case "status":
		proof, err := clusterInstallObserveNative(ctx, req, false)
		if err != nil {
			return err
		}
		if !clusterInstallThreeVoters(req, proof) {
			return errors.New("unexpected independently observed native voter membership")
		}
		var buffer bytes.Buffer
		if err := statusCommand(ctx, []string{"--data", root + "/data", "--db", root + "/data/gpuworks.db", "--http-addr", net.JoinHostPort(req.Host, fmt.Sprint(req.HTTP)), "--timeout", "5s"}, &buffer); err != nil {
			return err
		}
		report := buffer.String()
		if req.ClusterID == "" || !strings.Contains(report, "Cluster: "+req.ClusterID+"\n") {
			return errors.New("unexpected cluster identity in status")
		}

		for _, line := range []string{"Overall: ready (business reads verified)\n", "Business: open\n"} {
			if !strings.Contains(report, line) {
				return errors.New("three-voter business status not ready")
			}
		}
		result = map[string]any{"status": report, "native": proof}
	case "business", "read-setting":
		var setting struct{ Key, Value string }
		if err := json.Unmarshal(req.Invitation, &setting); err != nil {
			return err
		}
		if !strings.HasPrefix(setting.Key, "cluster_install_") {
			return errors.New("invalid installation verification setting")
		}
		if err := clusterInstallBusiness(ctx, req, setting.Key, setting.Value, req.Action == "business"); err != nil {
			return err
		}
	default:
		return errors.New("unknown cluster installation action")
	}
	return json.NewEncoder(output).Encode(result)
}
