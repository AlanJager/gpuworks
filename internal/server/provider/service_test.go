package provider

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type serviceTestDriver struct{}

func (serviceTestDriver) Name() string { return "service-test-provider" }

func (serviceTestDriver) Capabilities() Capabilities {
	return Capabilities{CanListInstances: true}
}

func (serviceTestDriver) ValidateCredentials(context.Context, Credentials) error { return nil }

func (serviceTestDriver) ListInstances(context.Context, Credentials) ([]Instance, error) {
	return nil, nil
}

func (serviceTestDriver) PowerState(context.Context, Credentials, string) (PowerState, error) {
	return PowerUnknown, nil
}

func (serviceTestDriver) PowerOn(context.Context, Credentials, string) error {
	return ErrUnsupported
}

func (serviceTestDriver) PowerOff(context.Context, Credentials, string) error {
	return ErrUnsupported
}

func (serviceTestDriver) CreateInstance(context.Context, Credentials, CreateInstanceRequest) (Instance, error) {
	return Instance{}, ErrUnsupported
}

func (serviceTestDriver) DeleteInstance(context.Context, Credentials, string) error {
	return ErrUnsupported
}

func (serviceTestDriver) ResizeInstance(context.Context, Credentials, string, ResizeInstanceRequest) error {
	return ErrUnsupported
}

func (serviceTestDriver) InstallAgent(context.Context, Credentials, InstallRequest) error {
	return nil
}

type installTokenTestDriver struct {
	token          string
	mode           string
	listen         string
	reader         AgentBinaryReader
	installed      Instance
	listedInstance *Instance
	detailInstance *Instance
}

func (d *installTokenTestDriver) Name() string { return "install-token-test-provider" }

func (d *installTokenTestDriver) Capabilities() Capabilities {
	return Capabilities{CanListInstances: true, CanInstallAgent: true}
}

func (d *installTokenTestDriver) ValidateCredentials(context.Context, Credentials) error { return nil }

func (d *installTokenTestDriver) ListInstances(context.Context, Credentials) ([]Instance, error) {
	if d.listedInstance != nil {
		return []Instance{*d.listedInstance}, nil
	}
	return []Instance{{ExternalID: "inst-1", Name: "inst-1", PublicIP: "203.0.113.10"}}, nil
}

func (d *installTokenTestDriver) GetInstance(context.Context, Credentials, string) (Instance, error) {
	if d.detailInstance == nil {
		return Instance{}, errors.New("detail endpoint unavailable")
	}
	return *d.detailInstance, nil
}

func (d *installTokenTestDriver) PowerState(context.Context, Credentials, string) (PowerState, error) {
	return PowerUnknown, nil
}

func (d *installTokenTestDriver) PowerOn(context.Context, Credentials, string) error {
	return ErrUnsupported
}

func (d *installTokenTestDriver) PowerOff(context.Context, Credentials, string) error {
	return ErrUnsupported
}

func (d *installTokenTestDriver) CreateInstance(context.Context, Credentials, CreateInstanceRequest) (Instance, error) {
	return Instance{}, ErrUnsupported
}

func (d *installTokenTestDriver) DeleteInstance(context.Context, Credentials, string) error {
	return ErrUnsupported
}

func (d *installTokenTestDriver) ResizeInstance(context.Context, Credentials, string, ResizeInstanceRequest) error {
	return ErrUnsupported
}

func (d *installTokenTestDriver) InstallAgent(ctx context.Context, creds Credentials, req InstallRequest) error {
	d.token = req.Token
	d.mode = req.ConnectMode
	d.listen = req.ServerDialListen
	d.reader = req.ReadAgentBinary
	d.installed = req.Instance
	return nil
}

func TestServiceAddStoresEndpointAndExtraCredentials(t *testing.T) {
	Register(serviceTestDriver{})

	st, err := store.Open("file:provider_service_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	box, err := auth.NewSecretbox("provider-service-test")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, box)

	p, err := svc.Add(context.Background(), "service-test-provider", "test", Credentials{
		Endpoint: "https://provider.example.test/",
		APIKey:   "api-key",
		Extra:    map[string]string{"ssh_password": "secret"},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, err := svc.creds(p)
	if err != nil {
		t.Fatalf("creds: %v", err)
	}
	if got.Endpoint != "https://provider.example.test/" {
		t.Fatalf("Endpoint = %q", got.Endpoint)
	}
	if got.APIKey != "api-key" {
		t.Fatalf("APIKey = %q", got.APIKey)
	}
	if got.Extra["ssh_password"] != "secret" {
		t.Fatalf("Extra[ssh_password] = %q", got.Extra["ssh_password"])
	}
}

func TestServiceUpdateMergesExistingCredentials(t *testing.T) {
	Register(serviceTestDriver{})

	st, err := store.Open("file:provider_service_update_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	box, err := auth.NewSecretbox("provider-service-update-test")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, box)

	p, err := svc.Add(context.Background(), "service-test-provider", "original", Credentials{
		Endpoint: "https://provider.example.test/",
		APIKey:   "old-key",
		Extra:    map[string]string{"ssh_password": "secret"},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	updated, err := svc.Update(context.Background(), p.ID, "renamed", Credentials{
		APIKey: "new-key",
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Name != "renamed" {
		t.Fatalf("updated name = %q", updated.Name)
	}
	got, err := svc.creds(updated)
	if err != nil {
		t.Fatalf("creds: %v", err)
	}
	if got.Endpoint != "https://provider.example.test/" {
		t.Fatalf("Endpoint = %q", got.Endpoint)
	}
	if got.APIKey != "new-key" {
		t.Fatalf("APIKey = %q", got.APIKey)
	}
	if got.Extra["ssh_password"] != "secret" {
		t.Fatalf("Extra[ssh_password] = %q", got.Extra["ssh_password"])
	}
}

func TestServiceInstallIssuesPerNodeToken(t *testing.T) {
	drv := &installTokenTestDriver{}
	Register(drv)

	st, err := store.Open("file:provider_install_token_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	box, err := auth.NewSecretbox("provider-install-token-test")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, box)
	wantAgent := []byte("embedded-agent")
	svc.SetAgentBinaryReader(func(name string) ([]byte, error) {
		if name != "gpuworks-agent-linux-amd64" {
			t.Fatalf("agent binary name = %q", name)
		}
		return wantAgent, nil
	})
	p, err := svc.Add(context.Background(), "install-token-test-provider", "test", Credentials{APIKey: "key"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	result, err := svc.Install(context.Background(), p.ID, "inst-1", "https://server", InstallOptions{
		SSHKeyPEM: "ssh-key", ConnectMode: "server_dial", ServerDialListen: ":19091", HostAddress: "10.0.0.8",
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if drv.token == "" || drv.token == "global-token" {
		t.Fatalf("InstallAgent token = %q", drv.token)
	}
	if drv.mode != "server_dial" || drv.listen != ":19091" {
		t.Fatalf("InstallAgent mode/listen = %q/%q", drv.mode, drv.listen)
	}
	if result.ServerDialAddress != "10.0.0.8:19091" {
		t.Fatalf("server_dial address = %q", result.ServerDialAddress)
	}
	if drv.reader == nil {
		t.Fatal("InstallAgent agent binary reader is nil")
	}
	gotAgent, err := drv.reader("gpuworks-agent-linux-amd64")
	if err != nil || string(gotAgent) != string(wantAgent) {
		t.Fatalf("InstallAgent agent binary = %q, %v", gotAgent, err)
	}
	if result.Token != drv.token || result.ConnectMode != "server_dial" || result.ServerDialAddress != "10.0.0.8:19091" {
		t.Fatalf("Install result = %+v token=%q", result, drv.token)
	}
	nodes, err := st.ListNodes()
	if err != nil || len(nodes) != 1 || nodes[0].Token != drv.token {
		t.Fatalf("pending node token = %+v, err=%v", nodes, err)
	}
	if nodes[0].ServerURL != "https://server" || nodes[0].ServerDialAddress != "10.0.0.8:19091" {
		t.Fatalf("pending node addresses = server=%q dial=%q", nodes[0].ServerURL, nodes[0].ServerDialAddress)
	}
	if nodes[0].ServerDialListen != ":19091" || !nodes[0].HasSSHCredential {
		t.Fatalf("pending node recovery settings = listen=%q credential=%v", nodes[0].ServerDialListen, nodes[0].HasSSHCredential)
	}
	credential, secrets, err := st.GetNodeSSHCredential(result.NodeUUID)
	if err != nil {
		t.Fatalf("stored node SSH credential: %v", err)
	}
	if credential.Host != "203.0.113.10" || credential.Port != 22 || credential.User != "root" || secrets.PrivateKeyPEM != "ssh-key" {
		t.Fatalf("stored node SSH credential = %+v secrets=%+v", credential, secrets)
	}
	redial, err := st.ServerDialNodes()
	if err != nil || len(redial) != 1 || redial[0].State != store.NodeStateConnecting {
		t.Fatalf("pending server_dial nodes = %+v, err=%v", redial, err)
	}
}

func TestServiceInstallFallsBackToInstancePublicIP(t *testing.T) {
	drv := &installTokenTestDriver{}
	Register(drv)
	st, err := store.Open("file:provider_install_address_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	box, err := auth.NewSecretbox("provider-install-address-test")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, box)
	p, err := svc.Add(context.Background(), "install-token-test-provider", "test", Credentials{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	// With no host address given, the server dials the instance's IP.
	result, err := svc.Install(context.Background(), p.ID, "inst-1", "https://server", InstallOptions{ConnectMode: "server_dial"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ServerDialAddress != "203.0.113.10:18800" {
		t.Fatalf("server_dial address = %q", result.ServerDialAddress)
	}
}

func TestServiceInstallPrefersInstancePrivateIP(t *testing.T) {
	drv := &installTokenTestDriver{listedInstance: &Instance{
		ExternalID: "inst-1", Name: "inst-1",
		PublicIP: "203.0.113.10", PrivateIP: "10.42.0.10",
	}}
	Register(drv)
	st, err := store.Open("file:provider_install_private_address_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	box, err := auth.NewSecretbox("provider-install-private-address-test")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, box)
	p, err := svc.Add(context.Background(), "install-token-test-provider", "test", Credentials{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.Install(context.Background(), p.ID, "inst-1", "https://server", InstallOptions{ConnectMode: "server_dial"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ServerDialAddress != "10.42.0.10:18800" {
		t.Fatalf("server_dial address = %q, want the provider private IP", result.ServerDialAddress)
	}
}

func TestServiceInstallRefreshesLaggingListSSHFromInstanceDetail(t *testing.T) {
	drv := &installTokenTestDriver{
		listedInstance: &Instance{ExternalID: "inst-1", Name: "runpod", GPUModel: "RTX 2000 Ada", GPUCount: 1, Power: PowerRunning, Kind: "container"},
		detailInstance: &Instance{ExternalID: "inst-1", PublicIP: "198.51.100.21", SSHPort: 5022, SSHUser: "root", SSHConnections: []SSHConnection{{ID: "direct", Type: "direct", Host: "198.51.100.21", Port: 5022, User: "root", InstallSupported: true}}},
	}
	Register(drv)
	st, err := store.Open("file:provider_install_lagging_list_ssh?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	box, err := auth.NewSecretbox("provider-install-lagging-list-ssh")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, box)
	p, err := svc.Add(context.Background(), "install-token-test-provider", "test", Credentials{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.Install(context.Background(), p.ID, "inst-1", "https://server", InstallOptions{SSHKeyPEM: "ssh-key"})
	if err != nil {
		t.Fatal(err)
	}
	if drv.installed.PublicIP != "198.51.100.21" || drv.installed.SSHPort != 5022 {
		t.Fatalf("install target = %+v", drv.installed)
	}
	if drv.installed.Name != "runpod" || drv.installed.GPUModel != "RTX 2000 Ada" || drv.installed.Kind != "container" {
		t.Fatalf("list metadata was not preserved: %+v", drv.installed)
	}
	if result.SSHConnection == nil || result.SSHConnection.ID != "direct" || len(result.SSHAttempts) != 1 || result.SSHAttempts[0].Status != "selected" {
		t.Fatalf("install connection result = %+v", result)
	}
}

func TestServiceInstallAcceptsNgrokEndpoint(t *testing.T) {
	drv := &installTokenTestDriver{}
	Register(drv)
	st, err := store.Open("file:provider_install_ngrok_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	box, err := auth.NewSecretbox("provider-install-ngrok-test")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, box)
	p, err := svc.Add(context.Background(), "install-token-test-provider", "test", Credentials{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.Install(context.Background(), p.ID, "inst-1", "https://server", InstallOptions{
		ConnectMode: "server_dial", HostAddress: "http://0.tcp.ap.ngrok.io:25408",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ServerDialAddress != "0.tcp.ap.ngrok.io:25408" {
		t.Fatalf("server_dial address = %q", result.ServerDialAddress)
	}
}

// autodlTestDriver is the install test driver under AutoDL's kind.
type autodlTestDriver struct{ *installTokenTestDriver }

func (autodlTestDriver) Name() string { return "autodl" }

// AutoDL instances open HTTP ports only, which a server-dialed agent cannot
// use: such an install is refused before any node is created.
func TestServiceInstallRefusesServerDialOnAutoDL(t *testing.T) {
	Register(autodlTestDriver{&installTokenTestDriver{}})
	st, err := store.Open("file:provider_install_autodl_server_dial_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	box, err := auth.NewSecretbox("provider-install-autodl-test")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, box)
	p, err := svc.Add(context.Background(), "autodl", "autodl", Credentials{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Install(context.Background(), p.ID, "inst-1", "", InstallOptions{ConnectMode: "server_dial"}); err == nil || !strings.Contains(err.Error(), "HTTP ports only") {
		t.Fatalf("Install = %v", err)
	}
	if nodes, err := st.ListNodes(); err != nil || len(nodes) != 0 {
		t.Fatalf("a refused install created nodes: %+v %v", nodes, err)
	}
}

// portMapperTestDriver chooses the agent's port itself, as Vast and RunPod do.
type portMapperTestDriver struct {
	*installTokenTestDriver
	seen InstallRequest
}

func (*portMapperTestDriver) Name() string { return "port-mapper-test-provider" }

func (d *portMapperTestDriver) InstallAgentWithReport(_ context.Context, _ Credentials, req InstallRequest) (InstallReport, error) {
	d.seen = req
	return InstallReport{ServerDialListen: "0.0.0.0:10100", ServerDialAddress: "198.51.100.4:18211"}, nil
}

func (*portMapperTestDriver) ServerDialAddressFor(context.Context, Credentials, string, string) (string, error) {
	return "198.51.100.4:18211", nil
}

// With no address given, a port-mapping provider is not handed the
// instance's IP: it chooses, and the node records where the agent listens
// and where the server reaches it.
func TestServiceInstallRecordsThePortAProviderChose(t *testing.T) {
	drv := &portMapperTestDriver{installTokenTestDriver: &installTokenTestDriver{}}
	Register(drv)
	st, err := store.Open("file:provider_install_port_mapper_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	box, err := auth.NewSecretbox("provider-install-port-mapper-test")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, box)
	p, err := svc.Add(context.Background(), "port-mapper-test-provider", "mapper", Credentials{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.Install(context.Background(), p.ID, "inst-1", "", InstallOptions{ConnectMode: "server_dial"})
	if err != nil {
		t.Fatal(err)
	}
	if drv.seen.ServerDialAddress != "" {
		t.Fatalf("the driver was handed address %q to use", drv.seen.ServerDialAddress)
	}
	if result.ServerDialAddress != "198.51.100.4:18211" || result.ServerDialListen != "0.0.0.0:10100" {
		t.Fatalf("result = %+v", result)
	}
	node, err := st.NodeByUUID(result.NodeUUID)
	if err != nil || node.ServerDialAddress != "198.51.100.4:18211" || node.ServerDialListen != "0.0.0.0:10100" {
		t.Fatalf("node = %+v, %v", node, err)
	}
	if infos := KindInfos(); !kindAutoPort(infos, "port-mapper-test-provider") || kindAutoPort(infos, "install-token-test-provider") {
		t.Fatal("server_dial_auto_port does not follow the driver's capability")
	}
}

func kindAutoPort(infos []KindInfo, kind string) bool {
	for _, info := range infos {
		if info.Kind == kind {
			return info.ServerDialAutoPort
		}
	}
	return false
}

// compshareMapperTestDriver maps ports for containers only, as CompShare
// does: its VMs have a public address.
type compshareMapperTestDriver struct{ *portMapperTestDriver }

func (*compshareMapperTestDriver) Name() string { return "compshare" }

func (*compshareMapperTestDriver) MapsServerDialPorts(inst Instance) bool {
	return inst.Kind == "container"
}

// A CompShare container is dialed where the driver forwards the agent's
// port from; a CompShare VM at its public address on the listen port.
func TestServiceInstallMapsPortsForCompShareContainersOnly(t *testing.T) {
	for _, tc := range []struct {
		kind, wantSeen, wantAddress string
	}{
		{kind: "container", wantSeen: "", wantAddress: "198.51.100.4:18211"},
		{kind: "vm", wantSeen: "203.0.113.10", wantAddress: "203.0.113.10:3389"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			drv := &compshareMapperTestDriver{&portMapperTestDriver{installTokenTestDriver: &installTokenTestDriver{
				listedInstance: &Instance{ExternalID: "inst-1", Name: "inst-1", PublicIP: "203.0.113.10", Kind: tc.kind},
			}}}
			Register(drv)
			st, err := store.Open("file:provider_install_compshare_mapper_"+tc.kind+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			box, err := auth.NewSecretbox("provider-install-compshare-mapper-test")
			if err != nil {
				t.Fatal(err)
			}
			svc := NewService(st, box)
			p, err := svc.Add(context.Background(), "compshare", "compshare", Credentials{APIKey: "key"})
			if err != nil {
				t.Fatal(err)
			}
			result, err := svc.Install(context.Background(), p.ID, "inst-1", "", InstallOptions{ConnectMode: "server_dial"})
			if err != nil {
				t.Fatal(err)
			}
			if drv.seen.ServerDialAddress != tc.wantSeen {
				t.Fatalf("driver was handed address %q, want %q", drv.seen.ServerDialAddress, tc.wantSeen)
			}
			if result.ServerDialAddress != tc.wantAddress {
				t.Fatalf("server_dial address = %q, want %q", result.ServerDialAddress, tc.wantAddress)
			}
		})
	}
}

// TestServiceRecordStoresWhatASyncWould: Record keeps the account the way
// Add does and its instances the way Sync does, and asks the vendor nothing.
func TestServiceRecordStoresWhatASyncWould(t *testing.T) {
	Register(serviceTestDriver{})
	st, err := store.Open("file:provider_service_record_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	box, err := auth.NewSecretbox("provider-service-record-test")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, box)
	if _, err := svc.Record("no-such-kind", "x", Credentials{}, nil); err == nil {
		t.Fatal("an unknown kind was recorded")
	}
	p, err := svc.Record("service-test-provider", "shown", Credentials{APIKey: "k"}, []Instance{{
		ExternalID: "i-1", Name: "gpu-1", GPUModel: "H100", GPUCount: 8, Power: PowerRunning,
		SSHConnections: []SSHConnection{{Host: "10.0.0.1", Port: 22}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := svc.creds(p); err != nil || got.APIKey != "k" || p.State != "valid" {
		t.Fatalf("creds = %+v, %v; state %q", got, err, p.State)
	}
	rows, err := st.ProviderInstances(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ExternalID != "i-1" || rows[0].GPUCount != 8 || rows[0].Power != string(PowerRunning) ||
		!strings.Contains(rows[0].SSHConnections, "10.0.0.1") {
		t.Fatalf("rows = %+v", rows)
	}
}
