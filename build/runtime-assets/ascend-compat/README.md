# Ascend runveil compatibility

`compat.c` supplies the missing user-to-logical device translation for the
hardware-version query in one qualified ARM64 CANN 8.5.1 `libtsdclient.so`:
SHA256 `4f6648daad462247e23782946b1a10b3d224d42d99aa2f468bd6b45c2a08676a`.
The affected library has no GNU build ID. Its `CheckDestructFlag` and
`GetInstance` call `GetPlatformInfo` before applying the visible-device map.

The runveil driver checks the library's SHA256 through guest symlinks/binds,
then passes its device/inode/size/mtime identity to the interposer. The native
hook checks the actual caller with `dladdr` and `stat`, so other CANN libraries,
unknown TSD builds and files replaced after preparation are forwarded without
translation. Only module 0 / info 1 (`MODULE_TYPE_SYSTEM` / `INFO_TYPE_VERSION`)
is translated. The complete `ASCEND_RT_VISIBLE_DEVICES` list is validated;
invalid, duplicate or out-of-range mappings fail rather than guessing a card.
No driver result is fabricated. Both global and already-loaded local driver
symbols are supported. This is a compatibility qualification, not a security
boundary against a workload modifying its own libraries or environment.

Only linux/arm64 ships this asset. Do not add another qualified digest without
checking both initialization call sites and proving the behavior on hardware.
The marker supplied by runveil independently avoids DCMI's failed external
container-detection child, whose `exit()` can deadlock in inherited CANN dump
locks. Both resources belong to the disposable container directory.

Build on ARM64 with glibc 2.28 or older (the current asset imports GLIBC_2.17):

```sh
build/runtime-assets/ascend-compat/build.sh /tmp/ascend-runveil-compat-arm64.so
```

Copy the result to `internal/assets/bin/`, update its entry in
`internal/assets/bin/manifest.sha256`, and run `make check-ascend-compat`.
The agent remains statically built with `CGO_ENABLED=0`.

Native dispatch, invalid mapping, driver failures and concurrent calls are
covered by `go test ./internal/assets -run TestAscendCompatibilityNativeDispatch`.
Go tests additionally cover qualification through guest symlinks/binds,
preload preservation, container cleanup and exec after agent restart.
