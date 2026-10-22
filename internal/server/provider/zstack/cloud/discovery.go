package cloud

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/apiclient"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/inventory"
)

// The wire names below follow HostInventory, GpuDeviceInventory and
// APIGetResourceConfigReply in ZStack. Missing capacities remain unknown.
type hostInventory struct {
	UUID            string `json:"uuid"`
	Name            string `json:"name"`
	ManagementIP    string `json:"managementIp"`
	Username        string `json:"username"`
	SSHPort         int    `json:"sshPort"`
	Hypervisor      string `json:"hypervisorType"`
	State           string `json:"state"`
	Status          string `json:"status"`
	TotalCPU        *int64 `json:"totalCpuCapacity"`
	AvailableCPU    *int64 `json:"availableCpuCapacity"`
	TotalMemory     *int64 `json:"totalMemoryCapacity"`
	AvailableMemory *int64 `json:"availableMemoryCapacity"`
}

type gpuInventory struct {
	GPUType     string `json:"gpuType"`
	Device      string `json:"device"`
	Description string `json:"description"`
	UUID        string `json:"uuid"`
	HostUUID    string `json:"hostUuid"`
	PCIAddress  string `json:"pciDeviceAddress"`
	State       string `json:"state"`
	Status      string `json:"status"`
	Allocation  string `json:"allocateStatus"`
	Mode        string `json:"mode"`
	VMUUID      string `json:"vmInstanceUuid"`
}

type globalConfigInventory struct {
	Name         string `json:"name"`
	Category     string `json:"category"`
	Value        string `json:"value"`
	DefaultValue string `json:"defaultValue"`
}

func isMissingResourceConfig(err error) bool {
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Kind != apiclient.ErrorAuthentication &&
		(apiErr.StatusCode == http.StatusNotFound || apiErr.Code == "20007" || apiErr.DiagnosticCode == "20007")
}

func queryAll[T any](ctx context.Context, c *apiclient.APIClient, path string) ([]T, error) {
	const pageSize = 100
	queryURL, err := url.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("zstack: invalid discovery query: %w", err)
	}
	query := queryURL.Query()
	query.Set("limit", strconv.Itoa(pageSize))
	query.Set("sort", "uuid")
	out := []T{}
	for start := 0; start < 100000; start += pageSize {
		var page struct {
			Inventories *[]T `json:"inventories"`
		}
		query.Set("start", strconv.Itoa(start))
		queryURL.RawQuery = query.Encode()
		if err := c.Get(ctx, queryURL.String(), &page); err != nil {
			return nil, err
		}
		if page.Inventories == nil {
			return nil, fmt.Errorf("zstack: missing inventories")
		}
		out = append(out, (*page.Inventories)...)
		if len(*page.Inventories) < pageSize {
			return out, nil
		}
	}
	return nil, fmt.Errorf("zstack: discovery pagination exceeded safety limit")
}

// readGlobalConfig reads a persisted global configuration value. The q
// filters are intentionally repeated: this is the query form supported by
// ZStack's REST API. A missing inventory means that the caller should use its
// own compatibility default.
func readGlobalConfig(ctx context.Context, c *apiclient.APIClient, category, name string) (string, bool, error) {
	var reply struct {
		Inventories []globalConfigInventory `json:"inventories"`
	}
	path := "/global-configurations?q=category=" + url.QueryEscape(category) + "&q=name=" + url.QueryEscape(name)
	if err := c.Get(ctx, path, &reply); err != nil {
		if isMissingResourceConfig(err) {
			return "", false, nil
		}
		return "", false, err
	}
	for _, inventory := range reply.Inventories {
		if inventory.Category != category || inventory.Name != name {
			continue
		}
		value := strings.TrimSpace(inventory.Value)
		if value == "" {
			value = strings.TrimSpace(inventory.DefaultValue)
		}
		return value, true, nil
	}
	return "", false, nil
}

// readCPUReservation prefers a host ResourceConfig override. When the host
// does not expose that binding, or exposes it without an override, the
// kvm.reservedCpu global configuration is the effective baseline.
func readCPUReservation(ctx context.Context, c *apiclient.APIClient, hostID string) (value int64, resourceConfigAvailable bool, err error) {
	var config struct {
		Value *string `json:"value"`
	}
	err = c.Get(ctx, "/resource-configurations/"+url.PathEscape(hostID)+"/kvm/reservedCpu", &config)
	if err != nil && !isMissingResourceConfig(err) {
		return 0, false, err
	}
	resourceConfigAvailable = err == nil
	if config.Value != nil {
		value, parseErr := strconv.ParseInt(strings.TrimSpace(*config.Value), 10, 64)
		if parseErr != nil || value < 0 {
			return 0, resourceConfigAvailable, fmt.Errorf("zstack: invalid CPU reservation")
		}
		return value, resourceConfigAvailable, nil
	}
	globalValue, _, globalErr := readGlobalConfig(ctx, c, "kvm", "reservedCpu")
	if globalErr != nil {
		return 0, resourceConfigAvailable, fmt.Errorf("zstack: global CPU reservation query failed: %w", globalErr)
	}
	if globalValue == "" {
		return 0, resourceConfigAvailable, nil
	}
	value, parseErr := strconv.ParseInt(globalValue, 10, 64)
	if parseErr != nil || value < 0 {
		return 0, resourceConfigAvailable, fmt.Errorf("zstack: invalid global CPU reservation")
	}
	return value, resourceConfigAvailable, nil
}

func (d *Driver) DiscoverResources(ctx context.Context, creds provider.Credentials) ([]inventory.CloudResource, error) {
	c, err := d.newClient(creds)
	if err != nil {
		return nil, err
	}
	ctx, cancel := c.OperationContext(ctx)
	defer cancel()
	hosts, err := queryAll[hostInventory](ctx, c, "/hosts?q=hypervisorType=KVM")
	if err != nil {
		return nil, err
	}
	gpus, err := queryAll[gpuInventory](ctx, c, "/gpu-device/gpu-devices")
	if err != nil {
		return nil, err
	}
	byHost := map[string][]inventory.CloudGPU{}
	for _, g := range gpus {
		if g.UUID == "" || g.HostUUID == "" {
			return nil, fmt.Errorf("zstack: GPU inventory lacks identity")
		}
		byHost[g.HostUUID] = append(byHost[g.HostUUID], cloudGPU(g))
	}
	resources := make([]inventory.CloudResource, 0, len(hosts))
	seen := map[string]bool{}
	for _, h := range hosts {
		if h.UUID == "" || seen[h.UUID] {
			return nil, fmt.Errorf("zstack: missing or duplicate host identity")
		}
		seen[h.UUID] = true
		var reserved, reservedCPU *int64
		if h.Hypervisor == "KVM" {
			var config struct {
				Value *string `json:"value"`
			}
			if err := c.Get(ctx, "/resource-configurations/"+url.PathEscape(h.UUID)+"/kvm/reservedMemory", &config); err != nil {
				return nil, err
			}
			if config.Value != nil {
				value, err := memoryBytes(*config.Value)
				if err != nil {
					return nil, err
				}
				reserved = &value
			}
			reservedCPUValue, _, err := readCPUReservation(ctx, c, h.UUID)
			if err != nil {
				return nil, fmt.Errorf("zstack: host CPU reservation query failed: %w", err)
			}
			reservedCPU = &reservedCPUValue
		}
		devices := byHost[h.UUID]
		if devices == nil {
			devices = []inventory.CloudGPU{}
		}
		resources = append(resources, inventory.CloudResource{
			Type: "host", ExternalID: h.UUID, Name: h.Name, ManagementAddress: h.ManagementIP,
			SSHUser: h.Username, SSHPort: h.SSHPort,
			Hypervisor: h.Hypervisor, State: h.State, Status: h.Status,
			CPU:    inventory.ResourceCapacity{Total: h.TotalCPU, Available: h.AvailableCPU, Reserved: reservedCPU, Unit: "vcpu"},
			Memory: inventory.ResourceCapacity{Total: h.TotalMemory, Available: h.AvailableMemory, Reserved: reserved, Unit: "bytes"}, GPUs: devices,
		})
	}
	return resources, nil
}

func (d *Driver) QueryHostResources(ctx context.Context, creds provider.Credentials, hostID string, gpuIDs []string) (inventory.CloudResource, error) {
	if strings.TrimSpace(hostID) == "" || len(gpuIDs) == 0 {
		return inventory.CloudResource{}, fmt.Errorf("zstack: target host and GPUs are required")
	}
	seen := make(map[string]bool, len(gpuIDs))
	for _, id := range gpuIDs {
		if strings.TrimSpace(id) == "" || seen[id] {
			return inventory.CloudResource{}, fmt.Errorf("zstack: target GPU IDs must be non-empty and unique")
		}
		seen[id] = true
	}
	c, err := d.newClient(creds)
	if err != nil {
		return inventory.CloudResource{}, err
	}
	ctx, cancel := c.OperationContext(ctx)
	defer cancel()
	h, err := queryResource[hostInventory](ctx, c, "/hosts", hostID)
	if err != nil {
		return inventory.CloudResource{}, err
	}
	if h.UUID != hostID {
		return inventory.CloudResource{}, fmt.Errorf("zstack: queried host identity does not match the target")
	}
	resource := inventory.CloudResource{
		Type: "host", ExternalID: h.UUID, Name: h.Name, ManagementAddress: h.ManagementIP,
		Hypervisor: h.Hypervisor, State: h.State, Status: h.Status,
		CPU:    inventory.ResourceCapacity{Total: h.TotalCPU, Available: h.AvailableCPU, Unit: "vcpu"},
		Memory: inventory.ResourceCapacity{Total: h.TotalMemory, Available: h.AvailableMemory, Unit: "bytes"},
		GPUs:   make([]inventory.CloudGPU, 0, len(gpuIDs)),
	}
	for _, id := range gpuIDs {
		g, err := queryResource[gpuInventory](ctx, c, "/gpu-device/gpu-devices", id)
		if err != nil {
			return inventory.CloudResource{}, err
		}
		if g.UUID != id || g.HostUUID != hostID {
			return inventory.CloudResource{}, fmt.Errorf("zstack: queried GPU identity or host does not match the target")
		}
		resource.GPUs = append(resource.GPUs, cloudGPU(g))
	}
	return resource, nil
}

func queryResource[T any](ctx context.Context, c *apiclient.APIClient, path, id string) (T, error) {
	var zero T
	var reply struct {
		Inventories []T `json:"inventories"`
	}
	query := url.Values{"q": {"uuid=" + id}}
	if err := c.Get(ctx, path+"?"+query.Encode(), &reply); err != nil {
		return zero, err
	}
	if len(reply.Inventories) != 1 {
		return zero, fmt.Errorf("zstack: expected one resource from %s for UUID %s, got %d", path, id, len(reply.Inventories))
	}
	return reply.Inventories[0], nil
}

func memoryBytes(raw string) (int64, error) {
	s := strings.ToUpper(strings.TrimSpace(raw))
	multiplier := int64(1)
	for suffix, factor := range map[string]int64{"K": 1 << 10, "M": 1 << 20, "G": 1 << 30, "T": 1 << 40, "B": 1} {
		if strings.HasSuffix(s, suffix) {
			multiplier = factor
			s = strings.TrimSuffix(s, suffix)
			break
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 || v > (1<<63-1)/multiplier {
		return 0, fmt.Errorf("zstack: invalid memory reservation")
	}
	return v * multiplier, nil
}

func cloudGPU(g gpuInventory) inventory.CloudGPU {
	model := strings.TrimSpace(g.GPUType)
	if model == "" {
		model = strings.TrimSpace(g.Device)
	}
	return inventory.CloudGPU{
		Model: model, Description: g.Description, ExternalID: g.UUID,
		PCIAddress: g.PCIAddress, State: g.State, Status: g.Status,
		Allocation: g.Allocation, Mode: g.Mode, VMUUID: g.VMUUID,
	}
}
