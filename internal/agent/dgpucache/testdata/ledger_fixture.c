/*
 * ledger_fixture.c writes the two region fixtures beside it from the shims'
 * own struct definitions, copied verbatim from the sources the packaged
 * libraries are built from:
 *
 *   ppu-ledger-v1.bin     struct vppu_region, ppu-slicing-shim
 *                         common/vppu_ledger.h (gpustack-operator fa399c03,
 *                         build/runtime-assets/ppu/build.sh)
 *   ascend-ledger-v1.bin  memory_shared_ledger_t, vcann-rt src/memory_ledger.c
 *                         as build/runtime-assets/ascend/patches/
 *                         0002-synchronous-memory-ledger.patch adds it
 *
 * Regenerate after either pin moves: cc ledger_fixture.c -o /tmp/f && /tmp/f .
 */
#include <stddef.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define VPPU_MAX_DEVICES 64
#define VPPU_REGION_MAGIC "VPPUREGN"
#define VPPU_REGION_MAGIC_BYTES 8
#define VPPU_REGION_VERSION 1u
#define VPPU_MAX_PROCESSES_PER_DEVICE 32

struct vppu_pid_state {
    uint64_t window_start_ns;
    uint64_t allow_ns;
    uint64_t last_step_ns;
    int32_t integral;
    int32_t last_error;
};

struct vppu_process_charge {
    int32_t pid;
    uint32_t reserved;
    uint64_t memory_bytes;
};

struct vppu_device_usage {
    uint64_t memory_quota_bytes;
    uint64_t memory_used_bytes;
    uint32_t sm_limit_percent;
    uint32_t sm_util_percent;
    int32_t lock_holder_pid;
    uint32_t reserved0;
    struct vppu_pid_state control;
    struct vppu_process_charge processes[VPPU_MAX_PROCESSES_PER_DEVICE];
};

struct vppu_region {
    char magic[VPPU_REGION_MAGIC_BYTES];
    uint32_t layout_version;
    uint32_t header_bytes;
    uint32_t device_slots;
    uint32_t process_slots;
    uint32_t reserved0;
    uint32_t reserved1;
    unsigned char lock_arena[VPPU_MAX_DEVICES];
    struct vppu_device_usage devices[VPPU_MAX_DEVICES];
};

_Static_assert(sizeof(struct vppu_process_charge) == 16, "process charge layout changed");
_Static_assert(sizeof(struct vppu_device_usage) == 576, "device usage layout changed");
_Static_assert(offsetof(struct vppu_device_usage, control) == 32, "the controller words may not move");
_Static_assert(offsetof(struct vppu_device_usage, processes) == 64, "process table offset changed");
_Static_assert(offsetof(struct vppu_region, lock_arena) == 32, "the lock arena may not move");
_Static_assert(offsetof(struct vppu_region, devices) == 96, "device table offset changed");
_Static_assert(sizeof(struct vppu_region) == 36960, "region size changed");

#define MEMORY_LEDGER_MAGIC UINT64_C(0x454E50554D454D31) /* ENPUMEM1 */
#define MEMORY_LEDGER_VERSION 1U
#define MEMORY_LEDGER_PROCESS_SLOTS 64U

typedef struct {
    int64_t pid;
    uint64_t start_time_ticks;
    uint64_t used;
} memory_process_slot_t;

typedef struct {
    uint64_t magic;
    uint32_t version;
    uint32_t structure_size;
    uint64_t quota;
    uint64_t used;
    memory_process_slot_t processes[MEMORY_LEDGER_PROCESS_SLOTS];
} memory_shared_ledger_t;

#define MIB (1024ULL * 1024ULL)

static int put(const char *dir, const char *name, const void *data, size_t size)
{
    char path[4096];
    snprintf(path, sizeof(path), "%s/%s", dir, name);
    FILE *file = fopen(path, "wb");
    if (file == NULL || fwrite(data, 1, size, file) != size || fclose(file) != 0) {
        perror(path);
        return 1;
    }
    return 0;
}

int main(int argc, char **argv)
{
    const char *dir = argc > 1 ? argv[1] : ".";

    /* A two-card PPU slice of 8 GiB per card: an engine and a worker on the
     * first card with its compute capped at 50%, the engine alone on the
     * second, uncapped. */
    struct vppu_region *region = calloc(1, sizeof(*region));
    if (region == NULL) {
        return 1;
    }
    memcpy(region->magic, VPPU_REGION_MAGIC, VPPU_REGION_MAGIC_BYTES);
    region->layout_version = VPPU_REGION_VERSION;
    region->header_bytes = (uint32_t)offsetof(struct vppu_region, devices);
    region->device_slots = VPPU_MAX_DEVICES;
    region->process_slots = VPPU_MAX_PROCESSES_PER_DEVICE;
    region->devices[0].memory_quota_bytes = 8192 * MIB;
    region->devices[0].sm_limit_percent = 50;
    region->devices[0].sm_util_percent = 37;
    region->devices[0].control.allow_ns = 12345678;
    region->devices[0].processes[0].pid = 41;
    region->devices[0].processes[0].memory_bytes = 6144 * MIB;
    region->devices[0].processes[3].pid = 57;
    region->devices[0].processes[3].memory_bytes = 1024 * MIB;
    region->devices[0].memory_used_bytes = 7168 * MIB;
    region->devices[1].memory_quota_bytes = 8192 * MIB;
    region->devices[1].sm_limit_percent = 100;
    region->devices[1].processes[0].pid = 41;
    region->devices[1].processes[0].memory_bytes = 2048 * MIB + 4096;
    region->devices[1].memory_used_bytes = 2048 * MIB + 4096;

    /* An Ascend slice of 8 GiB holding 6860 MiB across two processes. */
    memory_shared_ledger_t ledger;
    memset(&ledger, 0, sizeof(ledger));
    ledger.magic = MEMORY_LEDGER_MAGIC;
    ledger.version = MEMORY_LEDGER_VERSION;
    ledger.structure_size = (uint32_t)sizeof(ledger);
    ledger.quota = 8192 * MIB;
    ledger.processes[0].pid = 87;
    ledger.processes[0].start_time_ticks = 4242;
    ledger.processes[0].used = 6800 * MIB;
    ledger.processes[1].pid = 131;
    ledger.processes[1].start_time_ticks = 4300;
    ledger.processes[1].used = 60 * MIB;
    ledger.used = 6860 * MIB;

    return put(dir, "ppu-ledger-v1.bin", region, sizeof(*region))
           || put(dir, "ascend-ledger-v1.bin", &ledger, sizeof(ledger));
}
