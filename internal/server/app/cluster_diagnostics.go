//go:build linux && dqlite

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"gpuworks.ai/gpuworks/internal/logging"
	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/diagnostics"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
	"gpuworks.ai/gpuworks/internal/version"
)

// A diagnostics collection made on a cluster member covers every member.
// Each member gives its own part on its enrollment listener, read from its
// directories alone; the member that collects asks the others with its
// member certificate. Neither end needs the database, a leader or open
// business, so the members' records can be collected from a cluster that is
// not serving, by the server or by `gpuworks support-bundle` on any member.

// clusterDiagnosticsObserve bounds each probe of the status observation a
// part carries and a collection lists its members from.
const clusterDiagnosticsObserve = 3 * time.Second

// clusterDiagnostics is this installation as a cluster member, read from its
// data directory.
type clusterDiagnostics struct {
	cfg Config
}

// newClusterDiagnostics is the cluster a collection covers; nil for a data
// directory that is not a cluster member's.
func newClusterDiagnostics(cfg Config) diagnostics.Cluster {
	if member, err := selfinstall.ClusterMember(cfg.DataDir); err != nil || !member {
		return nil
	}
	return clusterDiagnostics{cfg: cfg}
}

func (c clusterDiagnostics) Self() diagnostics.MemberSource { return c }

// Members lists the members from the persisted membership, and from the
// leader's when one answers.
func (c clusterDiagnostics) Members(ctx context.Context) []diagnostics.ClusterMember {
	report, err := readClusterStatus(c.cfg)
	if err != nil {
		return nil
	}
	report.observeQuorum(ctx, c.cfg, clusterDiagnosticsObserve)
	out := make([]diagnostics.ClusterMember, 0, len(report.members))
	for _, member := range report.members {
		out = append(out, diagnostics.ClusterMember{ID: member.id, Address: member.address, Role: member.role, Self: member.id == report.id})
	}
	return out
}

// OpenMemberPart asks the member's enrollment listener. Its origin is the
// one the leader reports; with no leader to ask, it is taken to be the
// member's host on this member's own enrollment port, which is how the
// members of one installation are laid out. The pinned cluster authority and
// the member certificate's host decide whether whoever answers there is the
// member, so a wrong guess fails and never reaches another party.
func (c clusterDiagnostics) OpenMemberPart(ctx context.Context, member diagnostics.ClusterMember, from, to time.Time) (diagnostics.MemberAnswer, error) {
	none := diagnostics.MemberAnswer{}
	report, err := readClusterStatus(c.cfg)
	if err != nil {
		return none, err
	}
	if report.pin == "" || len(report.cert) == 0 || len(report.key) == 0 {
		return none, errors.New("this installation holds no cluster member identity")
	}
	report.observeQuorum(ctx, c.cfg, clusterDiagnosticsObserve)
	origin := ""
	for _, peer := range report.peers {
		if peer.id == member.ID {
			origin = peer.origin
		}
	}
	if origin == "" {
		origin = guessedEnrollmentOrigin(report, member.Address)
	}
	if !enrollment.CanonicalEnrollmentOrigin(origin) {
		return none, errors.New("the member's enrollment listener is not known")
	}
	client, err := enrollment.NewClient(origin+enrollmentCertificatePath, report.clusterID, report.pin, report.protocol)
	if err != nil {
		return none, err
	}
	answer, err := client.OpenClusterDiagnostics(ctx, report.cert, report.key, from, to)
	if err != nil {
		var status *enrollment.StatusError
		if errors.As(err, &status) && status.Code == http.StatusForbidden {
			return none, errors.Join(diagnostics.ErrMemberRefused, err)
		}
		return none, err
	}
	return diagnostics.MemberAnswer{Body: answer.Body, Clock: answer.Clock, RoundTrip: answer.RoundTrip}, nil
}

// guessedEnrollmentOrigin is the member's database host on this member's
// enrollment port.
func guessedEnrollmentOrigin(report clusterStatus, address string) string {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return ""
	}
	own := report.publicURL
	if own == "" && report.enrollmentListen != "" {
		own = "https://" + report.enrollmentListen
	}
	parsed, err := url.Parse(own)
	if err != nil || parsed.Port() == "" {
		return ""
	}
	return "https://" + net.JoinHostPort(host, parsed.Port())
}

func (c clusterDiagnostics) Facts() diagnostics.MemberFacts {
	facts := diagnostics.MemberFacts{Version: version.String(), LogFile: c.cfg.LogFile}
	if report, err := readClusterStatus(c.cfg); err == nil {
		facts.ID, facts.Address, facts.StorageTier = report.id, report.address, string(report.storageTier)
	}
	if exe, err := os.Executable(); err == nil {
		facts.ServerSHA256 = fileSHA256(exe)
	}
	if helper, err := cluster.HelperExecutable(); err == nil {
		facts.DatabaseProcessSHA256 = fileSHA256(helper)
	}
	return facts
}

func fileSHA256(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return ""
	}
	return hex.EncodeToString(sum.Sum(nil))
}

func (c clusterDiagnostics) LogFiles() []string {
	return diagnosticsSources{logFile: c.cfg.LogFile}.ServerLogFiles()
}

func (c clusterDiagnostics) StallDir() string {
	return diagnostics.StallReportDir(c.cfg.DataDir)
}

// Status is the status report as this member takes it now. The report of a
// member whose business is closed is still a report.
func (c clusterDiagnostics) Status(ctx context.Context) string {
	text, _ := ObserveClusterStatus(ctx, c.cfg, "", clusterDiagnosticsObserve)
	return text
}

// clusterDiagnosticsHandler gives this member's part to a caller that
// proves a member key this member's transport policy admits. The policy is
// the member's own file, refreshed from the database while it answers, so
// the check needs no leader; a key it revokes or does not name is refused,
// and so is every caller when the policy cannot be read.
func clusterDiagnosticsHandler(bundle *enrollmentBundle, requests *materialRequestTracker, logFile string, log *slog.Logger) (http.HandlerFunc, error) {
	if requests == nil || bundle == nil || bundle.trust == nil || bundle.dataDir == "" {
		return nil, errors.New("cluster diagnostics require an authenticated enrollment identity")
	}
	cfg := Config{DataDir: bundle.dataDir, LogFile: logFile}
	source := clusterDiagnostics{cfg: cfg}
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
		work := filepath.Join(bundle.dataDir, "diagnostics", "bundles")
		if err := os.MkdirAll(work, 0o700); err != nil {
			http.Error(w, "cluster diagnostics unavailable", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), diagnostics.MemberPartTimeout)
		defer cancel()
		// The clock goes out before the part is read, so the asker can place
		// it against its own.
		w.Header().Set(enrollment.ClusterDiagnosticsClockHeader, time.Now().UTC().Format(time.RFC3339Nano))
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_ = http.NewResponseController(w).Flush()
		// What the log's own goroutine still holds belongs in the part.
		logging.Flush()
		if err := diagnostics.WriteMemberPart(ctx, w, work, source, request.From, request.To); err != nil {
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
