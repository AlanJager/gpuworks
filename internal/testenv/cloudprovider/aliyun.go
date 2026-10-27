package cloudprovider

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	serverprovider "gpuworks.ai/gpuworks/internal/server/provider"
	aliyundriver "gpuworks.ai/gpuworks/internal/server/provider/aliyun"
)

const (
	aliyunRentTimeout    = 20 * time.Minute
	aliyunBootTimeout    = 7 * time.Minute
	aliyunDestroyTimeout = 5 * time.Minute
	aliyunStockTTL       = 30 * time.Second
	aliyunPollEvery      = 4 * time.Second
	// aliyunGoneWait bounds how long Destroy waits for the instance to vanish
	// from DescribeInstances after DeleteInstance accepted the request.
	aliyunGoneWait = 90 * time.Second
	// aliyunRegionConcurrency keeps listing every region well below API
	// throttling while still finishing in seconds.
	aliyunRegionConcurrency = 6
)

// AliyunConfig configures the Aliyun ECS adapter.
type AliyunConfig struct {
	AccessKeyID     string
	AccessKeySecret string
	// Images maps a region to the golden custom image to boot there. ECS
	// images are region-scoped (CopyImage makes the per-region copies), and a
	// region without an entry is never rented in.
	Images map[string]string
	// Regions narrows the regions scanned for leaks and rented in; empty means
	// the regions of Images for renting and every region for leak scans.
	Regions []string
	// IngressCIDR is where SSH (22) and the agent port are open to; empty
	// means anywhere. VMs carry a random root password and live minutes.
	IngressCIDR string
	// NoSpot / NoOnDemand take one billing mode out (debugging, cost caps).
	NoSpot, NoOnDemand bool
	// Endpoint redirects every API call to one URL (tests).
	Endpoint string
}

// AliyunPlacement is one concrete way to buy a shape: where and how.
type AliyunPlacement struct {
	Region, Zone string
	Shape        AliyunShape
	Spot         bool
	// Stock is Alibaba Cloud's StatusCategory for the zone ("WithStock" or the
	// tighter "ClosedWithStock").
	Stock string
}

func (p AliyunPlacement) billing() string {
	if p.Spot {
		return "spot"
	}
	return "on-demand"
}

func (p AliyunPlacement) String() string {
	return fmt.Sprintf("%s/%s %s %s", p.Region, p.Zone, p.Shape.InstanceType, p.billing())
}

type aliyunStockEntry struct {
	at    time.Time
	stock map[stockKey]string
}

// Aliyun rents ECS GPU VMs for L4 cases: spot first, on-demand (PostPaid)
// when spot has no stock or is refused, always from the golden image.
type Aliyun struct {
	api    *aliyunAPI
	creds  serverprovider.Credentials
	cfg    AliyunConfig
	runID  string
	sleep  func(context.Context, time.Duration) error
	poll   time.Duration
	now    func() time.Time
	bootBy time.Duration

	mu         sync.Mutex
	stockCache map[string]aliyunStockEntry
	candidates map[string][]AliyunPlacement
	pinned     map[string]AliyunPlacement
	networks   map[string]aliyunNetwork
	regionOf   map[string]string
	attempts   []string
}

// NewAliyun builds the adapter for one test run.
func NewAliyun(cfg AliyunConfig, runID string) (*Aliyun, error) {
	return newAliyun(aliyundriver.NewRPC(), cfg, runID)
}

func newAliyun(caller aliyunCaller, cfg AliyunConfig, runID string) (*Aliyun, error) {
	creds := serverprovider.Credentials{
		PublicKey: strings.TrimSpace(cfg.AccessKeyID), PrivateKey: strings.TrimSpace(cfg.AccessKeySecret),
		Endpoint: strings.TrimSpace(cfg.Endpoint),
	}
	if creds.PublicKey == "" || creds.PrivateKey == "" {
		return nil, errors.New("aliyun access key ID and secret are required")
	}
	if strings.TrimSpace(runID) == "" {
		var err error
		if runID, err = NewRunID(); err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(cfg.IngressCIDR) == "" {
		cfg.IngressCIDR = "0.0.0.0/0"
	}
	images := map[string]string{}
	for region, image := range cfg.Images {
		if region, image = strings.TrimSpace(region), strings.TrimSpace(image); region != "" && image != "" {
			images[region] = image
		}
	}
	cfg.Images = images
	return &Aliyun{
		api: &aliyunAPI{caller: caller, creds: creds}, creds: creds, cfg: cfg, runID: runID,
		sleep: sleepCtx, poll: aliyunPollEvery, now: time.Now, bootBy: aliyunBootTimeout,
		stockCache: map[string]aliyunStockEntry{}, candidates: map[string][]AliyunPlacement{},
		pinned: map[string]AliyunPlacement{}, networks: map[string]aliyunNetwork{}, regionOf: map[string]string{},
	}, nil
}

func (p *Aliyun) Name() string { return "aliyun" }

// RunID is the owner label of every resource this adapter creates.
func (p *Aliyun) RunID() string { return p.runID }

// ManagementCredentials hands the AccessKey to the product's Aliyun driver
// (the cases install the agent through it, as an operator would). The regions
// field limits its inventory sync to where the tests rent.
func (p *Aliyun) ManagementCredentials() serverprovider.Credentials {
	creds := p.creds
	if regions := p.rentRegions(); len(regions) > 0 {
		creds.Extra = map[string]string{"regions": strings.Join(regions, ",")}
	}
	return creds
}

// Attempts is the log of every create the adapter tried, in order, with the
// outcome. Cases print it so a spot refusal followed by an on-demand fallback
// is visible in the job log.
func (p *Aliyun) Attempts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.attempts...)
}

func (p *Aliyun) note(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.attempts = append(p.attempts, fmt.Sprintf(format, args...))
}

// rentRegions are the regions a VM can be rented in: those with a golden image.
func (p *Aliyun) rentRegions() []string {
	allowed := map[string]bool{}
	for _, region := range p.cfg.Regions {
		allowed[strings.TrimSpace(region)] = true
	}
	var regions []string
	for region := range p.cfg.Images {
		if len(allowed) == 0 || allowed[region] {
			regions = append(regions, region)
		}
	}
	sort.Strings(regions)
	return regions
}

// scanRegions are the regions leak scans cover, and whether the list is the
// operator's own (a failing region then fails the scan) or derived from the
// account (a region the key cannot use is skipped).
func (p *Aliyun) scanRegions(ctx context.Context) ([]string, bool, error) {
	if explicit := p.rentRegions(); len(explicit) > 0 {
		// Leaks can only be in regions a run rents in, but a run-scoped cleanup
		// without the image map must still find them: add the explicit list.
		return explicit, true, nil
	}
	if len(p.cfg.Regions) > 0 {
		return append([]string(nil), p.cfg.Regions...), true, nil
	}
	regions, err := p.api.regions(ctx)
	return regions, false, err
}

// Rent buys one VM: the first placement that works, spot before on-demand. A
// second Rent for the same card and GPU count stays in the zone and instance
// type of the first (two-VM cases need one private network and one shape),
// trying spot first there too and on-demand when spot is refused.
func (p *Aliyun) Rent(ctx context.Context, spec GPUSpec) (*Instance, error) {
	if len(p.rentRegions()) == 0 {
		return nil, errors.New("aliyun: no golden image is configured for any region (ALIYUN_IMAGE_<REGION>)")
	}
	ctx, cancel := withMaxTimeout(ctx, aliyunRentTimeout)
	defer cancel()
	gpuCount := spec.GPUCount
	if gpuCount <= 0 {
		gpuCount = 1
	}
	diskGB := spec.DiskGB
	if diskGB <= 0 {
		diskGB = 100
	}
	models := AliyunGPUModels()
	if strings.TrimSpace(spec.GPUModel) != "" {
		model, ok := canonicalAliyunModel(spec.GPUModel)
		if !ok {
			return nil, fmt.Errorf("aliyun: unknown GPU %q (known: %s)", spec.GPUModel, strings.Join(AliyunGPUModels(), ", "))
		}
		models = []string{model}
	}
	var refused []string
	for _, model := range models {
		if vram, _ := AliyunGPUVRAMMB(model); spec.MinVRAMMB > 0 && vram < spec.MinVRAMMB {
			refused = append(refused, fmt.Sprintf("%s: %d MiB per card, below the requested %d MiB", model, vram, spec.MinVRAMMB))
			continue
		}
		candidates, err := p.placementsToTry(ctx, model, gpuCount, spec)
		if err != nil {
			refused = append(refused, model+": "+err.Error())
			continue
		}
		for _, candidate := range candidates {
			instance, err := p.create(ctx, candidate, diskGB)
			if err == nil {
				p.mu.Lock()
				p.pinned[aliyunPlacementKey(model, gpuCount)] = candidate
				p.mu.Unlock()
				return instance, nil
			}
			if !isAliyunCapacityError(err) && !errors.Is(err, errAliyunBootFailed) {
				return nil, err
			}
			refused = append(refused, fmt.Sprintf("%s: %v", candidate, err))
		}
	}
	return nil, fmt.Errorf("no aliyun capacity could be rented for %s x%d: %s", orAny(spec.GPUModel), gpuCount, strings.Join(refused, "; "))
}

func orAny(model string) string {
	if strings.TrimSpace(model) == "" {
		return "any GPU"
	}
	return model
}

// placementsToTry orders what Rent attempts for one card: only the pinned
// zone and type once a first VM exists, otherwise the live candidate list.
func (p *Aliyun) placementsToTry(ctx context.Context, model string, gpuCount int, spec GPUSpec) ([]AliyunPlacement, error) {
	key := aliyunPlacementKey(model, gpuCount)
	p.mu.Lock()
	pinned, hasPinned := p.pinned[key]
	list := append([]AliyunPlacement(nil), p.candidates[key]...)
	p.mu.Unlock()
	if hasPinned && spec.Region == "" && spec.Zone == "" {
		var out []AliyunPlacement
		for _, spot := range []bool{pinned.Spot, !pinned.Spot} {
			if (spot && (p.cfg.NoSpot || spec.OnDemand)) || (!spot && p.cfg.NoOnDemand) {
				continue
			}
			again := pinned
			again.Spot = spot
			out = append(out, again)
		}
		return out, nil
	}
	if len(list) == 0 {
		if _, err := p.SelectPlacement(ctx, model, gpuCount); err != nil {
			return nil, err
		}
		p.mu.Lock()
		list = append([]AliyunPlacement(nil), p.candidates[key]...)
		p.mu.Unlock()
	}
	var out []AliyunPlacement
	for _, candidate := range list {
		if candidate.Spot && spec.OnDemand {
			continue
		}
		if spec.Region != "" && candidate.Region != spec.Region {
			continue
		}
		if spec.Zone != "" && candidate.Zone != spec.Zone {
			continue
		}
		out = append(out, candidate)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no aliyun capacity: no placement matches region=%q zone=%q on-demand=%t", spec.Region, spec.Zone, spec.OnDemand)
	}
	return out, nil
}

var errAliyunBootFailed = errors.New("aliyun instance did not become reachable")

func (p *Aliyun) network(ctx context.Context, region, zone string) (aliyunNetwork, error) {
	cacheKey := region + "/" + zone
	p.mu.Lock()
	cached, ok := p.networks[cacheKey]
	p.mu.Unlock()
	if ok {
		return cached, nil
	}
	// Concurrent jobs of one account share this network and create it with the
	// same idempotency tokens: the loser of a race gets
	// OperationFailed.IdempotentTokenProcessing while the winner's create is
	// still running. Looking again a moment later finds the finished resource.
	var network aliyunNetwork
	for attempt := 0; ; attempt++ {
		var err error
		if network, err = p.buildNetwork(ctx, region, zone); err == nil {
			break
		} else if aliyundriver.ErrorCode(err) != "OperationFailed.IdempotentTokenProcessing" || attempt >= 10 {
			return aliyunNetwork{}, err
		}
		if sleepErr := p.sleep(ctx, 3*p.poll); sleepErr != nil {
			return aliyunNetwork{}, sleepErr
		}
	}
	p.mu.Lock()
	p.networks[cacheKey] = network
	p.mu.Unlock()
	return network, nil
}

func (p *Aliyun) buildNetwork(ctx context.Context, region, zone string) (aliyunNetwork, error) {
	vpcID, err := p.api.ensureVPC(ctx, region)
	if err != nil {
		return aliyunNetwork{}, err
	}
	groupID, err := p.api.ensureSecurityGroup(ctx, region, vpcID, p.cfg.IngressCIDR)
	if err != nil {
		return aliyunNetwork{}, err
	}
	vswitchID, err := p.api.ensureVSwitch(ctx, region, zone, vpcID)
	if err != nil {
		return aliyunNetwork{}, err
	}
	return aliyunNetwork{VPCID: vpcID, VSwitchID: vswitchID, SecurityGroupID: groupID}, nil
}

// create buys one VM at a placement and waits for it to be reachable.
func (p *Aliyun) create(ctx context.Context, placement AliyunPlacement, diskGB int) (*Instance, error) {
	network, err := p.network(ctx, placement.Region, placement.Zone)
	if err != nil {
		return nil, err
	}
	password, err := generatePassword()
	if err != nil {
		return nil, err
	}
	token, err := NewRunID()
	if err != nil {
		return nil, err
	}
	id, err := p.api.runInstance(ctx, aliyunRunRequest{
		Region: placement.Region, Zone: placement.Zone, InstanceType: placement.Shape.InstanceType,
		ImageID: p.cfg.Images[placement.Region], VSwitchID: network.VSwitchID, SecurityGroupID: network.SecurityGroupID,
		Name: TestNamePrefix + p.runID, Password: password, RunID: p.runID, DiskGB: diskGB,
		Spot: placement.Spot, ClientToken: token,
	})
	if err != nil {
		p.note("%s: refused: %v", placement, err)
		return nil, err
	}
	p.mu.Lock()
	p.regionOf[id] = placement.Region
	p.mu.Unlock()
	p.note("%s: created %s", placement, id)

	bootCtx, bootCancel := context.WithTimeout(ctx, p.bootBy)
	defer bootCancel()
	running, err := p.waitRunning(bootCtx, placement.Region, id)
	if err != nil {
		p.note("%s: %s did not become reachable: %v", placement, id, err)
		destroyCtx, destroyCancel := context.WithTimeout(context.Background(), aliyunDestroyTimeout)
		defer destroyCancel()
		if destroyErr := p.Destroy(destroyCtx, id); destroyErr != nil {
			return nil, fmt.Errorf("%w: %v (and releasing %s failed: %v)", errAliyunBootFailed, err, id, destroyErr)
		}
		return nil, fmt.Errorf("%w: %v", errAliyunBootFailed, err)
	}
	return &Instance{
		InstanceID: id, GPUModel: placement.Shape.GPUModel, GPUCount: placement.Shape.GPUCount, Kind: "vm",
		Region: placement.Region, PublicIP: running.publicIP(), PrivateIP: running.privateIP(),
		SSHPort: 22, Username: "root", Password: password,
		Zone: placement.Zone, InstanceType: placement.Shape.InstanceType, Billing: placement.billing(),
	}, nil
}

// waitRunning polls until the instance is Running with both addresses. A spot
// instance reclaimed while booting disappears or stops; that ends the wait
// with a clear error instead of running out the clock.
func (p *Aliyun) waitRunning(ctx context.Context, region, id string) (aliyunInstance, error) {
	missing := 0
	for {
		found, err := p.api.describeInstances(ctx, region, []string{id}, "")
		switch {
		case err != nil:
			missing = 0
		case len(found) == 0:
			// A just-created instance can lag behind the create; after several
			// consecutive misses it is gone (reclaimed or deleted).
			missing++
			if missing >= 5 {
				return aliyunInstance{}, fmt.Errorf("instance %s disappeared while booting (spot capacity reclaimed?)", id)
			}
		default:
			missing = 0
			instance := found[0]
			if instance.Status == "Running" && instance.publicIP() != "" && instance.privateIP() != "" {
				return instance, nil
			}
			if instance.Status == "Stopped" || instance.reclaiming() {
				return aliyunInstance{}, fmt.Errorf("instance %s is %s while booting (spot capacity reclaimed?)", id, instance.Status)
			}
		}
		if sleepErr := p.sleep(ctx, p.poll); sleepErr != nil {
			return aliyunInstance{}, fmt.Errorf("wait for instance %s to run: %w (last error: %v)", id, sleepErr, err)
		}
	}
}

// locate finds the region of an instance, scanning when the adapter did not
// create it itself (cleanup of an earlier process's leak).
func (p *Aliyun) locate(ctx context.Context, id string) (string, error) {
	p.mu.Lock()
	region, ok := p.regionOf[id]
	p.mu.Unlock()
	if ok {
		return region, nil
	}
	regions, explicit, err := p.scanRegions(ctx)
	if err != nil {
		return "", err
	}
	for _, region := range regions {
		found, err := p.api.describeInstances(ctx, region, []string{id}, "")
		if err != nil {
			if explicit {
				return "", fmt.Errorf("look for %s in %s: %w", id, region, err)
			}
			continue
		}
		if len(found) > 0 {
			p.mu.Lock()
			p.regionOf[id] = region
			p.mu.Unlock()
			return region, nil
		}
	}
	return "", nil
}

// Destroy releases the VM with its system disk and public IP and waits until
// it is gone. An unknown ID is not an error (already released).
func (p *Aliyun) Destroy(ctx context.Context, instanceID string) error {
	ctx, cancel := withMaxTimeout(ctx, aliyunDestroyTimeout)
	defer cancel()
	region, err := p.locate(ctx, instanceID)
	if err != nil {
		return err
	}
	if region == "" {
		return nil
	}
	for {
		err = p.api.deleteInstance(ctx, region, instanceID)
		code := aliyundriver.ErrorCode(err)
		if err == nil || strings.HasPrefix(code, "InvalidInstanceId.NotFound") {
			break
		}
		// An instance still initializing its guest OS right after it started
		// (IncorrectInstanceStatus.Initializing, observed 2026-10-25 up to a
		// minute after Running) refuses deletion, Force or not. Keep trying
		// until the context ends.
		if !strings.HasPrefix(code, "IncorrectInstanceStatus") {
			return fmt.Errorf("delete aliyun instance %s: %w", instanceID, err)
		}
		if sleepErr := p.sleep(ctx, p.poll); sleepErr != nil {
			return fmt.Errorf("delete aliyun instance %s: %w (last error: %v)", instanceID, sleepErr, err)
		}
	}
	goneCtx, goneCancel := context.WithTimeout(ctx, aliyunGoneWait)
	defer goneCancel()
	for {
		found, err := p.api.describeInstances(goneCtx, region, []string{instanceID}, "")
		if err == nil && len(found) == 0 {
			return nil
		}
		if sleepErr := p.sleep(goneCtx, p.poll); sleepErr != nil {
			return fmt.Errorf("aliyun instance %s is still listed after delete: %w", instanceID, sleepErr)
		}
	}
}
