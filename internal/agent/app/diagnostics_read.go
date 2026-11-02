package app

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"path/filepath"
)

// StreamDiagnostics writes the node's part of a diagnostics collection as a
// tar: the agent's log (agent.log and its previous generation) and the named
// debug sessions' files. A file that is not there is left out; the server
// reads what arrived.
func (n *NodeRuntime) StreamDiagnostics(ctx context.Context, sessions []string, w io.Writer) error {
	dir := n.agentLogDir()
	files := []string{"agent.log", "agent.log.1"}
	for _, id := range sessions {
		if debugSessionID.MatchString(id) {
			files = append(files, filepath.Join("debug", id+".log"))
		}
	}
	tw := tar.NewWriter(w)
	for _, name := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := addTarFile(tw, filepath.Join(dir, name), filepath.ToSlash(name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return tw.Close()
}

// addTarFile copies the file's size as it is when opened: a log still being
// written grows past it, and the tail it gains is left for the next read.
func addTarFile(tw *tar.Writer, path, name string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: info.Size(), ModTime: info.ModTime()}); err != nil {
		return err
	}
	_, err = io.CopyN(tw, f, info.Size())
	return err
}
