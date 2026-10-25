#define _GNU_SOURCE
#include <dlfcn.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <limits.h>

/* The launcher SHA256-verifies the affected TSD, then supplies its file
 * identity. dladdr identifies the actual caller, including dlopen and symlink
 * aliases. A different/replaced library must not be mapped a second time. */
static int affected_caller(void *address) {
    const char *expected = getenv("GPUWORKS_ASCEND_TSD_FILE");
    if (!expected || !*expected) return 0;
    Dl_info caller = {0};
    struct stat st;
    if (!dladdr(address, &caller) || !caller.dli_fname || stat(caller.dli_fname, &st) != 0) return 0;
    char identity[160];
    int n = snprintf(identity, sizeof(identity), "%llu:%llu:%llu:%lld",
        (unsigned long long)st.st_dev, (unsigned long long)st.st_ino,
        (unsigned long long)st.st_size,
        (long long)st.st_mtim.tv_sec * 1000000000LL + st.st_mtim.tv_nsec);
    return n > 0 && (size_t)n < sizeof(identity) && strcmp(expected, identity) == 0;
}

/* Validate the entire list, including IDs after the requested position.
 * The runtime emits strict decimal lists; reject malformed/duplicate IDs
 * instead of silently choosing a different device. */
static int logical_device(uint32_t user, uint32_t *logical) {
    const char *p = getenv("ASCEND_RT_VISIBLE_DEVICES");
    if (!p || !*p) { *logical = user; return 0; }
    uint32_t ids[256];
    size_t count = 0;
    for (;;) {
        if (*p < '0' || *p > '9' || count == sizeof(ids)/sizeof(ids[0])) return -1;
        uint32_t id = 0;
        do {
            unsigned digit = (unsigned)(*p++ - '0');
            if (id > (UINT32_MAX - digit)/10) return -1;
            id = id * 10 + digit;
        } while (*p >= '0' && *p <= '9');
        for (size_t i = 0; i < count; i++) if (ids[i] == id) return -1;
        ids[count++] = id;
        if (!*p) break;
        if (*p++ != ',') return -1;
    }
    if (user >= count) return -1;
    *logical = ids[user];
    return 0;
}

__attribute__((visibility("default")))
int halGetDeviceInfo(uint32_t device, int32_t module, int32_t info, int64_t *value) {
    typedef int (*query_fn)(uint32_t, int32_t, int32_t, int64_t *);
    query_fn next = (query_fn)dlsym(RTLD_NEXT, "halGetDeviceInfo");
    if (!next) {
        /* CANN can dlopen the driver with RTLD_LOCAL. Reuse that loaded
         * object rather than loading a different driver or fabricating data. */
        void *driver = dlopen("libascend_hal.so", RTLD_LAZY | RTLD_NOLOAD);
        if (driver) {
            next = (query_fn)dlsym(driver, "halGetDeviceInfo");
            dlclose(driver);
        }
    }
    if (!next || next == halGetDeviceInfo) {
        fputs("gpuworks: Ascend compatibility cannot resolve halGetDeviceInfo\n", stderr);
        return 87;
    }
    if (module == 0 && info == 1) {
        if (affected_caller(__builtin_return_address(0)) && logical_device(device, &device) != 0) {
            fputs("gpuworks: invalid TSD visible-device mapping\n", stderr);
            return 87;
        }
    }
    return next(device, module, info, value);
}
