module gpuworks.ai/gpuworks/pkg/pluginsdk

go 1.26.11

require gpuworks.ai/gpuworks/pkg/extapi v0.0.0

// The SDK builds against the wire formats in this repository. A plugin's
// own module needs the same replace (or a go.work), since Go applies
// replace only in the main module, until pkg/extapi and pkg/pluginsdk are
// tagged and required by version.
replace gpuworks.ai/gpuworks/pkg/extapi => ../extapi
