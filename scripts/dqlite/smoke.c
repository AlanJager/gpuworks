#include <dqlite.h>
#include <sqlite3.h>
#include <stdio.h>
#include <uv.h>

int main(int argc, char **argv)
{
    const char *options[] = {
        "ENABLE_DBSTAT_VTAB", "ENABLE_PREUPDATE_HOOK", "ENABLE_SESSION", "THREADSAFE=1"
    };
    dqlite_node *node = NULL;
    int rc;

    if (argc != 2) {
        fprintf(stderr, "usage: dqlite-smoke EMPTY_DATA_DIRECTORY\n");
        return 2;
    }
    for (unsigned i = 0; i < sizeof(options) / sizeof(options[0]); i++) {
        if (!sqlite3_compileoption_used(options[i])) {
            fprintf(stderr, "missing SQLite option: %s\n", options[i]);
            return 1;
        }
    }
    printf("dqlite=%s sqlite=%s libuv=%s\n",
           dqlite_version_string, sqlite3_libversion(), uv_version_string());
    rc = dqlite_node_create(1, "127.0.0.1:0", argv[1], &node);
    if (rc == 0) {
        rc = dqlite_node_set_bind_address(node, "127.0.0.1:0");
    }
    if (rc == 0) {
        rc = dqlite_node_start(node);
    }
    if (rc != 0) {
        fprintf(stderr, "dqlite start failed (%d): %s\n", rc, dqlite_node_errmsg(node));
    } else {
        puts("start=ok");
        rc = dqlite_node_stop(node);
        printf("stop=%d\n", rc);
    }
    dqlite_node_destroy(node);
    return rc != 0;
}
