package provider

import (
	"context"
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/localization"
)

type fallbackTestDriver struct {
	serviceTestDriver
	name     string
	attempts []string
	failures map[string]error
}

func (d *fallbackTestDriver) Name() string { return d.name }

func (d *fallbackTestDriver) ListInstances(context.Context, Credentials) ([]Instance, error) {
	return []Instance{{
		ExternalID: "inst-fallback", Name: "fallback", PublicIP: "203.0.113.10", SSHConnections: []SSHConnection{
			{ID: "proxy", Type: "proxy", Label: localization.SameText("Proxy", requiredProviderLocales...), Host: "proxy.example", Port: 2200, User: "root", InstallSupported: true},
			{ID: "terminal", Type: "proxy", Label: localization.SameText("Terminal", requiredProviderLocales...), Host: "terminal.example", Port: 22, User: "user", InstallSupported: false},
			{ID: "direct", Type: "direct", Label: localization.SameText("Direct", requiredProviderLocales...), Host: "203.0.113.10", Port: 2222, User: "root", InstallSupported: true},
		},
	}}, nil
}

func (d *fallbackTestDriver) InstallAgent(_ context.Context, _ Credentials, req InstallRequest) error {
	d.attempts = append(d.attempts, req.Instance.PublicIP)
	return d.failures[req.Instance.PublicIP]
}

func newFallbackService(t *testing.T, driver *fallbackTestDriver) (*Service, uint) {
	t.Helper()
	Register(driver)
	st, err := store.Open("file:"+driver.name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	box, err := auth.NewSecretbox(driver.name)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, box)
	p, err := svc.Add(t.Context(), driver.name, driver.name, Credentials{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	return svc, p.ID
}

func TestServiceInstallUsesDirectWithoutTryingProxy(t *testing.T) {
	driver := &fallbackTestDriver{name: "fallback-direct-success", failures: map[string]error{}}
	svc, providerID := newFallbackService(t, driver)
	result, err := svc.Install(t.Context(), providerID, "inst-fallback", "https://server.example", InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(driver.attempts) != 1 || driver.attempts[0] != "203.0.113.10" {
		t.Fatalf("attempts = %v", driver.attempts)
	}
	if result.SSHConnection == nil || result.SSHConnection.ID != "direct" || len(result.SSHAttempts) != 1 || result.SSHAttempts[0].Status != "selected" {
		t.Fatalf("result = %+v", result)
	}
}

func TestServiceInstallFallsBackToProxyOnConnectionFailure(t *testing.T) {
	driver := &fallbackTestDriver{name: "fallback-proxy-success", failures: map[string]error{
		"203.0.113.10": RetryableSSHConnectionError("dial", errors.New("connection refused")),
	}}
	svc, providerID := newFallbackService(t, driver)
	result, err := svc.Install(t.Context(), providerID, "inst-fallback", "https://server.example", InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(driver.attempts) != 2 || driver.attempts[0] != "203.0.113.10" || driver.attempts[1] != "proxy.example" {
		t.Fatalf("attempts = %v", driver.attempts)
	}
	if result.SSHConnection == nil || result.SSHConnection.ID != "proxy" || len(result.SSHAttempts) != 2 {
		t.Fatalf("result = %+v", result)
	}
}

func TestServiceInstallExplicitSelectionDoesNotFallback(t *testing.T) {
	driver := &fallbackTestDriver{name: "fallback-explicit", failures: map[string]error{
		"203.0.113.10": RetryableSSHConnectionError("dial", errors.New("connection refused")),
	}}
	svc, providerID := newFallbackService(t, driver)
	_, err := svc.Install(t.Context(), providerID, "inst-fallback", "https://server.example", InstallOptions{SSHConnectionID: "direct"})
	if err == nil {
		t.Fatal("expected explicit direct connection to fail")
	}
	if len(driver.attempts) != 1 || driver.attempts[0] != "203.0.113.10" {
		t.Fatalf("attempts = %v", driver.attempts)
	}
}

func TestServiceInstallCommandFailureDoesNotFallback(t *testing.T) {
	driver := &fallbackTestDriver{name: "fallback-command", failures: map[string]error{
		"203.0.113.10": errors.New("install command failed"),
	}}
	svc, providerID := newFallbackService(t, driver)
	_, err := svc.Install(t.Context(), providerID, "inst-fallback", "https://server.example", InstallOptions{})
	if err == nil {
		t.Fatal("expected install command failure")
	}
	if len(driver.attempts) != 1 {
		t.Fatalf("attempts = %v", driver.attempts)
	}
}

func TestServiceSyncPersistsSSHConnections(t *testing.T) {
	driver := &fallbackTestDriver{name: "fallback-persistence", failures: map[string]error{}}
	svc, providerID := newFallbackService(t, driver)
	if _, err := svc.Sync(t.Context(), providerID); err != nil {
		t.Fatal(err)
	}
	instances, err := svc.store.ProviderInstances(providerID)
	if err != nil || len(instances) != 1 {
		t.Fatalf("instances = %+v, err = %v", instances, err)
	}
	if instances[0].SSHConnections == "" || instances[0].SSHConnections == "[]" {
		t.Fatalf("stored SSH connections = %q", instances[0].SSHConnections)
	}
}
