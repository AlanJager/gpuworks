package assets

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestAscendCompatibilityNativeDispatch(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("ELF preload test requires Linux")
	}
	cc, err := exec.LookPath("cc")
	if err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal(err)
		}
		t.Skip("native compiler unavailable")
	}
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	compile := func(args ...string) {
		t.Helper()
		out, err := exec.Command(cc, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("cc %v: %v\n%s", args, err, out)
		}
	}
	driver := write("driver.c", `#include <stdint.h>
#include <stdlib.h>
int halGetDeviceInfo(uint32_t d,int32_t m,int32_t i,int64_t*v){const char*e=getenv("DRIVER_RC"); if(e)return atoi(e); if(!v)return 87; *v=d; return 0;}`)
	caller := write("caller.c", `#include <stdint.h>
extern int halGetDeviceInfo(uint32_t,int32_t,int32_t,int64_t*);
int query(uint32_t d,int32_t m,int32_t i,int64_t*v){return halGetDeviceInfo(d,m,i,v);}`)
	main := write("main.c", `#define _GNU_SOURCE
#include <dlfcn.h>
#include <stdint.h>
#include <stdlib.h>
#include <stdio.h>
#include <sys/stat.h>
#include <pthread.h>
typedef int(*query_fn)(uint32_t,int32_t,int32_t,int64_t*);
static query_fn query; static int dev,module,info,want_rc; static long long want_value;
static void*run(void*unused){(void)unused; for(int i=0;i<100;i++){int64_t v=-1;int rc=query(dev,module,info,&v);if(rc!=want_rc||v!=want_value){fprintf(stderr,"rc=%d value=%lld expected=%d/%lld\n",rc,(long long)v,want_rc,want_value);return (void*)1;}}return 0;}
int main(int argc,char**argv){if(argc!=8)return 2;struct stat st; if(stat(argv[1],&st))return 3; char id[160];snprintf(id,sizeof(id),"%llu:%llu:%llu:%lld",(unsigned long long)st.st_dev,(unsigned long long)st.st_ino,(unsigned long long)st.st_size,(long long)st.st_mtim.tv_sec*1000000000LL+st.st_mtim.tv_nsec);setenv("GPUWORKS_ASCEND_TSD_FILE",id,1);
void*h=dlopen(argv[2],RTLD_NOW);if(!h){fprintf(stderr,"%s\n",dlerror());return 4;} query=(query_fn)dlsym(h,"query");if(!query)return 5;dev=atoi(argv[3]);module=atoi(argv[4]);info=atoi(argv[5]);want_rc=atoi(argv[6]);want_value=atoll(argv[7]);pthread_t threads[4];for(int i=0;i<4;i++)if(pthread_create(&threads[i],0,run,0))return 6;for(int i=0;i<4;i++){void*r;if(pthread_join(threads[i],&r)||r)return 1;}return 0;}`)
	compile("-shared", "-fPIC", "-o", filepath.Join(dir, "libascend_hal.so"), driver)
	for _, n := range []string{"tsd.so", "other.so"} {
		compile("-shared", "-fPIC", "-o", filepath.Join(dir, n), caller, "-L"+dir, "-lascend_hal", "-Wl,-rpath,"+dir)
	}
	// An unresolved driver must fail, never claim a successful hardware query.
	compile("-shared", "-fPIC", "-o", filepath.Join(dir, "missing-driver.so"), caller)
	shim := filepath.Join(dir, "compat.so")
	compile("-std=c11", "-O2", "-Wall", "-Wextra", "-Werror", "-shared", "-fPIC", "-o", shim, "../../build/runtime-assets/ascend-compat/compat.c", "-ldl")
	binary := filepath.Join(dir, "test")
	compile("-o", binary, main, "-ldl", "-pthread")
	oversized := make([]string, 257)
	for i := range oversized {
		oversized[i] = strconv.Itoa(i)
	}
	for _, tc := range []struct {
		name, mapping          string
		dev, module, info, rc  int
		value                  int64
		other, absent, missing bool
		driverRC               string
	}{
		{name: "sparse", mapping: "7", info: 1, value: 7},
		{name: "existing_zero", mapping: "5,7", info: 1, value: 5},
		{name: "second", mapping: "5,7", dev: 1, info: 1, value: 7},
		{name: "identity", mapping: "0,1", info: 1, value: 0},
		{name: "absent", dev: 7, info: 1, value: 7, absent: true},
		{name: "empty", dev: 7, info: 1, value: 7},
		{name: "other_caller", mapping: "7", info: 1, value: 0, other: true},
		{name: "other_module", mapping: "7", module: 1, info: 1, value: 0},
		{name: "other_info", mapping: "7", info: 2, value: 0},
		{name: "driver_failure", mapping: "7", info: 1, rc: 55, value: -1, driverRC: "55"},
		{name: "missing_driver", mapping: "7", info: 1, rc: 87, value: -1, missing: true},
		{name: "out_of_range", mapping: "7", dev: 1, info: 1, rc: 87, value: -1},
		{name: "trailing_comma", mapping: "7,", info: 1, rc: 87, value: -1},
		{name: "negative", mapping: "-1", info: 1, rc: 87, value: -1},
		{name: "overflow", mapping: "4294967296", info: 1, rc: 87, value: -1},
		{name: "duplicate", mapping: "7,7", info: 1, rc: 87, value: -1},
		{name: "invalid_later", mapping: "7,x", info: 1, rc: 87, value: -1},
		{name: "too_many", mapping: strings.Join(oversized, ","), info: 1, rc: 87, value: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := filepath.Join(dir, "tsd.so")
			if tc.other {
				target = filepath.Join(dir, "other.so")
			}
			if tc.missing {
				target = filepath.Join(dir, "missing-driver.so")
			}
			cmd := exec.Command(binary, filepath.Join(dir, "tsd.so"), target, strconv.Itoa(tc.dev), strconv.Itoa(tc.module), strconv.Itoa(tc.info), strconv.Itoa(tc.rc), fmt.Sprint(tc.value))
			cmd.Env = []string{"LD_PRELOAD=" + shim, "LD_LIBRARY_PATH=" + dir}
			if !tc.absent {
				cmd.Env = append(cmd.Env, "ASCEND_RT_VISIBLE_DEVICES="+tc.mapping)
			}
			if tc.driverRC != "" {
				cmd.Env = append(cmd.Env, "DRIVER_RC="+tc.driverRC)
			}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
		})
	}
}
