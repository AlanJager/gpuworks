package cloudprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	serverprovider "gpuworks.ai/gpuworks/internal/server/provider"
	autodldriver "gpuworks.ai/gpuworks/internal/server/provider/autodl"
)

const (
	autodlRentTimeout      = 8 * time.Minute
	autodlEndpointTimeout  = 5 * time.Minute
	autodlDestroyTimeout   = 3 * time.Minute
	autodlDefaultMaxPrice  = 5.0 // CNY/hour; snapshot payg_price is milli-CNY/hour.
	autodlDefaultImageUUID = "base-image-l2t43iu6uk"
	autodlDefaultGPUModel  = "RTX 4080S"
)

var autodlLedgerMu sync.Mutex

type autodlLifecycleDriver interface {
	ListInstances(context.Context, serverprovider.Credentials) ([]serverprovider.Instance, error)
	CreateInstance(context.Context, serverprovider.Credentials, serverprovider.CreateInstanceRequest) (serverprovider.Instance, error)
	DeleteInstance(context.Context, serverprovider.Credentials, string) error
	Snapshot(context.Context, serverprovider.Credentials, string) (autodldriver.Snapshot, error)
}

// AutoDL rents Pro container instances for L4 acceptance tests.
type AutoDL struct {
	driver   autodlLifecycleDriver
	creds    serverprovider.Credentials
	runID    string
	image    string
	maxPrice float64
}

func NewAutoDL(token, runID string) (*AutoDL, error) {
	image := strings.TrimSpace(os.Getenv("AUTODL_IMAGE_UUID"))
	if image == "" {
		image = autodlDefaultImageUUID
	}
	maxPrice := autodlDefaultMaxPrice
	if raw := strings.TrimSpace(os.Getenv("AUTODL_MAX_HOURLY_PRICE")); raw != "" {
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || value <= 0 {
			return nil, fmt.Errorf("autodl: invalid AUTODL_MAX_HOURLY_PRICE %q", raw)
		}
		maxPrice = value
	}
	return newAutoDL(autodldriver.New(), serverprovider.Credentials{APIKey: strings.TrimSpace(token)}, runID, image, maxPrice)
}

func newAutoDL(driver autodlLifecycleDriver, creds serverprovider.Credentials, runID, image string, maxPrice float64) (*AutoDL, error) {
	if strings.TrimSpace(creds.APIKey) == "" {
		return nil, fmt.Errorf("AutoDL token is required")
	}
	if strings.TrimSpace(runID) == "" {
		var err error
		runID, err = NewRunID()
		if err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(image) == "" {
		return nil, fmt.Errorf("AutoDL image UUID is required")
	}
	if maxPrice <= 0 {
		return nil, fmt.Errorf("AutoDL maximum hourly price must be positive")
	}
	return &AutoDL{driver: driver, creds: creds, runID: runID, image: strings.TrimSpace(image), maxPrice: maxPrice}, nil
}

func (p *AutoDL) Name() string { return "autodl" }

func (p *AutoDL) ManagementCredentials() serverprovider.Credentials { return p.creds }

func (p *AutoDL) Rent(ctx context.Context, spec GPUSpec) (*Instance, error) {
	ctx, cancel := withMaxTimeout(ctx, autodlRentTimeout)
	defer cancel()
	gpuModel := strings.TrimSpace(spec.GPUModel)
	if gpuModel == "" || strings.EqualFold(gpuModel, "auto") {
		gpuModel = autodlDefaultGPUModel
	}
	gpuCount := spec.GPUCount
	if gpuCount <= 0 {
		gpuCount = 1
	}
	if spec.MinVRAMMB > 0 {
		known, ok := gpuVRAMMB(gpuModel)
		if !ok {
			return nil, fmt.Errorf("autodl: cannot verify MinVRAMMB=%d for GPU %q", spec.MinVRAMMB, gpuModel)
		}
		if known < spec.MinVRAMMB {
			return nil, fmt.Errorf("autodl: GPU %q has %d MiB VRAM, below requested %d MiB", gpuModel, known, spec.MinVRAMMB)
		}
	}
	extraDisk := spec.DiskGB
	if extraDisk < 0 {
		extraDisk = 0
	}
	created, err := p.driver.CreateInstance(ctx, p.creds, serverprovider.CreateInstanceRequest{
		Name: TestNamePrefix + p.runID, GPUModel: gpuModel, GPUCount: gpuCount, Image: p.image,
		Extra: map[string]string{
			"expand_system_disk_by_gb": strconv.Itoa(extraDisk),
			"cuda_v_from":              "113",
			"start_command":            "sleep 1",
		},
	})
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(created.ExternalID) == "" {
		return nil, fmt.Errorf("autodl: create returned an empty instance id")
	}
	if err := addAutoDLLedger(created.ExternalID); err != nil {
		_ = p.driver.DeleteInstance(context.Background(), p.creds, created.ExternalID)
		return nil, err
	}
	failed := true
	defer func() {
		if failed {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), autodlDestroyTimeout)
			defer cleanupCancel()
			_ = p.Destroy(cleanupCtx, created.ExternalID)
		}
	}()
	endpointCtx, endpointCancel := withMaxTimeout(ctx, autodlEndpointTimeout)
	defer endpointCancel()
	var snapshot autodldriver.Snapshot
	var lastErr error
	for {
		snapshot, lastErr = p.driver.Snapshot(endpointCtx, p.creds, created.ExternalID)
		if lastErr == nil && snapshot.Instance.PublicIP != "" && snapshot.Instance.SSHPort > 0 && snapshot.RootPassword != "" {
			break
		}
		select {
		case <-endpointCtx.Done():
			return nil, fmt.Errorf("wait for AutoDL SSH endpoint: %w (instance=%s last_error=%v)", endpointCtx.Err(), created.ExternalID, lastErr)
		case <-time.After(5 * time.Second):
		}
	}
	if snapshot.Instance.PricePerHr <= 0 {
		return nil, fmt.Errorf("autodl: instance %s did not report a positive hourly price", created.ExternalID)
	}
	if snapshot.Instance.PricePerHr > p.maxPrice {
		return nil, fmt.Errorf("autodl: instance hourly price %.3f CNY exceeds configured maximum %.3f CNY", snapshot.Instance.PricePerHr, p.maxPrice)
	}
	failed = false
	return &Instance{
		InstanceID: created.ExternalID, GPUModel: firstNonEmpty(snapshot.Instance.GPUModel, gpuModel),
		GPUCount: firstPositive(snapshot.Instance.GPUCount, gpuCount), Kind: "container", Region: snapshot.Instance.Region,
		PublicIP: snapshot.Instance.PublicIP, SSHPort: snapshot.Instance.SSHPort,
		Username: firstNonEmpty(snapshot.Instance.SSHUser, "root"), Password: snapshot.RootPassword,
	}, nil
}

func (p *AutoDL) Destroy(ctx context.Context, instanceID string) error {
	ctx, cancel := withMaxTimeout(ctx, autodlDestroyTimeout)
	defer cancel()
	if err := p.driver.DeleteInstance(ctx, p.creds, instanceID); err != nil {
		return err
	}
	return removeAutoDLLedger(instanceID)
}

func (p *AutoDL) ListLeaked(ctx context.Context) ([]string, error) {
	instances, err := p.driver.ListInstances(ctx, p.creds)
	if err != nil {
		return nil, err
	}
	var leaked []string
	seen := map[string]struct{}{}
	for _, instance := range instances {
		if strings.HasPrefix(instance.Name, TestNamePrefix) {
			leaked = append(leaked, instance.ExternalID)
			seen[instance.ExternalID] = struct{}{}
		}
	}
	ledger, err := readAutoDLLedger()
	if err != nil {
		return nil, err
	}
	for _, id := range ledger {
		if _, ok := seen[id]; !ok {
			leaked = append(leaked, id)
		}
	}
	return leaked, nil
}

func (p *AutoDL) ListLeakedForRunID(ctx context.Context, runID string) ([]string, error) {
	instances, err := p.driver.ListInstances(ctx, p.creds)
	if err != nil {
		return nil, err
	}
	return leakedInstanceIDsForRun(p.Name(), runID, instances)
}

func autodlLedgerPath() string {
	return filepath.Join(os.TempDir(), "gpuworks-test-autodl-instances.json")
}

func readAutoDLLedger() ([]string, error) {
	autodlLedgerMu.Lock()
	defer autodlLedgerMu.Unlock()
	return readAutoDLLedgerLocked()
}

func readAutoDLLedgerLocked() ([]string, error) {
	data, err := os.ReadFile(autodlLedgerPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read AutoDL test ledger: %w", err)
	}
	var ids []string
	if err := json.Unmarshal(data, &ids); err != nil {
		return nil, fmt.Errorf("decode AutoDL test ledger: %w", err)
	}
	return ids, nil
}

func addAutoDLLedger(id string) error {
	autodlLedgerMu.Lock()
	defer autodlLedgerMu.Unlock()
	ids, err := readAutoDLLedgerLocked()
	if err != nil {
		return err
	}
	for _, existing := range ids {
		if existing == id {
			return nil
		}
	}
	return writeAutoDLLedgerLocked(append(ids, id))
}

func removeAutoDLLedger(id string) error {
	autodlLedgerMu.Lock()
	defer autodlLedgerMu.Unlock()
	ids, err := readAutoDLLedgerLocked()
	if err != nil {
		return err
	}
	filtered := ids[:0]
	for _, existing := range ids {
		if existing != id {
			filtered = append(filtered, existing)
		}
	}
	return writeAutoDLLedgerLocked(filtered)
}

func writeAutoDLLedgerLocked(ids []string) error {
	if len(ids) == 0 {
		if err := os.Remove(autodlLedgerPath()); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove AutoDL test ledger: %w", err)
		}
		return nil
	}
	data, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	if err := os.WriteFile(autodlLedgerPath(), data, 0o600); err != nil {
		return fmt.Errorf("write AutoDL test ledger: %w", err)
	}
	return nil
}
