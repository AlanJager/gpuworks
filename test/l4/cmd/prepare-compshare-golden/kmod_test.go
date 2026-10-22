package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbeddedScriptsAreFilledInAndParse(t *testing.T) {
	for _, name := range []string{"select-kmod.sh", "install-dual-kmod.sh"} {
		script := nvidiaFile(name)
		if strings.Contains(script, "@STORE@") || strings.Contains(script, "@VERSION@") || strings.Contains(script, "@SHA256@") || strings.Contains(script, "@TOOLKIT@") {
			t.Fatalf("%s keeps a placeholder", name)
		}
		if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
			t.Fatalf("%s does not parse: %v %s", name, err, out)
		}
	}
	install := nvidiaFile("install-dual-kmod.sh")
	for _, want := range []string{nvidiaDriverVersion, nvidiaDriverSHA256, "sha256sum -c", "kernel-open", nvidiaKmodStore, "systemctl enable gpuworks-nvidia-kmod.service", "nvidia-container-toolkit=\"" + nvidiaToolkitVersion, "nvidia-ctk runtime configure --runtime=docker", "mirrors.ustc.edu.cn/libnvidia-container"} {
		if !strings.Contains(install, want) {
			t.Fatalf("install script lacks %q", want)
		}
	}
	cmd := remoteDualKmodCommand()
	if !strings.Contains(cmd, "sudo -n sh /tmp/gpuworks-install-dual-kmod.sh") || !strings.Contains(cmd, "/tmp/gpuworks-nvidia-kmod.select") {
		t.Fatalf("remote command = %q", cmd)
	}
	if v := remoteKmodValidationCommand("img:tag"); !strings.Contains(v, "KMOD_FLAVOUR=") || !strings.Contains(v, "--gpus all") {
		t.Fatalf("validation command = %q", v)
	}
}

// runSelector runs the boot selector against fake module tools. initialises
// names the flavour whose nvidia.ko brings a GPU up ("" for none); devices
// is what lspci reports.
func runSelector(t *testing.T, initialises string, devices bool) (state, log string, err error) {
	t.Helper()
	root := t.TempDir()
	store := filepath.Join(root, "store")
	for _, flavour := range []string{"open", "proprietary"} {
		if err := os.MkdirAll(filepath.Join(store, flavour), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(store, flavour, "nvidia.ko"), []byte(flavour), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(root, "bin")
	gpus := filepath.Join(root, "gpus")
	dest := filepath.Join(root, "dest")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	lspci := "true"
	if devices {
		lspci = "echo '00:03.0 0302: 10de:1db5 (rev a1)'"
	}
	fakes := map[string]string{
		"lspci":  lspci,
		"rmmod":  "rm -rf " + gpus + "; echo rmmod $1 >>" + filepath.Join(root, "calls"),
		"depmod": "true",
		// Loading nvidia brings a GPU up only for the flavour that supports it.
		"modprobe":        `[ "$1" = nvidia ] || exit 0; if [ "$(cat ` + dest + `/nvidia.ko)" = "` + initialises + `" ]; then mkdir -p ` + gpus + `/0000:00:03.0; fi; echo modprobe $(cat ` + dest + `/nvidia.ko) >>` + filepath.Join(root, "calls"),
		"nvidia-modprobe": "true",
	}
	for name, body := range fakes {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", "-c", nvidiaFile("select-kmod.sh"))
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"),
		"GPUWORKS_NVIDIA_KMOD_STORE="+store, "GPUWORKS_NVIDIA_KMOD_DEST="+dest,
		"GPUWORKS_NVIDIA_KMOD_STATE="+filepath.Join(root, "state"), "GPUWORKS_NVIDIA_GPUS_DIR="+gpus)
	out, err := cmd.CombinedOutput()
	raw, _ := os.ReadFile(filepath.Join(root, "state"))
	calls, _ := os.ReadFile(filepath.Join(root, "calls"))
	return strings.TrimSpace(string(raw)), string(out) + string(calls), err
}

func TestSelectorKeepsOpenWhenItWorks(t *testing.T) {
	state, log, err := runSelector(t, "open", true)
	if err != nil || state != "open" || strings.Contains(log, "modprobe proprietary") {
		t.Fatalf("state=%q err=%v log=%s", state, err, log)
	}
}

// A V100S: the open nvidia.ko loads but initialises no GPU.
func TestSelectorFallsBackToProprietary(t *testing.T) {
	state, log, err := runSelector(t, "proprietary", true)
	if err != nil || state != "proprietary" {
		t.Fatalf("state=%q err=%v log=%s", state, err, log)
	}
	if !strings.Contains(log, "open modules initialised no GPU") || strings.Index(log, "modprobe open") > strings.Index(log, "modprobe proprietary") {
		t.Fatalf("expected open then proprietary: %s", log)
	}
}

func TestSelectorReportsWhenNothingWorks(t *testing.T) {
	state, log, err := runSelector(t, "", true)
	if err == nil || state != "failed" || !strings.Contains(log, "neither module build initialised a GPU") {
		t.Fatalf("state=%q err=%v log=%s", state, err, log)
	}
}

func TestSelectorDoesNothingWithoutAnNVIDIADevice(t *testing.T) {
	state, log, err := runSelector(t, "open", false)
	if err != nil || state != "none" || strings.Contains(log, "modprobe") {
		t.Fatalf("state=%q err=%v log=%s", state, err, log)
	}
}

func TestParseGPUEntries(t *testing.T) {
	for entry, want := range map[string]struct {
		model string
		count int
	}{"4090": {"4090", 1}, " 2080Ti*4 ": {"2080Ti", 4}, "V100S*x": {"V100S", 1}, "A100*0": {"A100", 1}} {
		model, count := parseGPU(entry)
		if model != want.model || count != want.count {
			t.Fatalf("%q -> %s x%d, want %s x%d", entry, model, count, want.model, want.count)
		}
	}
}
