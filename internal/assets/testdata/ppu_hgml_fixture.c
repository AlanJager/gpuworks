/* HGML ABI fixture: distinct physical inventory and selected compute order. */
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
struct memory { unsigned long long total, free, used; };
struct memory_v2 { unsigned int version; unsigned long long total, reserved, free, used; };
static int count(void) { return getenv("COMPACT") ? 1 : 15; }
static int index_of(void *device) { return (int)(uintptr_t)device - 1; }
int hgmlInit_v2(void) { return getenv("INIT_FAIL") ? 999 : 0; }
int hgmlShutdown(void) { return getenv("SHUTDOWN_FAIL") ? 999 : 0; }
int hgmlDeviceGetCount_v2(unsigned int *n) {
    if (!n) return 2;
    if (getenv("COUNT_FAIL")) return 999;
    *n = getenv("EMPTY") ? 0 : (unsigned int)count(); return 0;
}
int hgmlDeviceGetHandleByIndex_v2(unsigned int i, void **device) {
    if (!device || i >= (unsigned int)count() || getenv("HANDLE_FAIL")) return 2;
    if (!getenv("HANDLE_EMPTY")) *device = (void *)(uintptr_t)(i+1);
    return 0;
}
int hgmlDeviceGetIndex(void *device, unsigned int *index) {
    int i = index_of(device);
    if (!index || i < 0 || i >= count()) return 2;
    *index = (unsigned int)i; return 0;
}
#ifndef OMIT_UUID
int hgmlDeviceGetUUID(void *device, char *uuid, unsigned int length) {
    int i = index_of(device);
    if (!uuid || length < 41 || i < 0 || i >= count()) return 2;
    if (getenv("UUID_FAIL")) return 999;
    if (getenv("UUID_EMPTY")) return 0;
    if (getenv("UUID_UNTERMINATED")) { memset(uuid, 'a', length); return 0; }
    if (getenv("COMPACT")) i = 8;
    if (getenv("DUPLICATE") && i == 9) i = 8;
    snprintf(uuid, length, "GPU-00000000-0000-0000-0000-%012d", i);
    return 0;
}
#endif
int hgmlDeviceGetMemoryInfo(void *device, struct memory *m) {
    int i = index_of(device);
    if (!m || i < 0 || i >= count()) return 2;
    if (getenv("MEMORY_FAIL")) return 999;
    if (getenv("COMPACT")) i = 8;
    m->total = (getenv("SLICED") && i == 8) || (getenv("WRONG_SLICE") && i == 0) ? 4ULL<<30 : 96ULL<<30;
    m->used = 1ULL<<30; m->free = m->total - m->used; return 0;
}
int hgmlDeviceGetMemoryInfo_v2(void *device, struct memory_v2 *m) {
    if (!m || m->version != (sizeof(*m) | (2U<<24))) return 25;
    struct memory v1;
    int rc = hgmlDeviceGetMemoryInfo(device, &v1);
    if (rc) return rc;
    m->total = v1.total; m->free = v1.free; m->used = v1.used; m->reserved = 123;
    return 0;
}
