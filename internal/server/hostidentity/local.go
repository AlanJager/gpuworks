package hostidentity

import "sync"

// Local is this machine's facts, read once per process: the machine does
// not change while the server runs, and the root-written file is in place
// before the service starts (ExecStartPre). Reading it costs a few small
// file reads (an ioreg call on macOS), never a request's worth of work.
var Local = sync.OnceValue(local)
