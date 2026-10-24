#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/syscall.h>
#include <unistd.h>

/* faccessat2 is syscall 439 on the two embedded Linux architectures. Older
 * build-host headers may omit it even when the target kernel supports it. */
#ifndef SYS_faccessat2
#if defined(__aarch64__) || defined(__x86_64__)
#define SYS_faccessat2 439
#else
#error unsupported gate architecture
#endif
#endif

int main(int argc, char **argv) {
    if (argc != 2) return 2;
    char path[PATH_MAX], canonical[PATH_MAX], expected[PATH_MAX];
    snprintf(path, sizeof(path), "%s/data/leaf/", argv[1]);
    if (syscall(SYS_faccessat2, AT_FDCWD, path, F_OK, AT_EACCESS) != 0) {
        perror("guest faccessat2");
        return 1;
    }
    snprintf(path, sizeof(path), "%s/link/../data/leaf/", argv[1]);
    snprintf(expected, sizeof(expected), "%s/data/leaf", argv[1]);
    if (!realpath(path, canonical) || strcmp(canonical, expected) != 0) {
        perror("guest realpath with symlink, parent and trailing slash");
        return 1;
    }
    int fd = open(argv[1], O_RDONLY | O_DIRECTORY);
    if (fd < 0 || syscall(SYS_faccessat2, fd, "data/leaf", F_OK, AT_EACCESS) != 0) {
        perror("guest dirfd-relative faccessat2");
        return 1;
    }
    errno = 0;
    int rc = syscall(SYS_faccessat2, fd, "absent", F_OK, AT_EACCESS);
    int saved_errno = errno;
    close(fd);
    if (rc != -1 || saved_errno != ENOENT) {
        fprintf(stderr, "missing guest path: rc=%d errno=%d\n", rc, saved_errno);
        return 1;
    }
    puts("path_resolution=pass");
    return 0;
}
