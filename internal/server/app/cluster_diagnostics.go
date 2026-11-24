//go:build linux && dqlite

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster"
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

// clusterDiagnosticsObservationAge is how long one observation of the
// cluster serves a collection: its members are listed and each is asked
// from the same one, instead of probing again for every member (with no
// leader each observation costs its timeouts).
const clusterDiagnosticsObservationAge = 30 * time.Second

// clusterDiagnostics is this installation as a cluster member, read from its
// data directory. It opens no database and waits for no leader: the
// membership is the persisted one, and the leader's view is added when an
// enrollment listener or a database port answers within the probe timeout.
type clusterDiagnostics struct {
	cfg Config

	mu       sync.Mutex
	observed clusterStatus
	at       time.Time
}

// newClusterDiagnostics is the cluster a collection covers; nil for a data
// directory that is not a cluster member's.
func newClusterDiagnostics(cfg Config) diagnostics.Cluster {
	if member, err := selfinstall.ClusterMember(cfg.DataDir); err != nil || !member {
		return nil
	}
	return &clusterDiagnostics{cfg: cfg}
}

func (c *clusterDiagnostics) Self() diagnostics.MemberSource { return c }

// observe reads the persisted membership and asks for the leader's view.
func (c *clusterDiagnostics) observe(ctx context.Context) (clusterStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && time.Since(c.at) < clusterDiagnosticsObservationAge {
		return c.observed, nil
	}
	report, err := readClusterStatus(c.cfg)
	if err != nil {
		return clusterStatus{}, err
	}
	report.observeQuorum(ctx, c.cfg, clusterDiagnosticsObserve)
	c.observed, c.at = report, time.Now()
	return report, nil
}

// Members lists the members from the persisted membership, and from the
// leader's when one answers.
func (c *clusterDiagnostics) Members(ctx context.Context) []diagnostics.ClusterMember {
	report, err := c.observe(ctx)
	if err != nil {
		return nil
	}
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
// the member certificate's host decide whether whoever answers there is a
// member of this cluster on that host; where the leader named the member's
// key, the certificate that answers must carry that key.
func (c *clusterDiagnostics) OpenMemberPart(ctx context.Context, member diagnostics.ClusterMember, from, to time.Time) (diagnostics.MemberAnswer, error) {
	none := diagnostics.MemberAnswer{}
	report, err := c.observe(ctx)
	if err != nil {
		return none, err
	}
	if report.pin == "" || len(report.cert) == 0 || len(report.key) == 0 {
		return none, errors.New("this installation holds no cluster member identity")
	}
	origin, key := "", ""
	for _, peer := range report.peers {
		if peer.id == member.ID {
			origin, key = peer.origin, peer.key
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
		return none, memberAnswerError(err)
	}
	if key != "" && answer.MemberKey != key {
		_ = answer.Body.Close()
		return none, fmt.Errorf("%w: %s answered with member key %s, the cluster names %s for this member", diagnostics.ErrMemberFailed, origin, answer.MemberKey, key)
	}
	return diagnostics.MemberAnswer{Body: answer.Body, Clock: answer.Clock, Sent: answer.Sent, RoundTrip: answer.RoundTrip, Key: answer.MemberKey}, nil
}

// memberAnswerError sorts a member's refusal by its status; an error that
// is no status (the member was not reached) is left as it is.
func memberAnswerError(err error) error {
	var status *enrollment.StatusError
	if !errors.As(err, &status) {
		return err
	}
	switch status.Code {
	case http.StatusForbidden:
		return errors.Join(diagnostics.ErrMemberRefused, err)
	case http.StatusTooManyRequests:
		return errors.Join(diagnostics.ErrMemberBusy, err)
	case http.StatusNotFound:
		// A member of an earlier release, as during an upgrade.
		return errors.Join(diagnostics.ErrMemberUnsupported, err)
	}
	return errors.Join(diagnostics.ErrMemberFailed, err)
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

func (c *clusterDiagnostics) Facts() diagnostics.MemberFacts {
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

// executableDigests keeps a file's digest while its size and time stand,
// so a part does not read the executables again each time.
var executableDigests sync.Map // path -> executableDigest

type executableDigest struct {
	size    int64
	changed time.Time
	sum     string
}

func fileSHA256(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if kept, ok := executableDigests.Load(path); ok {
		if d := kept.(executableDigest); d.size == info.Size() && d.changed.Equal(info.ModTime()) {
			return d.sum
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return ""
	}
	digest := hex.EncodeToString(sum.Sum(nil))
	executableDigests.Store(path, executableDigest{size: info.Size(), changed: info.ModTime(), sum: digest})
	return digest
}

func (c *clusterDiagnostics) LogFiles() []string {
	return diagnosticsSources{logFile: c.cfg.LogFile}.ServerLogFiles()
}

func (c *clusterDiagnostics) StallDir() string {
	return diagnostics.StallReportDir(c.cfg.DataDir)
}

// Status is the status report as this member takes it now. The report of a
// member whose business is closed is still a report.
func (c *clusterDiagnostics) Status(ctx context.Context) string {
	text, _ := ObserveClusterStatus(ctx, c.cfg, "", clusterDiagnosticsObserve)
	return text
}
