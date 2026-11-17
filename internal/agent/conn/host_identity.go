package conn

import "gpuworks.ai/gpuworks/internal/agent/hostident"

// hostSystemUUID is the machine's firmware UUID the hello reports
// (proto.Hello.HostSystemUUID), read once per process. Tests replace it.
var hostSystemUUID = hostident.SystemUUID
