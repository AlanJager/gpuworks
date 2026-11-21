package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provider/testconn"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/apiclient"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/facade"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/inventory"
)

var _ provider.ConnectionTester = (*Driver)(nil)

// GPU query routes as Cloud mounts them; the facade owns the fallback policy.
const (
	gpuDeviceRoute = "/gpu-device/gpu-devices"
	pciDeviceRoute = "/pci-device/pci-devices"
)

// connectionProbe carries one connection test's Cloud reads across the layered
// checks so later checks reuse what earlier ones fetched. The testconn runner
// executes vendor checks sequentially.
type connectionProbe struct {
	client *apiclient.APIClient
	hosts  []hostInventory
	gpus   []inventory.GPUDevice
}

// ConnectionChecks implements provider.ConnectionTester. Check details never
// carry credentials or remote response text; classification follows the same
// rules as credential validation.
func (d *Driver) ConnectionChecks(creds provider.Credentials) []testconn.VendorCheck {
	probe := &connectionProbe{}
	return []testconn.VendorCheck{
		{Name: testconn.CheckAuthRequest, Depends: []string{testconn.CheckTCPConnect}, Run: d.checkAuth(creds, probe)},
		{Name: testconn.CheckProtocolShape, Depends: []string{testconn.CheckAuthRequest}, Run: d.checkProtocolShape(creds, probe)},
		{Name: testconn.CheckAPIEndpoints, Depends: []string{testconn.CheckAuthRequest}, Run: d.checkAPIEndpoints(creds, probe)},
		{Name: testconn.CheckReadPermissions, Depends: []string{testconn.CheckAuthRequest}, Run: d.checkReadPermissions(creds, probe)},
		{Name: testconn.CheckResourceSummary, Depends: []string{testconn.CheckReadPermissions}, Run: d.checkResourceSummary(creds, probe)},
	}
}

func (d *Driver) probeClient(creds provider.Credentials, probe *connectionProbe) (*apiclient.APIClient, error) {
	if probe.client != nil {
		return probe.client, nil
	}
	client, err := d.newClient(creds)
	if err != nil {
		return nil, &testconn.Failure{Code: messages.ErrorProviderCredentialMissing, Hint: "credential_missing", Err: err}
	}
	probe.client = client
	return client, nil
}

// checkFailure classifies a Cloud API failure into the driver's ZSTACK_* codes
// without retaining remote response text.
func checkFailure(err error) *testconn.Failure {
	code := messages.ErrorZStackRequestFailed
	var productErr *messages.ProductError
	if errors.As(credentialError(err, nil), &productErr) {
		code = productErr.Code()
	}
	return &testconn.Failure{Code: code, Err: err}
}

func isRouteMissing(err error) bool {
	var apiErr *apiclient.APIError
	return errors.As(err, &apiErr) && apiErr.ProtocolFailure == apiclient.APIRouteMissing
}

func isDenied(err error) bool {
	var apiErr *apiclient.APIError
	return errors.As(err, &apiErr) && apiErr.Kind == apiclient.ErrorAuthentication
}

// queryRaw reads one page without decoding the inventory fields.
func queryRaw(ctx context.Context, client *apiclient.APIClient, path string) ([]map[string]json.RawMessage, error) {
	var reply struct {
		Inventories []map[string]json.RawMessage `json:"inventories"`
	}
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	if err := client.Get(ctx, path+separator+"limit=1", &reply); err != nil {
		return nil, err
	}
	return reply.Inventories, nil
}

// checkAuth runs the cheapest authenticated read and classifies a rejection.
func (d *Driver) checkAuth(creds provider.Credentials, probe *connectionProbe) func(context.Context) (map[string]any, error) {
	return func(ctx context.Context) (map[string]any, error) {
		client, err := d.probeClient(creds, probe)
		if err != nil {
			return nil, err
		}
		apiCtx, cancel := client.OperationContext(ctx)
		defer cancel()
		hosts, err := apiclient.QueryAll[hostInventory](apiCtx, client, "/hosts")
		if err != nil {
			return nil, checkFailure(err)
		}
		probe.hosts = hosts
		return map[string]any{"auth": "access_key", "hosts": len(hosts)}, nil
	}
}

// checkProtocolShape determines which GPU query protocol the endpoint speaks:
// legacy (PCI devices only), intermediate (GPU query without its own
// allocation field) or modern (GPU query with allocateStatus). All three are
// supported; anything else fails. The CPU reservation config binding is shape
// information too: an unbound config is reported, not failed, because the
// driver then reads the global value.
func (d *Driver) checkProtocolShape(creds provider.Credentials, probe *connectionProbe) func(context.Context) (map[string]any, error) {
	return func(ctx context.Context) (map[string]any, error) {
		client, err := d.probeClient(creds, probe)
		if err != nil {
			return nil, err
		}
		apiCtx, cancel := client.OperationContext(ctx)
		defer cancel()
		detail, err := probeGPUShape(apiCtx, client)
		if err != nil {
			return nil, err
		}
		detail["cpu_reservation_config"] = probeCPUConfigBinding(apiCtx, client, firstKVMHost(probe.hosts))
		return detail, nil
	}
}

func probeGPUShape(ctx context.Context, client *apiclient.APIClient) (map[string]any, error) {
	items, err := queryRaw(ctx, client, gpuDeviceRoute)
	if isRouteMissing(err) {
		if _, pciErr := queryRaw(ctx, client, pciDeviceRoute); pciErr == nil {
			return map[string]any{"shape": "legacy"}, nil
		} else if isRouteMissing(pciErr) {
			return nil, &testconn.Failure{Code: messages.ErrorZStackRequestFailed, Hint: "zstack_version",
				Detail: map[string]any{"shape": "unsupported", "missing": []string{gpuDeviceRoute, pciDeviceRoute}},
				Err:    errors.New("zstack: no GPU query route exists")}
		} else {
			return nil, checkFailure(pciErr)
		}
	}
	if err != nil {
		return nil, checkFailure(err)
	}
	switch {
	case len(items) == 0:
		// With no GPUs the allocation field cannot be observed.
		return map[string]any{"shape": "gpu_query", "allocation_field": "unobserved"}, nil
	case !allocationFieldPresent(items):
		return map[string]any{"shape": "intermediate", "allocation_field": "absent"}, nil
	default:
		return map[string]any{"shape": "modern", "allocation_field": "present"}, nil
	}
}

func allocationFieldPresent(items []map[string]json.RawMessage) bool {
	for _, item := range items {
		if _, ok := item["allocateStatus"]; ok {
			return true
		}
	}
	return false
}

func firstKVMHost(hosts []hostInventory) string {
	for _, h := range hosts {
		if h.Hypervisor == "KVM" {
			return h.UUID
		}
	}
	return ""
}

// probeCPUConfigBinding reports whether hosts can carry their own
// kvm.reservedCpu override: bound, unbound (the global value applies) or
// unprobed when no KVM host exists. Probe errors are left to read_permissions.
func probeCPUConfigBinding(ctx context.Context, client *apiclient.APIClient, hostUUID string) string {
	if hostUUID == "" {
		return "unprobed"
	}
	var config struct {
		Value *string `json:"value"`
	}
	err := client.Get(ctx, "/resource-configurations/"+url.PathEscape(hostUUID)+"/kvm/reservedCpu", &config)
	switch {
	case err == nil:
		return "bound"
	case facade.IsMissingResourceConfig(err):
		return "unbound"
	default:
		return "error"
	}
}

// checkAPIEndpoints probes the read endpoints the product needs and names any
// missing one. The legacy PCI route covers a missing GPU device route, so that
// absence is reported without failing. Denials stay informational here;
// read_permissions owns them.
func (d *Driver) checkAPIEndpoints(creds provider.Credentials, probe *connectionProbe) func(context.Context) (map[string]any, error) {
	return func(ctx context.Context) (map[string]any, error) {
		client, err := d.probeClient(creds, probe)
		if err != nil {
			return nil, err
		}
		apiCtx, cancel := client.OperationContext(ctx)
		defer cancel()
		detail := map[string]any{}
		var missing, absent, denied []string
		probeRoute := func(path string) error {
			_, err := queryRaw(apiCtx, client, path)
			switch {
			case err == nil:
			case isRouteMissing(err):
				missing = append(missing, path)
			case isDenied(err):
				denied = append(denied, path)
			default:
				failure := checkFailure(err)
				failure.Detail = map[string]any{"endpoint": path}
				return failure
			}
			return nil
		}
		if err := probeRoute("/hosts"); err != nil {
			return nil, err
		}
		if _, err := queryRaw(apiCtx, client, gpuDeviceRoute); isRouteMissing(err) {
			if _, pciErr := queryRaw(apiCtx, client, pciDeviceRoute); isRouteMissing(pciErr) {
				missing = append(missing, gpuDeviceRoute, pciDeviceRoute)
			} else if pciErr != nil {
				if isDenied(pciErr) {
					denied = append(denied, pciDeviceRoute)
				} else {
					failure := checkFailure(pciErr)
					failure.Detail = map[string]any{"endpoint": pciDeviceRoute}
					return nil, failure
				}
			} else {
				absent = append(absent, gpuDeviceRoute)
				detail["gpu_query"] = "pci_fallback"
			}
		} else if err != nil {
			if isDenied(err) {
				denied = append(denied, gpuDeviceRoute)
			} else {
				failure := checkFailure(err)
				failure.Detail = map[string]any{"endpoint": gpuDeviceRoute}
				return nil, failure
			}
		}
		if err := probeRoute("/global-configurations"); err != nil {
			return nil, err
		}
		if len(absent) > 0 {
			detail["absent"] = absent
		}
		if len(denied) > 0 {
			detail["denied"] = denied
		}
		if len(missing) > 0 {
			return nil, &testconn.Failure{Code: messages.ErrorZStackRequestFailed, Hint: "zstack_version",
				Detail: map[string]any{"missing": missing},
				Err:    fmt.Errorf("zstack: required read endpoints missing: %v", missing)}
		}
		return detail, nil
	}
}

// checkReadPermissions runs the read queries the product actually runs and
// names any resource whose query Cloud denies.
func (d *Driver) checkReadPermissions(creds provider.Credentials, probe *connectionProbe) func(context.Context) (map[string]any, error) {
	return func(ctx context.Context) (map[string]any, error) {
		client, err := d.probeClient(creds, probe)
		if err != nil {
			return nil, err
		}
		apiCtx, cancel := client.OperationContext(ctx)
		defer cancel()
		access := newAccess(client)
		var denied []string
		run := func(label string, query func() error) error {
			err := query()
			switch {
			case err == nil:
			case isDenied(err):
				denied = append(denied, label)
			default:
				failure := checkFailure(err)
				failure.Detail = map[string]any{"query": label}
				return failure
			}
			return nil
		}
		var hosts []hostInventory
		if err := run("hosts", func() error {
			var err error
			hosts, err = apiclient.QueryAll[hostInventory](apiCtx, client, "/hosts?q=hypervisorType=KVM")
			return err
		}); err != nil {
			return nil, err
		}
		var gpus []inventory.GPUDevice
		if err := run("gpus", func() error {
			var err error
			gpus, err = access.gpus.List(apiCtx)
			return err
		}); err != nil {
			return nil, err
		}
		for _, h := range hosts {
			if h.Hypervisor != "KVM" {
				continue
			}
			if err := run("host_memory_reservations", func() error {
				var config struct {
					Value *string `json:"value"`
				}
				return client.Get(apiCtx, "/resource-configurations/"+url.PathEscape(h.UUID)+"/kvm/reservedMemory", &config)
			}); err != nil {
				return nil, err
			}
			if err := run("host_cpu_reservations", func() error {
				_, err := access.cpu.Get(apiCtx, h.UUID)
				return err
			}); err != nil {
				return nil, err
			}
		}
		if err := run("global_configurations", func() error {
			_, err := queryRaw(apiCtx, client, "/global-configurations?q=category=kvm&q=name=reservedCpu")
			return err
		}); err != nil {
			return nil, err
		}
		if len(denied) > 0 {
			return nil, &testconn.Failure{Code: messages.ErrorZStackAuthRejected, Hint: "zstack_permission",
				Detail: map[string]any{"denied": denied},
				Err:    fmt.Errorf("zstack: read queries denied: %v", denied)}
		}
		probe.hosts, probe.gpus = hosts, gpus
		return map[string]any{"queries": "all"}, nil
	}
}

// checkResourceSummary reports the resource counts as evidence for the run.
func (d *Driver) checkResourceSummary(creds provider.Credentials, probe *connectionProbe) func(context.Context) (map[string]any, error) {
	return func(ctx context.Context) (map[string]any, error) {
		if probe.hosts == nil || probe.gpus == nil {
			// Defensive: the runner gates this check on read_permissions, which
			// populates the probe.
			return nil, &testconn.Failure{Code: messages.ErrorProviderCheckFailed, Err: errors.New("zstack: resource summary without read results")}
		}
		free := 0
		for _, g := range probe.gpus {
			if g.GPU.Allocation == "Unallocated" {
				free++
			}
		}
		return map[string]any{"hosts": len(probe.hosts), "gpus": len(probe.gpus), "gpus_free": free}, nil
	}
}
