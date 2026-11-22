//go:build linux && dqlite

package app

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// ClusterUpgradeProgressPath is the public progress document of a cluster
// upgrade. Every member answers it on its business listener whether its
// business is open or closed, to a caller that proves nothing: during the
// upgrade window no session can be checked, because sessions are read from
// the business store. The document therefore holds counts and codes only.
const ClusterUpgradeProgressPath = "/api/v1/cluster/upgrade-progress"

// The states of the progress document. The console shows a state, it does
// not derive one.
const (
	clusterUpgradeIdle       = "idle"       // no upgrade: the schema is this executable's and the cluster serves
	clusterUpgradeWaiting    = "waiting"    // this executable is ahead of the schema and waits for the window
	clusterUpgradeWindow     = "window"     // the cluster is closed for the upgrade
	clusterUpgradeMigrating  = "migrating"  // the leader is applying migrations
	clusterUpgradeUnobserved = "unobserved" // this member could not read the upgrade facts now
)

// clusterUpgradeProgressDocument is the wire form of the progress document.
type clusterUpgradeProgressDocument struct {
	State string `json:"state"`
	// BusinessOpen says whether this member serves business requests now.
	BusinessOpen bool `json:"business_open"`
	// Schema counts migrations, core and extension modules together:
	// applied to the database, and known to the answering executable.
	Schema struct {
		Applied int `json:"applied"`
		Known   int `json:"known"`
	} `json:"schema"`
	// Members counts admitted members, those whose last report equals the
	// answering executable's migration lists, and those that repeated their
	// report recently.
	Members struct {
		Total    int `json:"total"`
		OnTarget int `json:"on_target"`
		Fresh    int `json:"fresh"`
	} `json:"members"`
}

// clusterUpgradeProgressSource reads the upgrade facts without a requester.
type clusterUpgradeProgressSource interface {
	ClusterUpgradeProgress(context.Context) (store.ClusterUpgradeStatus, bool, error)
}

// clusterUpgradeProgress answers the progress document. One observation
// serves every caller for a second, so an unauthenticated reader cannot make
// the member read its database more often than that.
type clusterUpgradeProgress struct {
	source clusterUpgradeProgressSource
	now    func() time.Time

	mu   sync.Mutex
	at   time.Time
	last clusterUpgradeProgressDocument
}

func newClusterUpgradeProgress(source clusterUpgradeProgressSource) *clusterUpgradeProgress {
	return &clusterUpgradeProgress{source: source, now: time.Now}
}

func (p *clusterUpgradeProgress) document(ctx context.Context, businessOpen bool) clusterUpgradeProgressDocument {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.at.IsZero() || p.now().Sub(p.at) >= time.Second {
		p.last = p.observe(ctx)
		p.at = p.now()
	}
	doc := p.last
	doc.BusinessOpen = businessOpen
	return doc
}

func (p *clusterUpgradeProgress) observe(ctx context.Context) clusterUpgradeProgressDocument {
	doc := clusterUpgradeProgressDocument{State: clusterUpgradeUnobserved}
	if p.source == nil {
		return doc
	}
	attempt, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	status, migrating, err := p.source.ClusterUpgradeProgress(attempt)
	if err != nil {
		return doc
	}
	doc.Schema.Applied = status.Applied + status.AppliedExtensions
	doc.Schema.Known = status.Known + status.KnownExtensions
	doc.Members.Total = len(status.Members)
	for _, member := range status.Members {
		if member.Reported && member.Markers == status.Known && member.Extensions == status.KnownExtensions {
			doc.Members.OnTarget++
		}
		if member.Fresh {
			doc.Members.Fresh++
		}
	}
	switch {
	case migrating:
		doc.State = clusterUpgradeMigrating
	case status.Phase.Phase != store.MaintenanceServing:
		doc.State = clusterUpgradeWindow
	case status.Pending() > 0:
		doc.State = clusterUpgradeWaiting
	default:
		doc.State = clusterUpgradeIdle
		// The reports are removed when an upgrade finishes; with nothing
		// pending every member that serves is on this schema.
		doc.Members.OnTarget = doc.Members.Total
	}
	return doc
}

func (p *clusterUpgradeProgress) serve(w http.ResponseWriter, r *http.Request, businessOpen bool) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := json.Marshal(p.document(r.Context(), businessOpen))
	if err != nil {
		http.Error(w, "upgrade progress unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(append(body, '\n'))
}

//go:embed cluster_upgrade_page.html
var clusterUpgradePage []byte

// wantsUpgradePage reports a browser navigation: a GET for a page, not for
// an API, a download or the inference gateway.
func wantsUpgradePage(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	path := r.URL.Path
	for _, prefix := range []string{"/api/", "/v1/", "/dl/", "/_"} {
		if strings.HasPrefix(path, prefix) {
			return false
		}
	}
	if path == "/healthz" || path == "/install.sh" {
		return false
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// serveClusterUpgradePage answers a browser that opens the console during
// the upgrade window. The console cannot be served then, so the member gives
// a page that holds everything it needs and polls the progress document. The
// status stays 503: to an entry layer this member is still not ready.
func serveClusterUpgradePage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", "5")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write(clusterUpgradePage)
}
