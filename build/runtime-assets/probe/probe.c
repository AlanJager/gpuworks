#define _GNU_SOURCE
#include <dlfcn.h>
#include <errno.h>
#include <limits.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>

struct dsmi_hbm_info {
    unsigned long long memory_size; /* KB */
    unsigned int freq;
    unsigned long long memory_usage;
    int temp;
    unsigned int bandwidth_util_rate;
};

struct hgml_memory {
    unsigned long long total; /* bytes */
    unsigned long long free;
    unsigned long long used;
};

static unsigned long long quota_mib(void) {
    const char *raw = getenv("GPUWORKS_PARTITION_PROBE_QUOTA_MIB");
    char *end = NULL;
    errno = 0;
    unsigned long long value = strtoull(raw ? raw : "", &end, 10);
    if (errno || end == raw || (end && *end) || value == 0) {
        fprintf(stderr, "invalid GPUWORKS_PARTITION_PROBE_QUOTA_MIB=%s\n", raw ? raw : "");
        exit(2);
    }
    return value;
}

static int probe_ascend(unsigned long long quota) {
    typedef int (*get_hbm_fn)(int, struct dsmi_hbm_info *);
    typedef int (*get_logic_id_fn)(unsigned int, unsigned int *);
    const char *raw = getenv("GPUWORKS_PARTITION_PROBE_DEVICE");
    char *end = NULL;
    errno = 0;
    unsigned long physical = strtoul(raw ? raw : "", &end, 10);
    if (!raw || raw[0] < '0' || raw[0] > '9' || errno || *end || physical > INT_MAX) {
        fprintf(stderr, "invalid GPUWORKS_PARTITION_PROBE_DEVICE=%s\n", raw ? raw : "");
        return 2;
    }
    /* DSMI uses container-local logical IDs. Ask the driver to resolve the
     * selected physical device: OCI containers can compact IDs, whereas
     * PRoot retains the host view, including gaps in its device inventory. */
    get_logic_id_fn get_logic_id = (get_logic_id_fn)dlsym(RTLD_DEFAULT, "dsmi_get_logicid_from_phyid");
    if (!get_logic_id) {
        fprintf(stderr, "dsmi_get_logicid_from_phyid unavailable: %s\n", dlerror());
        return 3;
    }
    unsigned int logical = UINT_MAX;
    int rc = get_logic_id((unsigned int)physical, &logical);
    if (rc != 0 || logical > INT_MAX) {
        fprintf(stderr, "Ascend device mapping failed: physical_id=%lu rc=%d logical_id=%u\n", physical, rc, logical);
        return 4;
    }
    get_hbm_fn get_hbm = (get_hbm_fn)dlsym(RTLD_DEFAULT, "dsmi_get_hbm_info");
    if (!get_hbm) {
        fprintf(stderr, "dsmi_get_hbm_info unavailable: %s\n", dlerror());
        return 3;
    }
    struct dsmi_hbm_info info = {0};
    rc = get_hbm((int)logical, &info);
    printf("backend=ascend physical_id=%lu logical_id=%u rc=%d total_kb=%llu quota_mib=%llu\n",
           physical, logical, rc, info.memory_size, quota);
    if (rc != 0 || info.memory_size != quota * 1024ULL) {
        fprintf(stderr, "Ascend slice view mismatch: got %llu KB, want %llu KB\n",
                info.memory_size, quota * 1024ULL);
        return 4;
    }
    return 0;
}

static int valid_ppu_uuid(const char *uuid) {
    if (!uuid || strlen(uuid) != 40 || strncmp(uuid, "GPU-", 4) != 0) return 0;
    for (int i = 4; i < 40; i++) {
        if (i == 12 || i == 17 || i == 22 || i == 27) {
            if (uuid[i] != '-') return 0;
        } else if (!((uuid[i] >= '0' && uuid[i] <= '9') || (uuid[i] >= 'a' && uuid[i] <= 'f'))) return 0;
    }
    return 1;
}

static int probe_ppu(unsigned long long quota) {
    typedef int (*init_fn)(void);
    typedef int (*shutdown_fn)(void);
    typedef int (*get_count_fn)(unsigned int *);
    typedef int (*get_handle_fn)(unsigned int, void **);
    typedef int (*get_uuid_fn)(void *, char *, unsigned int);
    typedef int (*get_memory_fn)(void *, struct hgml_memory *);
    const char *expected = getenv("GPUWORKS_PARTITION_PROBE_UUID");
    if (!valid_ppu_uuid(expected)) {
        fprintf(stderr, "invalid GPUWORKS_PARTITION_PROBE_UUID\n");
        return 2;
    }
    void *lib = dlopen("libhgml.so", RTLD_NOW | RTLD_LOCAL);
    if (!lib) {
        fprintf(stderr, "libhgml.so unavailable: %s\n", dlerror());
        return 5;
    }
    init_fn init = (init_fn)dlsym(lib, "hgmlInit_v2");
    shutdown_fn shutdown = (shutdown_fn)dlsym(lib, "hgmlShutdown");
    get_count_fn get_count = (get_count_fn)dlsym(lib, "hgmlDeviceGetCount_v2");
    get_handle_fn get_handle = (get_handle_fn)dlsym(lib, "hgmlDeviceGetHandleByIndex_v2");
    get_uuid_fn get_uuid = (get_uuid_fn)dlsym(lib, "hgmlDeviceGetUUID");
    get_memory_fn get_memory = (get_memory_fn)dlsym(lib, "hgmlDeviceGetMemoryInfo");
    if (!init || !shutdown || !get_count || !get_handle || !get_uuid || !get_memory) {
        fprintf(stderr, "required HGML identity/memory symbols unavailable\n");
        dlclose(lib);
        return 6;
    }
    unsigned int count = 0, selected_index = UINT_MAX, matches = 0;
    void *selected = NULL;
    struct hgml_memory memory = {0};
    int rc = init();
    int initialized = rc == 0;
    if (rc == 0) rc = get_count(&count);
    for (unsigned int i = 0; rc == 0 && i < count; i++) {
        void *device = NULL;
        char uuid[96] = {0};
        rc = get_handle(i, &device);
        if (rc == 0 && !device) rc = -1;
        if (rc == 0) rc = get_uuid(device, uuid, sizeof(uuid));
        if (rc == 0 && (!memchr(uuid, 0, sizeof(uuid)) || !valid_ppu_uuid(uuid))) rc = -1;
        if (rc == 0 && strcmp(uuid, expected) == 0) {
            selected = device;
            selected_index = i;
            matches++;
        }
    }
    if (rc == 0 && matches == 1) rc = get_memory(selected, &memory);
    printf("backend=ppu uuid=%s index=%u rc=%d count=%u matches=%u total_bytes=%llu quota_mib=%llu\n",
           expected, selected_index, rc, count, matches, memory.total, quota);
    int shutdown_rc = initialized ? shutdown() : 0;
    dlclose(lib);
    if (rc != 0 || shutdown_rc != 0 || matches != 1 || memory.total != quota * 1024ULL * 1024ULL) {
        fprintf(stderr, "PPU identity/slice view mismatch: got %llu bytes, want %llu bytes; shutdown_rc=%d\n",
                memory.total, quota * 1024ULL * 1024ULL, shutdown_rc);
        return 7;
    }
    return 0;
}

static int probe_hygon(unsigned long long quota) {
    typedef int (*get_count_fn)(int *);
    typedef int (*set_device_fn)(int);
    typedef int (*mem_info_fn)(size_t *, size_t *);
    typedef int (*malloc_fn)(void **, size_t);
    typedef int (*free_fn)(void *);
    const unsigned long long mib = 1024ULL * 1024ULL;
    if (quota >= SIZE_MAX / mib) {
        fprintf(stderr, "Hygon quota is too large for allocation probe\n");
        return 8;
    }
    void *lib = dlopen("libamdhip64.so", RTLD_NOW | RTLD_LOCAL);
    if (!lib) {
        fprintf(stderr, "libamdhip64.so unavailable: %s\n", dlerror());
        return 8;
    }
    get_count_fn get_count = (get_count_fn)dlsym(lib, "hipGetDeviceCount");
    set_device_fn set_device = (set_device_fn)dlsym(lib, "hipSetDevice");
    mem_info_fn mem_info = (mem_info_fn)dlsym(lib, "hipMemGetInfo");
    malloc_fn allocate = (malloc_fn)dlsym(lib, "hipMalloc");
    free_fn release = (free_fn)dlsym(lib, "hipFree");
    if (!get_count || !set_device || !mem_info || !allocate || !release) {
        fprintf(stderr, "required HIP memory symbols unavailable\n");
        dlclose(lib);
        return 9;
    }
    int count = 0;
    int rc = get_count(&count);
    if (rc != 0 || count != 1) {
        fprintf(stderr, "Hygon visible device count mismatch: rc=%d count=%d, want one\n", rc, count);
        dlclose(lib);
        return 10;
    }
    rc = set_device(0);
    if (rc != 0) {
        fprintf(stderr, "hipSetDevice(0) failed: rc=%d\n", rc);
        dlclose(lib);
        return 11;
    }
    size_t free_bytes = 0, total_bytes = 0;
    rc = mem_info(&free_bytes, &total_bytes);
    printf("backend=hygon rc=%d count=%d total_bytes=%zu quota_mib=%llu\n",
           rc, count, total_bytes, quota);
    if (rc != 0 || total_bytes != quota * mib) {
        fprintf(stderr, "Hygon slice view mismatch: got %zu bytes, want %llu bytes\n",
                total_bytes, quota * mib);
        dlclose(lib);
        return 12;
    }
    /* Prove that the allocator works before interpreting an OOM as quota
     * enforcement. free_bytes is not a stable quota signal on this driver. */
    void *small = NULL;
    rc = allocate(&small, (size_t)mib);
    if (rc != 0 || !small) {
        fprintf(stderr, "Hygon 1 MiB allocation failed: rc=%d\n", rc);
        dlclose(lib);
        return 13;
    }
    int free_rc = release(small);
    if (free_rc != 0) {
        fprintf(stderr, "Hygon 1 MiB free failed: rc=%d\n", free_rc);
        dlclose(lib);
        return 14;
    }
    void *over_limit = NULL;
    rc = allocate(&over_limit, (size_t)((quota + 1ULL) * mib));
    if (rc == 0 || over_limit) {
        if (over_limit) release(over_limit);
        fprintf(stderr, "Hygon over-quota allocation unexpectedly succeeded: rc=%d\n", rc);
        dlclose(lib);
        return 15;
    }
    /* HIP's public hipErrorOutOfMemory value is 2. Other errors do not prove
     * quota enforcement (e.g. a broken device or inaccessible driver). */
    if (rc != 2) {
        fprintf(stderr, "Hygon over-quota allocation failed for another reason: rc=%d, want OOM(2)\n", rc);
        dlclose(lib);
        return 16;
    }
    printf("backend=hygon quota_enforced=true over_limit_mib=%llu allocation_rc=%d\n", quota + 1ULL, rc);
    dlclose(lib);
    return 0;
}

int main(void) {
    const char *backend = getenv("GPUWORKS_PARTITION_PROBE_BACKEND");
    unsigned long long quota = quota_mib();
    /* Accept both the current dGPU vocabulary (ascend-dgpu, ppu-dgpu) and the
     * pre-rename wire names so an upgraded agent never bricks against a
     * probe image baked from the other era. */
    if (backend && (strcmp(backend, "ascend-vcann-logical") == 0 || strcmp(backend, "ascend-dgpu") == 0)) return probe_ascend(quota);
    if (backend && (strcmp(backend, "ppu-logical") == 0 || strcmp(backend, "ppu-dgpu") == 0)) return probe_ppu(quota);
    if (backend && strcmp(backend, "hygon-dgpu") == 0) return probe_hygon(quota);
    fprintf(stderr, "unsupported probe backend: %s\n", backend ? backend : "");
    return 2;
}
