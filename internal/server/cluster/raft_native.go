//go:build linux && dqlite

package cluster

// bootstrapMemberID is go-dqlite's BootstrapID. The server does not import the
// dqlite root package, because that package pulls in the cgo bindings.
const bootstrapMemberID uint64 = 0x2dc171858c3155be
