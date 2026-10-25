package cloudprovider

import (
	"errors"
	"strings"
	"testing"

	serverprovider "gpuworks.ai/gpuworks/internal/server/provider"
)

// Rent reports the SSH host of the preferred connection, a Vast proxy when
// the instance has no direct SSH port; its container ports are mapped on the
// instance's own address.
func TestVastMappedPortHostIsTheInstancesAddressNotTheSSHProxy(t *testing.T) {
	privateKey, _, err := GenerateSSHKey()
	if err != nil {
		t.Fatal(err)
	}
	driver := &fakeVastDriver{fakeProviderDriver: &fakeProviderDriver{listed: []serverprovider.Instance{
		{ExternalID: "other", PublicIP: "198.51.100.9"},
		{
			ExternalID: "contract-7", PublicIP: "203.0.113.8",
			SSHConnections: []serverprovider.SSHConnection{{ID: "proxy", Type: "proxy", Host: "ssh1.vast.ai", Port: 41022, User: "root", InstallSupported: true}},
		},
		{ExternalID: "no-address"},
	}}}
	p, err := newVast(driver, serverprovider.Credentials{APIKey: "key"}, "vast-run", privateKey, 0.25, "nvidia/cuda:test")
	if err != nil {
		t.Fatal(err)
	}
	host, err := p.MappedPortHost(t.Context(), "contract-7")
	if err != nil || host != "203.0.113.8" {
		t.Fatalf("host = %q, err = %v; want the instance's own address", host, err)
	}
	if _, err := p.MappedPortHost(t.Context(), "no-address"); err == nil || !strings.Contains(err.Error(), "no public address") {
		t.Fatalf("an instance without an address: err = %v", err)
	}
	if _, err := p.MappedPortHost(t.Context(), "gone"); err == nil || !strings.Contains(err.Error(), "not listed") {
		t.Fatalf("an unlisted instance: err = %v", err)
	}
	driver.err = errors.New("vast API down")
	if _, err := p.MappedPortHost(t.Context(), "contract-7"); err == nil || !strings.Contains(err.Error(), "vast API down") {
		t.Fatalf("a failing listing: err = %v", err)
	}
}
