package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const configDir = "/etc/vdev"

type deviceAPI interface {
	Init() error
	ShutDown() error
	PhysicalPCIBDF(gpuIndex int) (string, error)
	Create(gpuIndex, cuCount, memoryMiB int) ([]int, error)
	Stop(vdevID int) error
	Destroy(vdevID int) error
	Inspect(vdevID int) (deviceInfo, bool, error)
}

type deviceInfo struct {
	GPUIndex  int
	CUCount   int
	MemoryMiB int
	PCIBDF    string
}

type response struct {
	VDevID    int     `json:"vdev_id"`
	Exists    *bool   `json:"exists,omitempty"`
	GPUIndex  *int    `json:"gpu_index,omitempty"`
	CUCount   *int    `json:"cu_count,omitempty"`
	MemoryMiB *int    `json:"memory_mib,omitempty"`
	PCIBDF    *string `json:"pci_bdf,omitempty"`
}

type ownershipRecord struct {
	VDevID     int    `json:"vdev_id"`
	OwnerToken string `json:"owner_token"`
	GPUIndex   int    `json:"gpu_index"`
	CUCount    int    `json:"cu_count"`
	MemoryMiB  int    `json:"memory_mib"`
	PCIBDF     string `json:"pci_bdf"`
}

func main() {
	if err := run(os.Args[1:], newDeviceAPI(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, api deviceAPI, out io.Writer) (err error) {
	return runWithRemove(args, api, out, os.Remove)
}

func runWithRemove(args []string, api deviceAPI, out io.Writer, removeConfig func(string) error) (err error) {
	return runWithConfig(args, api, out, removeConfig, configPath)
}

func configPath(vdevID int) string {
	return filepath.Join(configDir, fmt.Sprintf("vdev%d.conf", vdevID))
}

func runWithConfig(args []string, api deviceAPI, out io.Writer, removeConfig func(string) error, sourceConfigPath func(int) string) (err error) {
	if len(args) == 0 {
		return errors.New("expected create, release, inspect, or prune")
	}
	cmd := args[0]
	flags := flag.NewFlagSet(cmd, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var gpuIndex, cuCount, memoryMiB, vdevID int
	var recordPath, pciBDF, snapshotPath, ownerToken string
	switch cmd {
	case "create":
		flags.IntVar(&gpuIndex, "gpu-index", -1, "physical device index")
		flags.IntVar(&cuCount, "cu-count", 0, "compute units")
		flags.IntVar(&memoryMiB, "memory-mib", 0, "memory in MiB")
		flags.StringVar(&recordPath, "record-path", "", "durable ownership record path")
		flags.StringVar(&pciBDF, "pci-bdf", "", "physical PCI address for the ownership record")
	case "release", "inspect", "prune":
		flags.IntVar(&vdevID, "vdev-id", -1, "virtual device ID")
		flags.StringVar(&ownerToken, "owner-token", "", "creation ownership token")
		if cmd == "release" {
			flags.IntVar(&gpuIndex, "gpu-index", -1, "expected physical device index")
			flags.IntVar(&cuCount, "cu-count", 0, "expected compute units")
			flags.IntVar(&memoryMiB, "memory-mib", 0, "expected memory in MiB")
			flags.StringVar(&pciBDF, "pci-bdf", "", "expected physical PCI address")
			flags.StringVar(&snapshotPath, "snapshot-path", "", "owned vdev config snapshot path")
		} else if cmd == "prune" {
			flags.StringVar(&snapshotPath, "snapshot-path", "", "owned vdev config snapshot path")
		}
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if cmd == "create" && (gpuIndex < 0 || cuCount <= 0 || memoryMiB <= 0) {
		return errors.New("create requires nonnegative --gpu-index and positive --cu-count and --memory-mib")
	}
	if cmd == "create" && ((recordPath == "") != (pciBDF == "")) {
		return errors.New("create requires --record-path and --pci-bdf together")
	}
	if cmd != "create" && vdevID < 0 {
		return errors.New("release, inspect, and prune require nonnegative --vdev-id")
	}
	if cmd == "prune" && snapshotPath == "" {
		return errors.New("prune requires --snapshot-path")
	}
	if cmd == "release" && (gpuIndex < 0 || cuCount <= 0 || memoryMiB <= 0 || pciBDF == "") {
		return errors.New("release requires nonnegative --gpu-index, positive --cu-count and --memory-mib, and --pci-bdf")
	}
	if cmd == "release" && snapshotPath == "" {
		return errors.New("release requires --snapshot-path")
	}
	if (cmd == "release" || cmd == "prune") && ownerToken == "" {
		return fmt.Errorf("%s requires --owner-token", cmd)
	}
	if cmd == "create" || cmd == "release" || cmd == "prune" || cmd == "inspect" && ownerToken != "" {
		lock, lockErr := lockVdevDir(filepath.Dir(sourceConfigPath(0)))
		if lockErr != nil {
			return lockErr
		}
		defer func() { err = errors.Join(err, unlockVdevDir(lock)) }()
	}
	if err := api.Init(); err != nil {
		return fmt.Errorf("initialize Hygon DCGM: %w", err)
	}
	defer func() {
		if shutdownErr := api.ShutDown(); shutdownErr != nil {
			err = errors.Join(err, fmt.Errorf("shut down Hygon DCGM: %w", shutdownErr))
		}
	}()

	switch cmd {
	case "create":
		physicalPCI, err := api.PhysicalPCIBDF(gpuIndex)
		if err != nil {
			return fmt.Errorf("verify physical GPU %d PCI address before create: %w", gpuIndex, err)
		}
		physicalPCI = strings.TrimSpace(physicalPCI)
		if physicalPCI == "" {
			return fmt.Errorf("physical GPU %d has no PCI address; refusing to create a vdev", gpuIndex)
		}
		if pciBDF != "" && !strings.EqualFold(pciBDF, physicalPCI) {
			return fmt.Errorf("physical GPU %d PCI address changed: got %s, expected %s; refusing to create a vdev", gpuIndex, physicalPCI, pciBDF)
		}
		pciBDF = physicalPCI
		if err := fenceAbsentOwners(api, filepath.Dir(sourceConfigPath(0))); err != nil {
			return fmt.Errorf("fence absent vdev owners: %w", err)
		}
		before, err := checkExistingConfigs(api, filepath.Dir(sourceConfigPath(0)))
		if err != nil {
			return err
		}
		intentPath, err := beginCreateIntent(recordPath)
		if err != nil {
			return err
		}
		ids, createErr := api.Create(gpuIndex, cuCount, memoryMiB)
		if len(ids) > 1 || len(ids) == 1 && (ids[0] < 0 || createErr != nil) {
			return errors.Join(createErr, fmt.Errorf("DCGM returned ambiguous vdev IDs for one requested device: %v; manual inspection required", ids))
		}
		verifiedID, verifyErr := recoverCreatedVdevID(api, before, filepath.Dir(sourceConfigPath(0)), deviceInfo{GPUIndex: gpuIndex, CUCount: cuCount, MemoryMiB: memoryMiB, PCIBDF: pciBDF})
		if verifyErr != nil {
			return errors.Join(createErr, fmt.Errorf("cannot verify newly created vdev (returned IDs %v); manual inspection required: %w", ids, verifyErr))
		}
		if len(ids) == 1 && ids[0] != verifiedID {
			return fmt.Errorf("DCGM returned vdev %d but the verified new config belongs to vdev %d; manual inspection required", ids[0], verifiedID)
		}
		ids = []int{verifiedID}
		if recordPath != "" {
			previous, ownerErr := readCurrentOwner(filepath.Dir(sourceConfigPath(0)), ids[0])
			if ownerErr != nil && !errors.Is(ownerErr, os.ErrNotExist) {
				return fmt.Errorf("read previous owner of vdev %d: %w; manual inspection required", ids[0], ownerErr)
			}
			if ownerErr == nil && previous.Active {
				return fmt.Errorf("vdev %d still has an active owner; manual inspection required", ids[0])
			}
			token, err := newOwnerToken()
			if err != nil {
				return fmt.Errorf("generate vdev owner token: %w", err)
			}
			record := ownershipRecord{VDevID: ids[0], OwnerToken: token, GPUIndex: gpuIndex, CUCount: cuCount, MemoryMiB: memoryMiB, PCIBDF: pciBDF}
			snapshotPath := filepath.Join(filepath.Dir(recordPath), fmt.Sprintf("vdev%d.conf", ids[0]))
			created, snapshotErr := writeConfigSnapshot(sourceConfigPath(ids[0]), snapshotPath)
			if snapshotErr != nil {
				destroyed, rollbackErr := releaseDeviceChecked(api, ids[0], deviceInfo{GPUIndex: gpuIndex, CUCount: cuCount, MemoryMiB: memoryMiB, PCIBDF: pciBDF}, "", sourceConfigPath(ids[0]), removeConfig)
				if destroyed && created {
					rollbackErr = errors.Join(rollbackErr, removeSnapshot(snapshotPath))
				}
				if destroyed {
					rollbackErr = errors.Join(rollbackErr, clearCreateIntent(intentPath))
				}
				return errors.Join(fmt.Errorf("snapshot vdev %d config: %w", ids[0], snapshotErr), rollbackErr)
			}
			if err := writeOwnershipRecord(recordPath, record); err != nil {
				destroyed, rollbackErr := releaseDeviceChecked(api, ids[0], deviceInfo{GPUIndex: gpuIndex, CUCount: cuCount, MemoryMiB: memoryMiB, PCIBDF: pciBDF}, "", sourceConfigPath(ids[0]), removeConfig)
				if destroyed {
					rollbackErr = errors.Join(rollbackErr, removeSnapshot(snapshotPath), clearCreateIntent(intentPath))
				}
				return errors.Join(fmt.Errorf("record vdev %d ownership: %w", ids[0], err), rollbackErr)
			}
			if err := writeCurrentOwner(filepath.Dir(sourceConfigPath(0)), ids[0], currentOwner{Token: token, Active: true}); err != nil {
				// The private ledger is deliberately left in place for manual
				// recovery if native rollback cannot prove destruction.
				destroyed, rollbackErr := releaseDeviceChecked(api, ids[0], deviceInfo{GPUIndex: gpuIndex, CUCount: cuCount, MemoryMiB: memoryMiB, PCIBDF: pciBDF}, snapshotPath, sourceConfigPath(ids[0]), removeConfig)
				if destroyed {
					rollbackErr = errors.Join(rollbackErr, clearCreateIntent(intentPath))
				}
				return errors.Join(fmt.Errorf("record current owner of vdev %d: %w", ids[0], err), rollbackErr)
			}
		}
		if err := clearCreateIntent(intentPath); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(response{VDevID: ids[0]})
	case "inspect":
		var owner currentOwner
		if ownerToken != "" {
			var err error
			owner, err = requireCurrentOwner(filepath.Dir(sourceConfigPath(0)), vdevID, ownerToken)
			if err != nil {
				return err
			}
		}
		info, exists, err := api.Inspect(vdevID)
		if err != nil {
			return fmt.Errorf("inspect vdev %d: %w", vdevID, err)
		}
		if ownerToken != "" && !owner.Active && exists {
			return fmt.Errorf("vdev %d owner was released but a device exists; refusing reuse", vdevID)
		}
		result := response{VDevID: vdevID, Exists: &exists}
		if exists {
			result.GPUIndex = &info.GPUIndex
			result.CUCount = &info.CUCount
			result.MemoryMiB = &info.MemoryMiB
			result.PCIBDF = &info.PCIBDF
		}
		return json.NewEncoder(out).Encode(result)
	case "release":
		ownerDir := filepath.Dir(sourceConfigPath(0))
		owner, err := requireCurrentOwner(ownerDir, vdevID, ownerToken)
		if err != nil {
			return err
		}
		if !owner.Active {
			return fmt.Errorf("vdev %d owner is already released", vdevID)
		}
		destroyed, releaseErr := releaseDeviceChecked(api, vdevID, deviceInfo{GPUIndex: gpuIndex, CUCount: cuCount, MemoryMiB: memoryMiB, PCIBDF: pciBDF}, snapshotPath, sourceConfigPath(vdevID), removeConfig)
		if destroyed {
			owner.Active = false
			if err := writeCurrentOwner(ownerDir, vdevID, owner); err != nil {
				return errors.Join(releaseErr, fmt.Errorf("mark vdev %d owner released: %w", vdevID, err))
			}
		}
		if releaseErr != nil {
			return releaseErr
		}
		if !destroyed {
			return fmt.Errorf("vdev %d is absent; use prune to clear its owner", vdevID)
		}
		return json.NewEncoder(out).Encode(response{VDevID: vdevID})
	case "prune":
		ownerDir := filepath.Dir(sourceConfigPath(0))
		owner, err := requireCurrentOwner(ownerDir, vdevID, ownerToken)
		if err != nil {
			return err
		}
		if err := pruneConfig(api, vdevID, snapshotPath, sourceConfigPath(vdevID), removeConfig); err != nil {
			return err
		}
		if owner.Active {
			owner.Active = false
			if err := writeCurrentOwner(ownerDir, vdevID, owner); err != nil {
				return err
			}
		}
		return json.NewEncoder(out).Encode(response{VDevID: vdevID})
	}
	panic("unreachable")
}

func vdevConfigNames(dir string) (map[string]struct{}, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make(map[string]struct{})
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "vdev") && strings.HasSuffix(entry.Name(), ".conf") {
			names[entry.Name()] = struct{}{}
		}
	}
	return names, nil
}

func pruneConfig(api deviceAPI, vdevID int, snapshotPath, hostConfigPath string, removeConfig func(string) error) error {
	_, exists, err := api.Inspect(vdevID)
	if err != nil {
		return fmt.Errorf("inspect vdev %d before pruning config: %w", vdevID, err)
	}
	if exists {
		return fmt.Errorf("vdev %d still exists; refusing to prune config", vdevID)
	}
	hostData, err := os.ReadFile(hostConfigPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read vdev %d host config: %w", vdevID, err)
	}
	snapshotData, err := os.ReadFile(snapshotPath)
	if err != nil {
		return fmt.Errorf("read vdev %d config snapshot: %w", vdevID, err)
	}
	if !bytes.Equal(hostData, snapshotData) {
		return fmt.Errorf("vdev %d host config differs from owned snapshot; refusing to prune", vdevID)
	}
	if err := removeConfig(hostConfigPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove vdev %d host config: %w", vdevID, err)
	}
	return nil
}

func lockVdevDir(dir string) (*os.File, error) {
	path := filepath.Join(dir, ".gpuworks-vdev.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open vdev lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		file.Close()
		return nil, fmt.Errorf("lock vdev operations at %s: %w", path, err)
	}
	return file, nil
}

func unlockVdevDir(file *os.File) error {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return fmt.Errorf("unlock vdev operations: %w", errors.Join(err, closeErr))
	}
	return nil
}

func checkExistingConfigs(api deviceAPI, dir string) (map[string]struct{}, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]struct{}{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan existing vdev configs in %s: %w", dir, err)
	}
	before := make(map[string]struct{})
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "vdev") || !strings.HasSuffix(name, ".conf") {
			continue
		}
		id, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "vdev"), ".conf"))
		if err != nil || id < 0 || name != fmt.Sprintf("vdev%d.conf", id) {
			return nil, fmt.Errorf("cannot verify existing vdev config %s before create: invalid ID", filepath.Join(dir, name))
		}
		_, exists, err := api.Inspect(id)
		if err != nil {
			return nil, fmt.Errorf("inspect vdev %d from existing config before create: %w", id, err)
		}
		if !exists {
			return nil, fmt.Errorf("existing vdev config %s has no live device; resolve stale config before create", filepath.Join(dir, name))
		}
		before[name] = struct{}{}
	}
	return before, nil
}

func releaseDeviceChecked(api deviceAPI, vdevID int, expected deviceInfo, snapshotPath, hostConfigPath string, removeConfig func(string) error) (bool, error) {
	info, exists, err := api.Inspect(vdevID)
	if err != nil {
		return false, fmt.Errorf("inspect vdev %d before release: %w", vdevID, err)
	}
	if !exists {
		// No device identity can be verified, so leave any config file alone.
		return false, nil
	}
	if info.GPUIndex != expected.GPUIndex || info.CUCount != expected.CUCount || info.MemoryMiB != expected.MemoryMiB || !strings.EqualFold(info.PCIBDF, expected.PCIBDF) {
		return false, fmt.Errorf("vdev %d identity mismatch: got gpu-index=%d cu-count=%d memory-mib=%d pci-bdf=%s; expected gpu-index=%d cu-count=%d memory-mib=%d pci-bdf=%s", vdevID, info.GPUIndex, info.CUCount, info.MemoryMiB, info.PCIBDF, expected.GPUIndex, expected.CUCount, expected.MemoryMiB, expected.PCIBDF)
	}
	if snapshotPath != "" {
		hostData, err := os.ReadFile(hostConfigPath)
		if err != nil {
			return false, fmt.Errorf("read vdev %d host config before release: %w", vdevID, err)
		}
		snapshotData, err := os.ReadFile(snapshotPath)
		if err != nil {
			return false, fmt.Errorf("read vdev %d config snapshot before release: %w", vdevID, err)
		}
		if !bytes.Equal(hostData, snapshotData) {
			return false, fmt.Errorf("vdev %d host config differs from owned snapshot; refusing to release", vdevID)
		}
	}
	stopErr := api.Stop(vdevID)
	if destroyErr := api.Destroy(vdevID); destroyErr != nil {
		return false, errors.Join(fmt.Errorf("destroy vdev %d: %w", vdevID, destroyErr), stopErr)
	}
	path := hostConfigPath
	if err := removeConfig(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return true, fmt.Errorf("remove vdev config %s: %w", path, err)
	}
	if stopErr != nil {
		return true, fmt.Errorf("stop vdev %d reported an error even though destroy succeeded: %w", vdevID, stopErr)
	}
	return true, nil
}

func writeConfigSnapshot(source, destination string) (bool, error) {
	input, err := os.Open(source)
	if err != nil {
		return false, err
	}
	defer input.Close()
	dir := filepath.Dir(destination)
	// The workload's non-root process must traverse this directory to read
	// the bind-mounted config; the ownership JSON remains private (0600).
	if err := os.Chmod(dir, 0o755); err != nil {
		return false, err
	}
	file, err := os.CreateTemp(dir, ".vdev-config-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(file.Name())
	if _, err := io.Copy(file, input); err != nil {
		file.Close()
		return false, err
	}
	if err := file.Chmod(0o644); err != nil {
		file.Close()
		return false, err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return false, err
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	// A prior snapshot may belong to another create attempt; never replace it.
	if err := os.Link(file.Name(), destination); err != nil {
		return false, err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return true, err
	}
	defer directory.Close()
	return true, directory.Sync()
}

func removeSnapshot(path string) error {
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove vdev config snapshot %s after rollback: %w", path, err)
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func writeOwnershipRecord(path string, record ownershipRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, ".vdev-ownership-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(append(data, '\n')); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	// Link fails if an ownership record already exists; never replace it.
	if err := os.Link(file.Name(), path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
