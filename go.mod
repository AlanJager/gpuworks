module gpuworks.ai/gpuworks

go 1.26.11

require (
	github.com/coder/websocket v1.8.15
	github.com/glebarez/go-sqlite v1.21.2
	github.com/glebarez/sqlite v1.11.0
	github.com/go-chi/chi/v5 v5.4.0
	github.com/itchyny/gojq v0.12.19
	github.com/klauspost/compress v1.20.3
	github.com/libp2p/go-yamux/v5 v5.1.0
	go.starlark.net v0.0.0-20260828210309-6dd8f160a37f
	golang.org/x/crypto v0.57.0
	golang.org/x/net v0.60.0
	golang.org/x/sys v0.48.0
	golang.org/x/term v0.46.0
	golang.org/x/tools v0.49.0
	gopkg.in/yaml.v3 v3.0.1
	gorm.io/gorm v1.31.2
	gpuworks.ai/gpuworks/pkg/extapi v0.0.0
	gpuworks.ai/gpuworks/pkg/pluginsdk v0.0.0
	modernc.org/sqlite v1.23.1
	pgregory.net/rapid v1.3.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.3.0 // indirect
	github.com/itchyny/timefmt-go v0.1.8 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	github.com/libp2p/go-buffer-pool v0.0.2 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	modernc.org/libc v1.22.5 // indirect
	modernc.org/mathutil v1.5.0 // indirect
	modernc.org/memory v1.5.0 // indirect
)

// pkg/extapi is a public module of its own (the plugin wire formats, which
// plugins import without the server); the server builds against this copy.
// Go applies replace directives only in the main module, so a module that
// depends on this one needs its own replace for pkg/extapi, or a go.work
// that uses this checkout (see pkg/extapi/README.md); requiring a tagged
// pkg/extapi/vX.Y.Z here replaces v0.0.0 before the module is published.
replace gpuworks.ai/gpuworks/pkg/extapi => ./pkg/extapi

// pkg/pluginsdk, what plugins are built with, is a module of its own for
// the same reason; the contract tests build a fake plugin against it.
replace gpuworks.ai/gpuworks/pkg/pluginsdk => ./pkg/pluginsdk
