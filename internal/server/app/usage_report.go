package app

import (
	"context"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/distribution"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/usagereport"
	"gpuworks.ai/gpuworks/internal/server/uxstore"
	"gpuworks.ai/gpuworks/internal/version"
)

// usageReporter is the user experience improvement program: once a day it
// sends one report of the previous UTC day (docs/reference/usage-report.md)
// to the publisher, and the console can preview it and switch it off.
type usageReporter struct {
	store    *store.Store
	ux       *uxstore.Store // nil: no gateway traffic is counted
	settings *serverSettings
	sender   *usagereport.Sender
	log      *slog.Logger
	now      func() time.Time
	counters usagereport.Counters

	// kick wakes the scheduler when the program is switched on, so the first
	// report goes out at once instead of at the next idle check.
	kick chan struct{}

	mu   sync.Mutex
	last *dto.UsageReportAttempt
	// failedDay is a day whose report no endpoint took; it is not tried
	// again, the next day's report carries its in-process counts instead.
	failedDay string
}

var _ api.UsageReport = (*usageReporter)(nil)

// newImprovementProgram builds the daily usage report and the notice bell.
// Both reach out through channel's client: the report to its endpoints, the
// notices to the distribution sources.
func newImprovementProgram(st *store.Store, ux *uxstore.Store, settings *serverSettings, channel *distribution.Client, log *slog.Logger) (*usageReporter, *noticeService) {
	usage := &usageReporter{store: st, ux: ux, settings: settings, log: log, now: time.Now,
		sender: &usagereport.Sender{HTTP: channel.HTTP}, kick: make(chan struct{}, 1)}
	notices := &noticeService{store: st, client: func() *distribution.Client { return channel }, settings: settings,
		log: log, now: time.Now, started: time.Now().UTC()}
	return usage, notices
}

// usageIdle is how often the scheduler looks again while nothing is due, so
// switching the program on or off, or a clock change, is noticed in time.
const usageIdle = time.Hour

// usageDeliverTimeout bounds one delivery, every endpoint included.
const usageDeliverTimeout = 30 * time.Second

func (u *usageReporter) Counters() *usagereport.Counters { return &u.counters }

func (u *usageReporter) enabled() bool {
	on, _ := u.settings.UsageReport()
	return on
}

func (u *usageReporter) Status() dto.UsageReportStatus {
	on, editable := u.settings.UsageReport()
	out := dto.UsageReportStatus{Enabled: on, Editable: editable}
	out.LastDay, _ = u.store.UsageReportLastDay()
	u.mu.Lock()
	if u.last != nil {
		last := *u.last
		out.LastAttempt = &last
	}
	failed := u.failedDay
	u.mu.Unlock()
	if on && out.LastDay != "" {
		_, due := usagereport.Next(u.settings.InstallID(), laterDay(out.LastDay, failed), u.now())
		out.NextDueAt = &due
	}
	return out
}

// SetEnabled saves the switch. Switching the program on forgets the last
// day reported, so a report goes out at once, as on a first start.
func (u *usageReporter) SetEnabled(on bool) error {
	was := u.enabled()
	if err := u.settings.SetUsageReport(on); err != nil {
		return err
	}
	if on && !was {
		if err := u.store.SetUsageReportLastDay(""); err != nil {
			return err
		}
		u.mu.Lock()
		u.failedDay = ""
		u.mu.Unlock()
		select {
		case u.kick <- struct{}{}:
		default:
		}
	}
	return nil
}

// Preview builds the report that is due next from what is known now,
// without taking the in-process counts it shows.
func (u *usageReporter) Preview(ctx context.Context) ([]byte, error) {
	last, err := u.store.UsageReportLastDay()
	if err != nil {
		return nil, err
	}
	now := u.now()
	day, _ := usagereport.Next(u.settings.InstallID(), last, now)
	report, err := u.build(ctx, day, now, u.counters.Peek())
	if err != nil {
		return nil, err
	}
	return usagereport.Encode(report)
}

// Run sends each day's report when it is due until ctx ends.
func (u *usageReporter) Run(ctx context.Context) {
	for {
		wait := u.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		case <-u.kick:
		}
	}
}

// tick sends the report if one is due and says how long to wait before
// looking again.
func (u *usageReporter) tick(ctx context.Context) time.Duration {
	if !u.enabled() {
		// What happened while the program was off is never reported.
		u.counters.Drain()
		return usageIdle
	}
	installID := u.settings.InstallID()
	if installID == "" {
		return usageIdle
	}
	last, err := u.store.UsageReportLastDay()
	if err != nil {
		u.log.Debug("usage report: read the last day", "err", err)
		return usageIdle
	}
	now := u.now()
	u.mu.Lock()
	failed := u.failedDay
	u.mu.Unlock()
	day, due := usagereport.Next(installID, laterDay(last, failed), now)
	// A first start, or the program switched back on, reports at once (the
	// day before); after that, one report a day when it is due.
	if last != "" && now.Before(due) {
		return min(due.Sub(now), usageIdle)
	}
	u.send(ctx, day, now)
	return time.Minute
}

func (u *usageReporter) send(ctx context.Context, day, now time.Time) {
	name := day.Format(usagereport.DayLayout)
	drained := u.counters.Drain()
	report, err := u.build(ctx, day, now, drained)
	if err == nil {
		deliverCtx, cancel := context.WithTimeout(ctx, usageDeliverTimeout)
		_, _, err = u.sender.Send(deliverCtx, report)
		cancel()
	}
	attempt := &dto.UsageReportAttempt{At: now.UTC(), Day: name, OK: err == nil}
	if err != nil {
		attempt.Error = err.Error()
		u.counters.Restore(drained)
		u.log.Debug("usage report not delivered", "day", name, "err", err)
	} else if err := u.store.SetUsageReportLastDay(name); err != nil {
		u.log.Debug("usage report: record the day", "day", name, "err", err)
	}
	u.mu.Lock()
	u.last = attempt
	if !attempt.OK {
		u.failedDay = name
	}
	u.mu.Unlock()
}

// laterDay is the later of two days in DayLayout ("" is earliest).
func laterDay(a, b string) string {
	if b > a {
		return b
	}
	return a
}

// build assembles the report of day from the database, the gateway's request
// counts and the in-process counts.
func (u *usageReporter) build(ctx context.Context, day, now time.Time, counts map[string][]usagereport.Count) (*usagereport.Report, error) {
	since, until := usagereport.Window(day)
	facts, err := u.store.UsageFacts(since, until, now)
	if err != nil {
		return nil, err
	}
	channel, _ := u.settings.UpdateChannel()
	r := &usagereport.Report{
		Schema: usagereport.Schema, InstallID: u.settings.InstallID(), Day: day.Format(usagereport.DayLayout),
		Server: usagereport.Server{Version: version.Version, Channel: channel, OS: runtime.GOOS, Arch: runtime.GOARCH,
			SchemaVersion: facts.SchemaVersion},
	}
	environmentOf(&r.Environment, facts)
	usageOf(&r.Usage, facts)
	tasksOf(&r.Tasks, facts, since, until)
	r.Errors = usagereport.Errors{
		Events: namedCounts(facts.Events), ImagesFailed: facts.ImagesFailed, ModelFilesFailed: facts.ModelFilesFailed,
		APIErrors: counts[usagereport.KindAPIError], Panics: counts[usagereport.KindPanic], Protocol: counts[usagereport.KindProtocol],
	}
	r.Usage.Routes = counts[usagereport.KindRoute]
	r.Usage.Logins = counts[usagereport.KindLogin]
	if u.ux != nil {
		byDeployment, err := u.ux.RequestsByDeployment(ctx, since.UnixMilli(), until.UnixMilli()-1)
		if err != nil {
			return nil, err
		}
		engines := map[uint]string{}
		for _, d := range facts.Deployments {
			engines[d.ID] = d.Engine
		}
		var gateway []usagereport.Count
		for id, n := range byDeployment {
			gateway = append(gateway, usagereport.Count{Name: orUnknown(engines[id]), N: int(n)})
		}
		r.Usage.GatewayRequests = gateway
	}
	return r, nil
}

func environmentOf(e *usagereport.Environment, f store.UsageFacts) {
	add := func(c *usagereport.Counts, name string) {
		*c = append(*c, usagereport.Count{Name: orUnknown(name), N: 1})
	}
	for _, n := range f.Nodes {
		e.Nodes++
		if n.State == store.NodeStateOnline {
			e.OnlineNodes++
		}
		add(&e.AgentVersions, n.AgentVersion)
		add(&e.OS, n.OS)
		add(&e.Kernels, usagereport.MajorMinor(n.Kernel))
		add(&e.Arch, n.Arch)
		if n.GPUVendor != "" {
			add(&e.Vendors, n.GPUVendor)
			add(&e.Drivers, joinNonEmpty(n.GPUVendor, usagereport.Major(n.GPUDriver)))
		}
		if n.Framework != "" {
			add(&e.Frameworks, joinNonEmpty(n.Framework, usagereport.MajorMinor(n.FrameworkVersion)))
		}
		add(&e.Runtimes, n.RuntimeDriver)
		mode := n.ConnectMode
		if mode == "" {
			mode = "agent_dial"
		}
		add(&e.ConnectModes, mode)
		switch {
		case n.Cloud:
			add(&e.Onboarding, "cloud")
		case n.SSH:
			add(&e.Onboarding, "ssh")
		default:
			add(&e.Onboarding, "agent")
		}
	}
	for _, c := range f.Cards {
		switch {
		case c.Ownership != "":
			e.CardOwnership = append(e.CardOwnership, usagereport.Count{Name: c.Ownership, N: c.Count})
		default:
			e.GPUModels = append(e.GPUModels, usagereport.Count{Name: orUnknown(c.Name), N: c.Count})
		}
		if c.HealthCode != "" {
			e.CardHealth = append(e.CardHealth, usagereport.Count{Name: c.HealthCode, N: c.Count})
		}
	}
	e.Providers = namedCounts(f.Providers)
	e.Storages = namedCounts(f.Storages)
}

// modelHubs are the model sources whose repository IDs are public names, and
// the host a credential for each is kept under.
var modelHubs = map[string]string{"huggingface": "huggingface.co", "modelscope": "modelscope.cn"}

func usageOf(u *usagereport.Usage, f store.UsageFacts) {
	tokens := map[string]bool{}
	for _, host := range f.TokenHosts {
		tokens[host] = true
	}
	add := func(c *usagereport.Counts, name string) {
		*c = append(*c, usagereport.Count{Name: orUnknown(name), N: 1})
	}
	for _, d := range f.Deployments {
		u.Deployments++
		add(&u.DeploymentEngines, d.Engine)
		add(&u.DeploymentStates, d.State)
		if d.NNodes > 1 {
			u.MultiNode++
		}
		if d.Sleepable {
			u.Sleepable++
		}
		if d.PartitionBackend != "" {
			add(&u.DGPU, d.PartitionBackend)
		}
		source := d.TemplateSource
		if source == "" {
			source = "none"
		}
		add(&u.TemplateSources, source)
		if d.TemplateSource == "upstream" && d.TemplateModel != "" {
			add(&u.Templates, d.TemplateModel)
		}
		add(&u.Models, publicModelName(d, tokens))
	}
	u.PD = f.PDDeployments
	u.InstanceStates = namedCounts(f.InstanceStates)
}

// publicModelName names a deployment's model when its repository is
// certainly public: a built-in template's, or one from a hub the platform
// holds no credential for (every download from it was anonymous, so the
// repository is public). Anything else is "custom".
func publicModelName(d store.UsageDeployment, tokens map[string]bool) string {
	host, hub := modelHubs[d.ModelSource]
	if !hub || d.ModelSourceID == "" {
		return "custom"
	}
	if d.TemplateSource == "upstream" || !tokens[host] {
		return d.ModelSource + ":" + d.ModelSourceID
	}
	return "custom"
}

func tasksOf(t *usagereport.Tasks, f store.UsageFacts, since, until time.Time) {
	for _, d := range f.Deployments {
		if !d.CreatedAt.Before(since) && d.CreatedAt.Before(until) {
			t.DeploymentsCreated = append(t.DeploymentsCreated, usagereport.Count{Name: orUnknown(d.Engine), N: 1})
		}
	}
	type life struct {
		created       time.Time
		ready, failed bool
	}
	started := map[string]*life{}
	for _, e := range f.Lifecycle {
		if e.Created {
			if !e.At.Before(since) && e.At.Before(until) {
				started[e.InstanceUUID] = &life{created: e.At}
			}
			continue
		}
		l := started[e.InstanceUUID]
		if l == nil || l.ready || l.failed {
			continue
		}
		switch e.State {
		case proto.InstRunning:
			l.ready = true
			t.ReadySeconds = append(t.ReadySeconds, usagereport.Count{Name: usagereport.DurationBucket(e.At.Sub(l.created)), N: 1})
		case proto.InstFailed:
			l.failed = true
		}
	}
	for _, l := range started {
		t.InstancesStarted++
		if l.ready {
			t.InstancesReady++
		}
		if l.failed {
			t.InstancesFailed++
		}
	}
	for _, m := range f.ModelFiles {
		t.ModelFiles = append(t.ModelFiles, usagereport.Count{Name: joinNonEmpty(orUnknown(m.Acquisition), orUnknown(m.State)), N: 1})
		if m.Acquisition == "download" {
			t.ModelBytes = append(t.ModelBytes, usagereport.Count{Name: usagereport.SizeBucket(m.SizeBytes), N: 1})
		}
	}
	for _, b := range f.Benchmarks {
		dataset := b.DatasetKind
		if dataset == "" {
			dataset = "random"
		}
		t.Benchmarks = append(t.Benchmarks, usagereport.Count{Name: strings.Join([]string{orUnknown(b.Mode), dataset, orUnknown(b.State)}, " "), N: 1})
	}
}

func namedCounts(in []store.NameCount) usagereport.Counts {
	out := make(usagereport.Counts, 0, len(in))
	for _, c := range in {
		out = append(out, usagereport.Count{Name: orUnknown(c.Name), N: c.Count})
	}
	return out
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return usagereport.Unknown
	}
	return s
}

func joinNonEmpty(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " ")
}
