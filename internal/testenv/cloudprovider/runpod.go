package cloudprovider

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	serverprovider "gpuworks.ai/gpuworks/internal/server/provider"
	runpoddriver "gpuworks.ai/gpuworks/internal/server/provider/runpod"
)

const (
	runpodRentTimeout     = 8 * time.Minute
	runpodEndpointTimeout = 90 * time.Second
	runpodDestroyTimeout  = 2 * time.Minute
)

type providerDriver interface {
	ListInstances(context.Context, serverprovider.Credentials) ([]serverprovider.Instance, error)
	CreateInstance(context.Context, serverprovider.Credentials, serverprovider.CreateInstanceRequest) (serverprovider.Instance, error)
	DeleteInstance(context.Context, serverprovider.Credentials, string) error
	ListSSHKeys(context.Context, serverprovider.Credentials) ([]string, error)
	ReplaceSSHKeys(context.Context, serverprovider.Credentials, []string) error
}

type instanceGetter interface {
	GetInstance(context.Context, serverprovider.Credentials, string) (serverprovider.Instance, error)
}

type Runpod struct {
	driver     providerDriver
	creds      serverprovider.Credentials
	runID      string
	manual     *Instance
	mu         sync.Mutex
	activeKeys map[string]string
}

// RunPod replaces the account's complete SSH-key collection on every PUT.
// Serialize read-modify-write operations across all test providers so one
// test cannot remove another test's key.
var runpodSSHKeysMu sync.Mutex

func NewRunpod(apiKey, runID string) (*Runpod, error) {
	return newRunpod(runpoddriver.New(), serverprovider.Credentials{APIKey: strings.TrimSpace(apiKey)}, runID)
}

// NewRunpodManual creates a non-owning adapter for a Pod started by a user.
// It performs the same SSH/agent/tunnel checks but never destroys the Pod.
func NewRunpodManual(instanceID, publicIP string, sshPort int, username string, privateKey []byte) (*Runpod, error) {
	if strings.TrimSpace(instanceID) == "" || strings.TrimSpace(publicIP) == "" || sshPort <= 0 || len(privateKey) == 0 {
		return nil, fmt.Errorf("runpod manual SSH requires pod id, public IP, SSH port, and private key")
	}
	if strings.TrimSpace(username) == "" {
		username = "root"
	}
	runID, err := NewRunID()
	if err != nil {
		return nil, err
	}
	return &Runpod{runID: runID, manual: &Instance{InstanceID: instanceID, PublicIP: publicIP, SSHPort: sshPort, Username: username, PrivateKey: append([]byte(nil), privateKey...)}, activeKeys: make(map[string]string)}, nil
}

func newRunpod(driver providerDriver, creds serverprovider.Credentials, runID string) (*Runpod, error) {
	if strings.TrimSpace(creds.APIKey) == "" {
		return nil, fmt.Errorf("runpod API key is required")
	}
	if strings.TrimSpace(runID) == "" {
		var err error
		runID, err = NewRunID()
		if err != nil {
			return nil, err
		}
	}
	return &Runpod{driver: driver, creds: creds, runID: runID, activeKeys: make(map[string]string)}, nil
}

func (p *Runpod) Name() string { return "runpod" }

func (p *Runpod) RunID() string { return p.runID }

func (p *Runpod) ManagementCredentials() serverprovider.Credentials { return p.creds }

func (p *Runpod) Rent(ctx context.Context, spec GPUSpec) (*Instance, error) {
	if p.manual != nil {
		instance := *p.manual
		instance.PrivateKey = append([]byte(nil), p.manual.PrivateKey...)
		return &instance, nil
	}
	ctx, cancel := withMaxTimeout(ctx, runpodDuration("RUNPOD_RENT_TIMEOUT", runpodRentTimeout))
	defer cancel()
	if strings.TrimSpace(spec.GPUModel) == "" || strings.EqualFold(strings.TrimSpace(spec.GPUModel), "auto") {
		models := append([]string(nil), runpodGPUAllowlist...)
		var failures []string
		for _, model := range models {
			candidate := spec
			candidate.GPUModel = model
			instance, err := p.rent(ctx, candidate)
			if err == nil {
				return instance, nil
			}
			failures = append(failures, model+": "+err.Error())
		}
		return nil, fmt.Errorf("runpod: no preferred GPU candidate available (%s)", strings.Join(failures, "; "))
	}
	return p.rent(ctx, spec)
}

func (p *Runpod) rent(ctx context.Context, spec GPUSpec) (*Instance, error) {
	privateKey, publicKey, err := GenerateSSHKey()
	if err != nil {
		return nil, err
	}
	gpuCount := spec.GPUCount
	if gpuCount <= 0 {
		gpuCount = 1
	}
	diskGB := spec.DiskGB
	if diskGB <= 0 {
		diskGB = 40
	}
	if spec.MinVRAMMB > 0 {
		known, ok := gpuVRAMMB(spec.GPUModel)
		if !ok {
			return nil, fmt.Errorf("runpod: cannot verify MinVRAMMB=%d for unknown GPU model %q", spec.MinVRAMMB, spec.GPUModel)
		}
		if known < spec.MinVRAMMB {
			return nil, fmt.Errorf("runpod: GPU %q has %d MiB VRAM, below requested %d MiB", spec.GPUModel, known, spec.MinVRAMMB)
		}
	}
	if err := p.addSSHKey(ctx, publicKey); err != nil {
		return nil, err
	}
	created, err := p.driver.CreateInstance(ctx, p.creds, serverprovider.CreateInstanceRequest{
		Name: TestNamePrefix + p.runID, GPUModel: NormalizeGPUModel(spec.GPUModel), GPUCount: gpuCount,
		Extra: map[string]string{
			"disk_gb": strconv.Itoa(diskGB), "ports": "22/tcp",
			"env.GPUWORKS_TEST": "true", "env.GPUWORKS_TEST_RUN_ID": p.runID,
		},
	})
	if err != nil {
		_ = p.removeSSHKey(context.Background(), string(publicKey))
		return nil, err
	}
	failed := true
	defer func() {
		if failed {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), runpodDestroyTimeout)
			defer cleanupCancel()
			_ = p.driver.DeleteInstance(cleanupCtx, p.creds, created.ExternalID)
			_ = p.removeSSHKey(context.Background(), string(publicKey))
		}
	}()
	instance := created
	lastObserved := created
	var lastRefreshErr error
	endpointCtx, endpointCancel := withMaxTimeout(ctx, runpodDuration("RUNPOD_ENDPOINT_TIMEOUT", runpodEndpointTimeout))
	defer endpointCancel()
	for instance.PublicIP == "" || instance.SSHPort == 0 || (spec.MinVRAMMB > 0 && instance.GPUModel == "") {
		candidate, refreshErr := p.refreshInstance(endpointCtx, created.ExternalID)
		if refreshErr == nil {
			lastRefreshErr = nil
			lastObserved = candidate
			if candidate.Power == serverprovider.PowerStopped {
				return nil, fmt.Errorf("runpod: pod %s stopped before exposing programmable SSH (gpu=%q)", created.ExternalID, candidate.GPUModel)
			}
			instance = candidate
		} else {
			lastRefreshErr = refreshErr
		}
		if instance.PublicIP != "" && instance.SSHPort != 0 && (spec.MinVRAMMB <= 0 || instance.GPUModel != "") {
			break
		}
		select {
		case <-endpointCtx.Done():
			return nil, fmt.Errorf("wait for runpod SSH endpoint: %w (%s)", endpointCtx.Err(), runpodEndpointDetail(created.ExternalID, lastObserved, lastRefreshErr))
		case <-time.After(5 * time.Second):
		}
	}
	p.mu.Lock()
	p.activeKeys[instance.ExternalID] = strings.TrimSpace(string(publicKey))
	p.mu.Unlock()
	if want := NormalizeGPUModel(spec.GPUModel); instance.GPUModel != "" && spec.MinVRAMMB > 0 {
		if vram, ok := gpuVRAMMB(instance.GPUModel); !ok {
			_ = p.Destroy(context.Background(), instance.ExternalID)
			return nil, fmt.Errorf("runpod: could not verify created GPU VRAM for model %q (requested %q)", instance.GPUModel, want)
		} else if vram < spec.MinVRAMMB {
			_ = p.Destroy(context.Background(), instance.ExternalID)
			return nil, fmt.Errorf("runpod: created GPU %q has %d MiB VRAM, below requested %d MiB (requested %q)", instance.GPUModel, vram, spec.MinVRAMMB, want)
		}
	} else if spec.MinVRAMMB > 0 {
		_ = p.Destroy(context.Background(), instance.ExternalID)
		return nil, fmt.Errorf("runpod: created pod did not report a GPU model; cannot verify MinVRAMMB=%d", spec.MinVRAMMB)
	}
	failed = false
	return &Instance{
		InstanceID: instance.ExternalID,
		GPUModel:   instance.GPUModel,
		GPUCount:   instance.GPUCount,
		Kind:       instance.Kind,
		Region:     instance.Region,
		PublicIP:   instance.PublicIP,
		SSHPort:    instance.SSHPort,
		Username:   firstNonEmpty(instance.SSHUser, "root"),
		PrivateKey: privateKey,
	}, nil
}

func (p *Runpod) refreshInstance(ctx context.Context, id string) (serverprovider.Instance, error) {
	refreshCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if getter, ok := p.driver.(instanceGetter); ok {
		if instance, err := getter.GetInstance(refreshCtx, p.creds, id); err == nil {
			return instance, nil
		}
	}
	instances, err := p.driver.ListInstances(refreshCtx, p.creds)
	if err != nil {
		return serverprovider.Instance{}, err
	}
	for _, instance := range instances {
		if instance.ExternalID == id {
			return instance, nil
		}
	}
	return serverprovider.Instance{}, fmt.Errorf("runpod: pod %s not found while waiting for SSH endpoint", id)
}

func runpodEndpointDetail(id string, instance serverprovider.Instance, refreshErr error) string {
	detail := fmt.Sprintf("pod=%s power=%s gpu=%q public_ip=%q ssh_port=%d", id, instance.Power, instance.GPUModel, instance.PublicIP, instance.SSHPort)
	if refreshErr != nil {
		detail += ", last_refresh_error=" + refreshErr.Error()
	}
	return detail
}

func runpodDuration(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

func (p *Runpod) Destroy(ctx context.Context, instanceID string) error {
	if p.manual != nil {
		return nil
	}
	ctx, cancel := withMaxTimeout(ctx, runpodDestroyTimeout)
	defer cancel()
	err := p.driver.DeleteInstance(ctx, p.creds, instanceID)
	if err == nil {
		if key := p.activeKey(instanceID); key != "" {
			if keyErr := p.removeSSHKey(ctx, key); err == nil {
				err = keyErr
			}
		}
	}
	return err
}

func (p *Runpod) addSSHKey(ctx context.Context, publicKey []byte) error {
	runpodSSHKeysMu.Lock()
	defer runpodSSHKeysMu.Unlock()
	keys, err := p.driver.ListSSHKeys(ctx, p.creds)
	if err != nil {
		return fmt.Errorf("runpod: list SSH keys: %w", err)
	}
	key := strings.TrimSpace(string(publicKey))
	keys = appendUnique(keys, key)
	if err := p.driver.ReplaceSSHKeys(ctx, p.creds, keys); err != nil {
		return fmt.Errorf("runpod: register SSH key: %w", err)
	}
	return nil
}

// ForgetAccountSSHKey removes a key the product added to the account. Each
// L4 run starts a server with a new platform key, which the RunPod driver
// appends to the account SSH keys; without this the account would collect
// one key per run.
func (p *Runpod) ForgetAccountSSHKey(ctx context.Context, publicKey string) error {
	return p.removeSSHKey(ctx, publicKey)
}

func (p *Runpod) activeKey(instanceID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.activeKeys[instanceID]
}

func (p *Runpod) removeSSHKey(ctx context.Context, key string) error {
	runpodSSHKeysMu.Lock()
	defer runpodSSHKeysMu.Unlock()
	key = strings.TrimSpace(key)
	keys, err := p.driver.ListSSHKeys(ctx, p.creds)
	if err != nil {
		return fmt.Errorf("runpod: list SSH keys for restore: %w", err)
	}
	filtered := keys[:0]
	for _, existing := range keys {
		if strings.TrimSpace(existing) != key {
			filtered = append(filtered, existing)
		}
	}
	if err := p.driver.ReplaceSSHKeys(ctx, p.creds, filtered); err != nil {
		return fmt.Errorf("runpod: restore SSH keys: %w", err)
	}
	p.mu.Lock()
	for id, active := range p.activeKeys {
		if active == key {
			delete(p.activeKeys, id)
		}
	}
	p.mu.Unlock()
	return nil
}

func appendUnique(keys []string, key string) []string {
	for _, existing := range keys {
		if existing == key {
			return keys
		}
	}
	return append(keys, key)
}

// GPUVRAMMB is the per-card memory of a GPU model spelling, when known.
func GPUVRAMMB(model string) (int, bool) { return gpuVRAMMB(model) }

func gpuVRAMMB(model string) (int, bool) {
	canonical := strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "_", "").Replace(strings.TrimSpace(model)))
	switch canonical {
	case "PRO6000MIG24GB", "NVIDIAPRO6000MIG24GB", "RTXPRO6000BLACKWELLMIG24GB", "NVIDIARTXPRO6000BLACKWELLMIG24GB":
		return 24576, true
	case "RTXPRO6000BLACKWELLSERVEREDITIONMIG1G.24GB", "NVIDIARTXPRO6000BLACKWELLSERVEREDITIONMIG1G.24GB":
		return 24576, true
	case "RTXPRO4000BLACKWELL", "NVIDIARTXPRO4000BLACKWELL":
		return 24576, true
	case "RTXA5000", "NVIDIARTXA5000":
		return 24576, true
	case "RTXA6000", "NVIDIARTXA6000":
		return 49152, true
	case "A40", "NVIDIAA40":
		return 49152, true
	case "L4", "NVIDIAL4":
		return 24576, true
	case "L40", "NVIDIAL40", "L40S", "NVIDIAL40S":
		return 49152, true
	case "RTX2000ADA", "NVIDIARTX2000ADAGENERATION":
		return 16384, true
	case "RTX4000ADA", "NVIDIARTX4000ADAGENERATION":
		return 20480, true
	case "RTX4080S", "NVIDIAGEFORCERTX4080S", "4080S", "RTX4080", "NVIDIAGEFORCERTX4080", "4080":
		// AutoDL's v-32g-p specification exposes a 32 GiB 4080(S) shape.
		return 32768, true
	case "RTX4090", "NVIDIAGEFORCERTX4090", "4090":
		return 24576, true
	case "RTX409048G", "NVIDIAGEFORCERTX409048G", "409048G":
		// CompShare's 48 GiB 4090 variant; the underscore is stripped by the
		// canonicalizer above, so the plain-RTX4090 contains-match cannot see it.
		return 49152, true
	case "RTX5090", "NVIDIAGEFORCERTX5090", "5090":
		// CompShare's 5090 GpuType; kept out of the plain-RTX contains-match
		// family below, so it needs its own entry like 4090_48G did.
		return 32768, true
	case "V100S", "NVIDIATESLAV100S", "TESLAV100S":
		// CompShare datacenter tier used to widen the live shape fallback.
		return 32768, true
	case "P40", "NVIDIATESLAP40", "TESLAP40":
		// P40 only exists as a 24 GiB board; exactly meets the 24 GiB floor.
		return 24576, true
	case "A800", "NVIDIAA800":
		return 81920, true
	case "H20", "NVIDIAH20":
		return 98304, true
	case "A100":
		// CompShare's bare A100 GpuType; the 40GB/80GB suffix matches below
		// never fire for it. 40 GiB is the conservative floor.
		return 40960, true
	case "RTX2080TI", "NVIDIAGEFORCERTX2080TI", "2080TI":
		return 11264, true
	case "RTX3080TI", "NVIDIAGEFORCERTX3080TI", "3080TI":
		return 12288, true
	case "RTX3090", "NVIDIAGEFORCERTX3090", "3090":
		return 24576, true
	case "A10040GB", "NVIDIAA10040GBPCIE", "NVIDIAA100SXM440GB":
		return 40960, true
	case "A10080GB", "NVIDIAA10080GBPCIE", "NVIDIAA100SXM480GB":
		return 81920, true
	default:
		if strings.Contains(canonical, "A100") && strings.Contains(canonical, "40GB") {
			return 40960, true
		}
		if strings.Contains(canonical, "A100") && strings.Contains(canonical, "80GB") {
			return 81920, true
		}
		if strings.Contains(canonical, "RTX4090") {
			return 24576, true
		}
		return 0, false
	}
}

func (p *Runpod) ListLeaked(ctx context.Context) ([]string, error) {
	if p.manual != nil {
		return nil, nil
	}
	instances, err := p.driver.ListInstances(ctx, p.creds)
	if err != nil {
		return nil, err
	}
	var leaked []string
	for _, instance := range instances {
		if strings.HasPrefix(instance.Name, TestNamePrefix) {
			leaked = append(leaked, instance.ExternalID)
		}
	}
	return leaked, nil
}

func (p *Runpod) ListLeakedForRunID(ctx context.Context, runID string) ([]string, error) {
	if p.manual != nil {
		return nil, nil
	}
	instances, err := p.driver.ListInstances(ctx, p.creds)
	if err != nil {
		return nil, err
	}
	return leakedInstanceIDsForRun(p.Name(), runID, instances)
}

func withMaxTimeout(parent context.Context, maximum time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := parent.Deadline(); ok && time.Until(deadline) <= maximum {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, maximum)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
