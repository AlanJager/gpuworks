/* Link against an existing pinned SDK; this fixture never rebuilds dqlite.
 * vfs.h is from that SDK's matching pinned source directory. */
#include <assert.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#include <sqlite3.h>
#include "vfs.h"

#define REGION_SIZE 32768

static sqlite3_file *open_file(sqlite3_vfs *vfs)
{
    sqlite3_file *file = calloc(1, (size_t)vfs->szOsFile);
    int flags = SQLITE_OPEN_CREATE | SQLITE_OPEN_READWRITE | SQLITE_OPEN_MAIN_DB;
    assert(file != NULL);
    assert(vfs->xOpen(vfs, "gap.db", file, flags, &flags) == SQLITE_OK);
    return file;
}

static volatile unsigned char *map(sqlite3_file *file, int index, int extend)
{
    void volatile *region = NULL;
    assert(file->pMethods->xShmMap(file, index, REGION_SIZE, extend, &region) == SQLITE_OK);
    return region;
}

static void unmap(sqlite3_file *file)
{
    assert(file->pMethods->xShmUnmap(file, 0) == SQLITE_OK);
}

static void close_file(sqlite3_file *file)
{
    unmap(file);
    assert(file->pMethods->xClose(file) == SQLITE_OK);
    free(file);
}

static void region_gap(sqlite3_vfs *vfs)
{
    sqlite3_file *file = open_file(vfs);
    volatile unsigned char *zero = map(file, 0, 1);
    assert(zero != NULL);
    zero[128] = 31;
    volatile unsigned char *two = map(file, 2, 1);
    volatile unsigned char *one = map(file, 1, 0);
    assert(one != NULL && two != NULL && one != two);
    one[0] = 11;
    one[REGION_SIZE - 1] = 12;
    two[0] = 21;
    two[REGION_SIZE - 1] = 22;
    assert(map(file, 0, 0) == zero && zero[128] == 31);
    assert(map(file, 1, 0) == one && one[0] == 11 && one[REGION_SIZE - 1] == 12);
    assert(map(file, 2, 0) == two && two[0] == 21 && two[REGION_SIZE - 1] == 22);
    unmap(file);
    /* No region is mapped by this handle now, while the shared file exists. */
    two = map(file, 2, 0);
    assert(two != NULL && two[0] == 21 && two[REGION_SIZE - 1] == 22);
    assert(map(file, 0, 0)[128] == 31);
    assert(map(file, 1, 0)[0] == 11);
    close_file(file);
}

static void shared_existing_region(sqlite3_vfs *vfs)
{
    sqlite3_file *writer = open_file(vfs);
    volatile unsigned char *writer_regions[3];
    for (int i = 0; i < 3; i++) {
        writer_regions[i] = map(writer, i, 1);
        assert(writer_regions[i] != NULL);
        writer_regions[i][128] = (unsigned char)(40 + i);
    }
    sqlite3_file *reader = open_file(vfs);
    assert(map(reader, 0, 0)[128] == 40);
    /* extend=false must still fill missing mappings when the file is large enough. */
    volatile unsigned char *two = map(reader, 2, 0);
    volatile unsigned char *one = map(reader, 1, 0);
    assert(one != NULL && two != NULL && one[128] == 41 && two[128] == 42);
    two[129] = 73;
    one[129] = 74;
    assert(writer_regions[2][129] == 73 && writer_regions[1][129] == 74);
    close_file(reader);
    assert(writer_regions[2][129] == 73 && writer_regions[1][129] == 74);
    close_file(writer);
}

static void no_extend_missing_region(sqlite3_vfs *vfs)
{
    sqlite3_file *file = open_file(vfs);
    assert(map(file, 2, 0) == NULL);
    assert(map(file, 0, 0) == NULL);
    assert(map(file, 0, 1) != NULL);
    assert(map(file, 2, 0) == NULL);
    assert(map(file, 1, 0) == NULL);
    /* A second handle observes that the failed requests did not extend the file. */
    sqlite3_file *other = open_file(vfs);
    assert(map(other, 1, 0) == NULL);
    assert(map(file, 1, 1) != NULL);
    assert(map(other, 2, 0) == NULL);
    close_file(other);
    close_file(file);
}

static void repeated_map_unmap(sqlite3_vfs *vfs)
{
    sqlite3_file *file = open_file(vfs);
    unmap(file);
    unmap(file);
    for (int round = 0; round < 3; round++) {
        for (int i = 0; i < 3; i++) {
            volatile unsigned char *region = map(file, i, 1);
            assert(region != NULL);
            region[128] = (unsigned char)(round + i);
            assert(map(file, i, 0) == region);
            assert(map(file, i, 1) == region);
            assert(region[128] == (unsigned char)(round + i));
        }
        unmap(file);
        unmap(file);
    }
    close_file(file);
}

int main(int argc, char **argv)
{
    assert(argc == 2);
    /* The production SDK currently targets Linux amd64; region 0 -> 2 assumes
     * the OS page size is at most SQLite's 32 KiB WAL-index region. */
    assert(sysconf(_SC_PAGESIZE) > 0 && sysconf(_SC_PAGESIZE) <= REGION_SIZE);
    assert(sqlite3_initialize() == SQLITE_OK);
    sqlite3_int64 baseline = sqlite3_memory_used();
    sqlite3_vfs vfs = {0};
    struct vfsConfig config = {.name = "gap", .page_size = 512};
    assert(VfsInit(&vfs, &config) == SQLITE_OK);
    if (strcmp(argv[1], "region-gap") == 0) {
        region_gap(&vfs);
    } else if (strcmp(argv[1], "shared-existing-region") == 0) {
        shared_existing_region(&vfs);
    } else if (strcmp(argv[1], "no-extend-missing-region") == 0) {
        no_extend_missing_region(&vfs);
    } else if (strcmp(argv[1], "repeated-map-unmap") == 0) {
        repeated_map_unmap(&vfs);
    } else {
        fprintf(stderr, "unknown case: %s\n", argv[1]);
        return 2;
    }
    VfsClose(&vfs);
    /* VfsInit registers a process-wide extension; release that registration
     * before comparing the fixture's SQLite allocation baseline. */
    sqlite3_reset_auto_extension();
    assert(sqlite3_memory_used() == baseline);
    assert(sqlite3_shutdown() == SQLITE_OK);
    printf("PASS %s\n", argv[1]);
    return 0;
}
