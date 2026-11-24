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

	"github.com/go-chi/chi/v5"
	"gpuworks.ai/gpuworks/internal/logging"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/diagnostics"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
)

// clusterDiagnosticsSource is where a member's part is read from. Tests
// replace it.
var clusterDiagnosticsSource = func(cfg Config) diagnostics.MemberSource { return &clusterDiagnostics{cfg: cfg} }

// clusterDiagnosticsHandler gives this member's part to a caller that
// proves a member key this member's transport policy admits, one part at a
// time. Nothing is written to this member's disk, and the listener's close
// and the member's withdrawal never wait for the part: see the comments
// below for how.
//
// local is this member's own configuration: its database path (which a
// member that formed the cluster may have chosen) says where its raft
// identity and transport policy are, its log file what the part is read
// from.
func clusterDiagnosticsHandler(bundle *enrollmentBundle, requests *materialRequestTracker, local Config, log *slog.Logger) (http.HandlerFunc, error) {
	if requests == nil || bundle == nil || bundle.trust == nil || bundle.dataDir == "" {
		return nil, errors.New("cluster diagnostics require an authenticated enrollment identity")
	}
	cfg := Config{DataDir: bundle.dataDir, DBPath: local.DBPath, LogFile: local.LogFile}
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
		// The window is checked here as well as by WriteMemberPart: after
		// the status line is sent a refusal can only cut the stream.
		if err != nil || json.Unmarshal(data, &request) != nil || !diagnostics.ValidWindow(request.From, request.To) {
			http.Error(w, "invalid cluster diagnostics request", http.StatusBadRequest)
			return
		}
		select {
		case slot <- struct{}{}:
		default:
			http.Error(w, "a diagnostics part is being given", http.StatusTooManyRequests)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), diagnostics.MemberPartTimeout())
		defer cancel()
		connection := http.NewResponseController(w)
		// The listener's own write timeout would cut a part short.
		_ = connection.SetWriteDeadline(time.Now().Add(diagnostics.MemberPartTimeout()))
		// The clock goes out before the part is read, so the asker can place
		// it against its own.
		w.Header().Set(enrollment.ClusterDiagnosticsClockHeader, time.Now().UTC().Format(time.RFC3339Nano))
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_ = connection.Flush()
		// The part is read by a goroutine of its own and handed over through
		// a pipe; this handler only copies to the connection. What can block
		// is that goroutine, inside one read of a log file or an executable
		// on a disk that does not answer. It holds no request, and gives the
		// slot back when that read returns: until then the member answers
		// 429 and is indexed as busy by whoever asks.
		reader, writer := io.Pipe()
		go func() {
			defer func() { <-slot }()
			// What the log's own goroutine still holds belongs in the part.
			logging.Flush()
			_ = writer.CloseWithError(diagnostics.WriteMemberPart(ctx, writer, source, request.From, request.To))
		}()
		// When the request or the listener's request owner is cancelled, the
		// copy below ends at once: the pipe is dropped and the write
		// deadline expired.
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

// registerClusterDiagnostics mounts the route. A member that cannot mount it
// answers 404 there, which an asker reads as a release without the route:
// the reason is logged here, so that is not its only trace.
func registerClusterDiagnostics(router chi.Router, bundle *enrollmentBundle, requests *materialRequestTracker, local Config, log *slog.Logger) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	part, err := clusterDiagnosticsHandler(bundle, requests, local, log)
	if err != nil {
		log.Error("cluster diagnostics route is not served: other members cannot collect this member's part", "err", err)
		return
	}
	router.Post(enrollment.ClusterDiagnosticsPath, part)
}

// clusterDiagnosticsAdmitted reports a key this member's transport policy
// admits now. The policy is the member's own file, refreshed from the
// database while there is a leader, so the check needs none; a key it
// revokes or does not name is refused, and so is every caller when the file
// cannot be read. While there is no leader the file is not refreshed: a
// member removed since is admitted until it is.
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
