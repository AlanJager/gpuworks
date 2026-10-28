package main

import (
	"fmt"
	"os"
	"time"

	agentapp "gpuworks.ai/gpuworks/internal/agent/app"
	"gpuworks.ai/gpuworks/internal/agent/simulate"
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
	driver, prober := simulate.New(spec)
	cfg.Prober, cfg.Driver, cfg.ForceDriver = prober, driver, driver.Name()
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
	if spec.ParentPID > 0 {
		go exitWithParent(spec.ParentPID)
	}
	return nil
}

// exitWithParent ends the simulated node once the experience environment
// that started it is gone, however it ended.
func exitWithParent(pid int) {
	for {
		time.Sleep(2 * time.Second)
		if os.Getppid() != pid {
			os.Exit(0)
		}
	}
}
