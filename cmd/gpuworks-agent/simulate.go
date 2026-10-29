package main

import (
	"context"
	"fmt"
	"os"
	"time"

	agentapp "gpuworks.ai/gpuworks/internal/agent/app"
	"gpuworks.ai/gpuworks/internal/agent/simulate"
	"gpuworks.ai/gpuworks/internal/parentwatch"
	"gpuworks.ai/gpuworks/internal/proto"
)

// applySimulation makes the agent a simulated node of the experience
// environment (internal/agent/simulate): the machine described by the spec
// file, with simulated cards and runtime, reporting the spec's address.
func applySimulation(cfg *agentapp.Config, path string) error {
	spec, err := simulate.Load(path)
	if err != nil {
		return err
	}
	// Only the experience environment starts a simulated node, as its child:
	// a real node that inherited the variable (an exported shell, a stale
	// service drop-in) must not start reporting simulated cards under its
	// real identity.
	if spec.ParentPID <= 0 || os.Getppid() != spec.ParentPID {
		return fmt.Errorf("%s is set, but this agent was not started by the experience environment that wrote %s", simulate.EnvSpec, path)
	}
	driver, prober := simulate.New(spec)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := driver.ReleaseAll(ctx); err != nil {
		return fmt.Errorf("release the engines of the previous simulated node: %w", err)
	}
	cfg.Prober, cfg.Driver, cfg.ForceDriver = prober, driver, driver.Name()
	cfg.SimulatedHost = prober.Host()
	address := spec.Address
	cfg.NetworkInterfaceResolver = func(string) (string, error) {
		if address == "" {
			return "", fmt.Errorf("simulated node has no address")
		}
		return "eth0", nil
	}
	cfg.NetworkInventory = func() proto.NetInventory {
		inv := proto.NetInventory{Interfaces: []proto.NetInterface{}, RDMAPorts: []proto.RDMAPort{}}
		if address != "" {
			inv.Interfaces = append(inv.Interfaces, proto.NetInterface{Name: "eth0", State: "up", Addrs: []string{address + "/24"}})
			inv.Management = &proto.ManagementAddress{Interface: "eth0", IP: address}
		}
		return inv
	}
	// None outlives the environment that started it.
	go parentwatch.Follow(context.Background(), spec.ParentPID, func() { os.Exit(0) })
	return nil
}
