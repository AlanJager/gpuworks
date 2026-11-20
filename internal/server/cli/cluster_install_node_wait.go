package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

func clusterInstallWaitLogin(ctx context.Context, req clusterInstallNodeRequest) (string, error) {
	// Only fixed response classifications may enter retained diagnostics. In
	// particular, never retain response bodies or arbitrary transport text.
	diagnostic := func(err error) error {
		message := err.Error()
		var status int
		if n, _ := fmt.Sscanf(message, "POST returned HTTP %d (expected 200)", &status); n == 1 && status >= 100 && status <= 999 && message == fmt.Sprintf("POST returned HTTP %d (expected 200)", status) {
			return errors.New(message)
		}
		switch message {
		case "installation API returned invalid JSON", "installation API response exceeds limit", "unexpected installation login identity":
			return errors.New(message)
		default:
			return errors.New("installation login transport or response failure")
		}
	}
	var lastFailure error
	for {
		token, err := clusterInstallLogin(ctx, req)
		if err == nil {
			return token, nil
		}
		// A final request interrupted by the caller's deadline must not replace
		// a useful earlier HTTP rejection with a generic transport failure.
		if ctx.Err() == nil {
			lastFailure = diagnostic(err)
		}
		select {
		case <-ctx.Done():
			if lastFailure != nil {
				return "", errors.Join(ctx.Err(), fmt.Errorf("last installation login failure: %w", lastFailure))
			}
			return "", ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func clusterInstallWaitJoinObserved(ctx context.Context, req clusterInstallNodeRequest, observe func(context.Context) (cluster.QuorumProof, error)) error {
	// stdout is a one-time admission marker, possibly materials_ready=false.
	// Live native protocol observations avoid unregistered HTTP facts routes.
	address := net.JoinHostPort(req.Host, fmt.Sprint(req.Database))
	lastObservation := errors.New("join admission marker has not been recorded")
	for {
		pid, err := clusterInstallOwnedProcess(req, "join")
		if err != nil {
			return err
		}
		if pid == 0 {
			return errors.New("join exited before live membership observation; inspect logs/join.out")
		}
		raw, err := os.ReadFile(req.Dir + "/logs/join.out")
		if err != nil {
			return err
		}
		var admitted app.ClusterCatchUp
		for _, line := range bytes.Split(raw, []byte("\n")) {
			var value app.ClusterCatchUp
			if json.Unmarshal(line, &value) == nil && value.ID != 0 && value.Address == address && value.Role != "" {
				admitted = value
				break
			}
		}
		if admitted.ID != 0 {
			proof, observationErr := observe(ctx)
			if observationErr != nil {
				lastObservation = observationErr
			} else {
				lastObservation = errors.New("fresh native membership does not contain the admitted join identity")
			}
			current, err := clusterInstallOwnedProcess(req, "join")
			if err != nil {
				return err
			}
			if current != pid {
				return errors.New("join owner exited or changed during live membership observation")
			}
			if observationErr == nil {
				for _, member := range proof.Members {
					if member.ID == admitted.ID && member.Address == address {
						return nil
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return errors.Join(fmt.Errorf("join live membership observation deadline: %w", ctx.Err()), lastObservation)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// clusterInstallServiceConfig is the server.yaml the unit starts from. It holds
// no credential: the admin account already exists in the replicated database.
func clusterInstallServiceConfig(req clusterInstallNodeRequest) serverconfig.File {
	return serverconfig.File{
		Listen:  serverconfig.Listen{Console: []string{net.JoinHostPort(req.Host, fmt.Sprint(req.HTTP))}},
		DataDir: req.Dir + "/data",
		Log:     serverconfig.Log{File: req.Dir + "/logs/server.log", MaxSize: "10MB", Keep: 20},
	}
}

func clusterInstallService(ctx context.Context, req clusterInstallNodeRequest) error {
	// Render and publish the configuration before stopping the temporary
	// process, so a refused publication leaves the running process in place.
	configPath := serverconfig.Path(req.Dir)
	payload, err := serverconfig.Render(clusterInstallServiceConfig(req))
	if err != nil {
		return err
	}
	if err := clusterInstallPrivateFile(configPath, payload); err != nil {
		return fmt.Errorf("exclusive service configuration creation failed: %w", err)
	}
	if err := clusterInstallStop(ctx, req, "serve"); err != nil {
		return err
	}
	// The unit keeps stderr on the journal; the server also writes the log file
	// named in its configuration.
	unit := selfinstall.RenderUnit(selfinstall.UnitConfig{Binary: req.Dir + "/gpuworks", ConfigPath: configPath, DataDir: req.Dir + "/data", ServiceUser: req.User})
	path := "/etc/systemd/system/" + clusterInstallUnit(req.Dir)
	c := exec.CommandContext(ctx, "sudo", "-n", "sh", "-c", "umask 022; set -C; cat > "+clusterInstallQuote(path))
	c.Stdin = strings.NewReader(unit)
	if err := c.Run(); err != nil {
		return fmt.Errorf("exclusive service creation failed: %w", err)
	}
	for _, args := range [][]string{{"-n", "systemctl", "daemon-reload"}, {"-n", "systemctl", "enable", "--now", clusterInstallUnit(req.Dir)}} {
		if err := exec.CommandContext(ctx, "sudo", args...).Run(); err != nil {
			return fmt.Errorf("service start failed (files retained): %w", err)
		}
	}
	return nil
}

func clusterInstallLogNodeError(path string, req clusterInstallNodeRequest, err error) {
	if err == nil {
		return
	}
	message := err.Error()
	if req.Password != "" {
		message = strings.ReplaceAll(message, req.Password, "[password omitted]")
	}
	var invitation dto.ClusterInvitationResponse
	if json.Unmarshal(req.Invitation, &invitation) == nil && invitation.Token != "" {
		message = strings.ReplaceAll(message, invitation.Token, "[invitation omitted]")
	}
	f, openErr := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if openErr == nil {
		defer f.Close()
		fmt.Fprintf(f, "%s %s: %s\n", time.Now().UTC().Format(time.RFC3339), req.Action, message)
	}
}
