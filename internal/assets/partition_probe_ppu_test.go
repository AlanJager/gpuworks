package assets

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const ppuTestUUID8 = "GPU-00000000-0000-0000-0000-000000000008"
const ppuTestUUID9 = "GPU-00000000-0000-0000-0000-000000000009"

func ppuFixture(t *testing.T) (string, string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Linux HGML ABI")
	}
	cc, err := exec.LookPath("cc")
	if err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal(err)
		}
		t.Skip("C compiler unavailable")
	}
	dir := t.TempDir()
	compile := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(cc, args...).CombinedOutput(); err != nil {
			t.Fatalf("compile: %v\n%s", err, out)
		}
	}
	compile("-Wall", "-Wextra", "-Werror", "../../build/runtime-assets/probe/probe.c", "-ldl", "-o", filepath.Join(dir, "probe"))
	compile("-shared", "-fPIC", "-Wall", "-Wextra", "-Werror", "testdata/ppu_hgml_fixture.c", "-o", filepath.Join(dir, "libhgml.so"))
	if err := os.Mkdir(filepath.Join(dir, "missing"), 0700); err != nil {
		t.Fatal(err)
	}
	compile("-shared", "-fPIC", "-DOMIT_UUID", "testdata/ppu_hgml_fixture.c", "-o", filepath.Join(dir, "missing/libhgml.so"))
	return dir, cc
}

func TestPartitionProbePPUDeviceIdentity(t *testing.T) {
	dir, _ := ppuFixture(t)
	for _, tc := range []struct {
		name, uuid, extra, want string
		code                    int
	}{
		{"host inventory", ppuTestUUID8, "SLICED=1", "index=8 rc=0 count=15 matches=1 total_bytes=4294967296", 0},
		{"OCI compacted", ppuTestUUID8, "COMPACT=1", "index=0 rc=0 count=1 matches=1", 0},
		{"wrong card has quota", ppuTestUUID8, "WRONG_SLICE=1", "total_bytes=103079215104", 7},
		{"whole card", ppuTestUUID8, "", "total_bytes=103079215104", 7},
		{"missing UUID", "unset", "", "invalid GPUWORKS_PARTITION_PROBE_UUID", 2},
		{"malformed UUID", "GPU-bad", "", "invalid GPUWORKS_PARTITION_PROBE_UUID", 2},
		{"multiple selections", ppuTestUUID8 + "," + ppuTestUUID9, "", "invalid GPUWORKS_PARTITION_PROBE_UUID", 2},
		{"absent selected UUID", "GPU-00000000-0000-0000-0000-000000000099", "SLICED=1", "matches=0", 7},
		{"duplicate UUID", ppuTestUUID8, "DUPLICATE=1", "matches=2", 7},
		{"empty inventory", ppuTestUUID8, "EMPTY=1", "count=0 matches=0", 7},
		{"init error", ppuTestUUID8, "INIT_FAIL=1", "rc=999", 7},
		{"count error", ppuTestUUID8, "COUNT_FAIL=1", "rc=999", 7},
		{"handle error", ppuTestUUID8, "HANDLE_FAIL=1", "rc=2", 7},
		{"handle empty", ppuTestUUID8, "HANDLE_EMPTY=1", "rc=-1", 7},
		{"UUID error", ppuTestUUID8, "UUID_FAIL=1", "rc=999", 7},
		{"UUID empty", ppuTestUUID8, "UUID_EMPTY=1", "rc=-1", 7},
		{"UUID unterminated", ppuTestUUID8, "UUID_UNTERMINATED=1", "rc=-1", 7},
		{"memory error", ppuTestUUID8, "MEMORY_FAIL=1", "rc=999", 7},
		{"shutdown error", ppuTestUUID8, "SHUTDOWN_FAIL=1", "shutdown_rc=999", 7},
		{"UUID symbol absent", ppuTestUUID8, "NO_UUID=1", "required HGML identity/memory symbols unavailable", 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			libdir := dir
			if tc.extra == "NO_UUID=1" {
				libdir = filepath.Join(dir, "missing")
			}
			cmd := exec.Command(filepath.Join(dir, "probe"))
			cmd.Env = []string{"LD_LIBRARY_PATH=" + libdir, "GPUWORKS_PARTITION_PROBE_BACKEND=ppu-dgpu", "GPUWORKS_PARTITION_PROBE_QUOTA_MIB=4096"}
			if tc.uuid != "unset" {
				cmd.Env = append(cmd.Env, "GPUWORKS_PARTITION_PROBE_UUID="+tc.uuid)
			}
			if tc.extra != "" {
				cmd.Env = append(cmd.Env, tc.extra)
			}
			if tc.extra == "COMPACT=1" || tc.extra == "SHUTDOWN_FAIL=1" {
				cmd.Env = append(cmd.Env, "SLICED=1")
			}
			out, err := cmd.CombinedOutput()
			if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != tc.code || !strings.Contains(string(out), tc.want) {
				t.Fatalf("exit=%v err=%v out=%s; want %d %q", cmd.ProcessState, err, out, tc.code, tc.want)
			}
		})
	}
}

func TestEmbeddedPPUHGMLQuotaUsesComputeUUIDOrder(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("PPU asset is amd64 only")
	}
	dir, cc := ppuFixture(t)
	data, err := files.ReadFile("bin/runtime-assets/ppu/linux-amd64/hggc-13.0/hgml_dlsym_hook.so")
	if err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(dir, "hook.so")
	if err = os.WriteFile(hook, data, 0700); err != nil {
		t.Fatal(err)
	}
	const source = `
#include <dlfcn.h>
#include <stdio.h>
struct mem { unsigned long long total, free, used; };
struct mem2 { unsigned int version; unsigned long long total, reserved, free, used; };
int main(void) {
 void *lib=dlopen("libhgml.so",RTLD_NOW); if(!lib) return 1;
 int (*init)(void)=dlsym(lib,"hgmlInit_v2");
 int (*handle)(unsigned int,void**)=dlsym(lib,"hgmlDeviceGetHandleByIndex_v2");
 int (*get)(void*,struct mem*)=dlsym(lib,"hgmlDeviceGetMemoryInfo");
 int (*get2)(void*,struct mem2*)=dlsym(lib,"hgmlDeviceGetMemoryInfo_v2");
 if(!init||!handle||!get||!get2||init()) return 2;
 unsigned int indexes[]={0,8,9};
 for(int i=0;i<3;i++) {
  void *dev=0; struct mem m={0}; struct mem2 m2={.version=sizeof(m2)|(2U<<24)};
  if(handle(indexes[i],&dev)||get(dev,&m)||get2(dev,&m2)) return 3;
  if(m.total!=m2.total||m.free!=m2.free||m.used!=m2.used||m2.version!=(sizeof(m2)|(2U<<24))||m2.reserved!=123) return 4;
  printf("%u=%llu/%llu/%llu\n",indexes[i],m.total>>30,m.used>>30,m.free>>30);
 }
 return 0;
}`
	client := filepath.Join(dir, "client.c")
	if err = os.WriteFile(client, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "client")
	if out, err := exec.Command(cc, "-Wall", "-Wextra", "-Werror", client, "-ldl", "-o", binary).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, tc := range []struct{ name, visible, extra, libdir, want string }{
		{"nonzero card", ppuTestUUID8, "", dir, "0=96/1/95\n8=4/0/4\n9=96/1/95"},
		{"reverse selection", ppuTestUUID9 + "," + ppuTestUUID8, "", dir, "0=96/1/95\n8=8/0/8\n9=4/0/4"},
		{"unselected identity", "GPU-00000000-0000-0000-0000-000000000099", "", dir, "0=96/1/95\n8=96/1/95\n9=96/1/95"},
		{"duplicate UUID", ppuTestUUID8 + "," + ppuTestUUID8, "", dir, "0=96/1/95\n8=96/1/95\n9=96/1/95"},
		{"invalid suffix", ppuTestUUID8 + ",bad", "", dir, "0=96/1/95\n8=96/1/95\n9=96/1/95"},
		{"trailing comma", ppuTestUUID8 + ",", "", dir, "0=96/1/95\n8=96/1/95\n9=96/1/95"},
		{"UUID error", ppuTestUUID8, "UUID_FAIL=1", dir, "0=96/1/95\n8=96/1/95\n9=96/1/95"},
		{"UUID symbol missing", ppuTestUUID8, "", filepath.Join(dir, "missing"), "0=96/1/95\n8=96/1/95\n9=96/1/95"},
		{"legacy agent numeric", "0", "", dir, "0=4/0/4\n8=96/1/95\n9=96/1/95"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(binary)
			cmd.Env = []string{"LD_LIBRARY_PATH=" + tc.libdir, "LD_PRELOAD=" + hook, "CUDA_VISIBLE_DEVICES=" + tc.visible, "HGGC_DEVICE_MEMORY_LIMIT_0=4096", "HGGC_DEVICE_MEMORY_LIMIT_1=8192", fmt.Sprintf("HGGC_LEDGER_PATH=%s", filepath.Join(t.TempDir(), "ledger"))}
			if tc.extra != "" {
				cmd.Env = append(cmd.Env, tc.extra)
			}
			out, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(out), tc.want) {
				t.Fatalf("%v\n%s; want %s", err, out, tc.want)
			}
		})
	}
}
