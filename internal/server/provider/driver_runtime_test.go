package provider

import (
	"context"
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type runtimeFactoryDriver struct{ serviceTestDriver }

func (*runtimeFactoryDriver) Name() string { return "runtime-factory-test" }

func (d *runtimeFactoryDriver) NewDriver(st *store.Store, svc *Service, installer provision.Installer) Driver {
	return &runtimeBoundDriver{runtimeFactoryDriver: d, store: st, service: svc, installer: installer}
}

type runtimeBoundDriver struct {
	*runtimeFactoryDriver
	store     *store.Store
	service   *Service
	installer provision.Installer
}

type runtimeInstaller struct{ err error }

func (i *runtimeInstaller) InstallSSH(context.Context, provision.SSHInstallRequest) (provision.SSHInstallReport, error) {
	return provision.SSHInstallReport{}, i.err
}

func registerRuntimeTestDriver(t *testing.T, driver Driver) {
	t.Helper()
	previous, existed := Get(driver.Name())
	Register(driver)
	t.Cleanup(func() {
		regMu.Lock()
		defer regMu.Unlock()
		if existed {
			registry[driver.Name()] = previous
		} else {
			delete(registry, driver.Name())
		}
	})
}

func TestConfigureDriversKeepsApplicationDependenciesIsolated(t *testing.T) {
	prototype := &runtimeFactoryDriver{}
	registerRuntimeTestDriver(t, prototype)
	legacy := &serviceTestDriver{}
	registerRuntimeTestDriver(t, legacy)
	first, second := NewService(&store.Store{}, nil), NewService(&store.Store{}, nil)
	firstInstaller := &runtimeInstaller{err: errors.New("first SSH failed")}
	secondInstaller := &runtimeInstaller{err: context.Canceled}
	first.ConfigureDrivers(firstInstaller)
	second.ConfigureDrivers(secondInstaller)
	for _, test := range []struct {
		service   *Service
		installer *runtimeInstaller
	}{{first, firstInstaller}, {second, secondInstaller}} {
		driver, ok := test.service.driver(prototype.Name())
		bound, isBound := driver.(*runtimeBoundDriver)
		if !ok || !isBound || bound.store != test.service.store || bound.service != test.service {
			t.Fatalf("driver did not bind to its own application: %#v", driver)
		}
		_, err := bound.installer.InstallSSH(t.Context(), provision.SSHInstallRequest{})
		if !errors.Is(err, test.installer.err) {
			t.Fatalf("installer boundary crossed applications: %v", err)
		}
		if got, _ := test.service.driver(legacy.Name()); got != legacy {
			t.Fatal("driver without a factory changed")
		}
	}
	if got, _ := Get(prototype.Name()); got != prototype {
		t.Fatal("application dependencies replaced the global prototype")
	}
	before, _ := first.driver(prototype.Name())
	first.ConfigureDrivers(secondInstaller)
	if after, _ := first.driver(prototype.Name()); after != before {
		t.Fatal("configuration replaced an existing app-local driver")
	}
}
