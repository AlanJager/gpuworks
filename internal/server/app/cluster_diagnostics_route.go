//go:build linux && dqlite

package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"gpuworks.ai/gpuworks/internal/logging"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/diagnostics"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
)

// clusterDiagnosticsSource is where a member's part is read from. Tests
// replace it.
var clusterDiagnosticsSource = func(cfg Config) diagnostics.MemberSource { return &clusterDiagnostics{cfg: cfg} }

// clusterDiagnosticsHandler gives this member's part to a caller that
// proves a member key this member's transport policy admits. The policy is
// the member's own file, refreshed from the database while it answers, so
// the check needs no leader; a key it revokes or does not name is refused,
// and so is every caller when the policy cannot be read. With no leader the
// file is not refreshed: a member removed since is still admitted until it
// is.
//
// The part is read by a goroutine of its own and handed over through a
// pipe, and nothing is written to this member's disk. The handler itself
// only copies to the connection, under its own write deadline (the
// listener's 15 s would cut a part short), and ends as soon as the request
// or the listener's request owner is cancelled: it then drops the pipe and
// expires the write deadline. So the listener's close and the member's
// withdrawal never wait for a disk. What can still block is the reading
// goroutine, inside one read of a log file or an executable on a disk that
// does not answer; it holds no request and ends at that read's return. One
// part is given at a time; while one is being read, another asker is told
// 429.
func clusterDiagnosticsHandler(bundle *enrollmentBundle, requests *materialRequestTracker, logFile string, log *slog.Logger) (http.HandlerFunc, error) {
	if requests == nil || bundle == nil || bundle.trust == nil || bundle.dataDir == "" {
		return nil, errors.New("cluster diagnostics require an authenticated enrollment identity")
	}
	cfg := Config{DataDir: bundle.dataDir, LogFile: logFile}
	source := clusterDiagnosticsSource(cfg)
	slot := make(chan struct{}, 1)
	return requests.wrap(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "member authentication rejected", http.StatusForbidden)
			return
		}
		requester, err := bundle.trust.VerifyMember(r.TLS.PeerCertificates[0], time.Now().UTC())
		if err != nil || !clusterDiagnosticsAdmitted(cfg, bundle, requester) {
			http.Error(w, "member authentication rejected", http.StatusForbidden)
			return
		}
		var request enrollment.ClusterDiagnosticsRequest
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<10))
		if err != nil || json.Unmarshal(data, &request) != nil || !request.From.Before(request.To) || request.To.Sub(request.From) > diagnostics.MaxBundleWindow {
			http.Error(w, "invalid cluster diagnostics request", http.StatusBadRequest)
			return
		}
		select {
		case slot <- struct{}{}:
		default:
			w.Header().Set("Retry-After", "3")
			http.Error(w, "a diagnostics part is being given", http.StatusTooManyRequests)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), diagnostics.MemberPartTimeout)
		defer cancel()
		connection := http.NewResponseController(w)
		_ = connection.SetWriteDeadline(time.Now().Add(diagnostics.MemberPartTimeout))
		// The clock goes out before the part is read, so the asker can place
		// it against its own.
		w.Header().Set(enrollment.ClusterDiagnosticsClockHeader, time.Now().UTC().Format(time.RFC3339Nano))
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_ = connection.Flush()
		reader, writer := io.Pipe()
		go func() {
			defer func() { <-slot }()
			// What the log's own goroutine still holds belongs in the part.
			logging.Flush()
			_ = writer.CloseWithError(diagnostics.WriteMemberPart(ctx, writer, source, request.From, request.To))
		}()
		stop := context.AfterFunc(ctx, func() {
			_ = reader.CloseWithError(ctx.Err())
			_ = connection.SetWriteDeadline(time.Now())
		})
		defer stop()
		if _, err := io.Copy(w, reader); err != nil {
			_ = reader.CloseWithError(err)
			log.Warn("cluster diagnostics part ended early", "requester", requester, "err", err)
			// The status is sent already: end the stream short of its
			// trailer, so the asker reads the part as incomplete.
			panic(http.ErrAbortHandler)
		}
		log.Info("cluster diagnostics part given", "requester", requester, "from", request.From.UTC().Format(time.RFC3339), "to", request.To.UTC().Format(time.RFC3339))
	}), nil
}

// clusterDiagnosticsAdmitted reports a key this member's transport policy
// admits now.
func clusterDiagnosticsAdmitted(cfg Config, bundle *enrollmentBundle, key string) bool {
	path, _, err := ClusterTransportPolicyPath(cfg)
	if err != nil {
		return false
	}
	policy, err := identity.LoadTransportPolicy(path)
	if err != nil {
		return false
	}
	authorize, err := policy.Authorizer(bundle.info.ClusterID, bundle.info.CAFingerprint)
	return err == nil && authorize(key) == nil
}
