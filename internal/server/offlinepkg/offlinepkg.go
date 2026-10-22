// Package offlinepkg builds the agent's offline package: one self-extracting
// shell script carrying the agent for every platform the server embeds and
// the install script, for a node that cannot download from the server (the
// server dials it, and there is no SSH to upload through). The operator
// copies the file to the node and runs it with the node's token; the token is
// never in the file, which is the same for every node.
//
// The package picks its agent with uname, unpacks it next to install.sh in a
// temporary directory, and runs install.sh with GPUWORKS_AGENT_BINARY — the
// same install the SSH onboarding runs.
package offlinepkg

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gpuworks.ai/gpuworks/internal/server/installscript"
)

// FileName is the package as it is served (/dl/FileName) and as the install
// command runs it.
const FileName = "gpuworks-agent.run"

// ChecksumName is the package's sha256 in sha256sum's format.
const ChecksumName = FileName + ".sha256"

// Platform is one agent the package can carry.
type Platform struct {
	// Binary is the agent's name under /dl/ (proto.AgentBinaryName).
	Binary string
	// Uname is the case pattern `$(uname -s)/$(uname -m)` matches.
	Uname string
	// Label names the platform in the package's messages.
	Label string
}

// Platforms are the agents the package carries when the server has them:
// Linux on x86-64 and Arm, macOS on Apple silicon (an Intel Mac cannot run
// the Metal engines).
var Platforms = []Platform{
	{Binary: "gpuworks-agent-linux-amd64", Uname: "Linux/x86_64|Linux/amd64", Label: "Linux x86_64"},
	{Binary: "gpuworks-agent-linux-arm64", Uname: "Linux/aarch64|Linux/arm64", Label: "Linux arm64"},
	{Binary: "gpuworks-agent-darwin-arm64", Uname: "Darwin/arm64", Label: "macOS (Apple silicon)"},
}

// ErrNoAgents is Build's error when the server embeds no agent at all (a
// development build without cross-compiled agents).
var ErrNoAgents = errors.New("offlinepkg: the server has no agent binaries to package")

// Build renders the package. read returns an agent binary by its /dl/ name;
// a platform whose binary is missing is left out, and the package says so on
// that platform.
func Build(version string, read func(name string) ([]byte, error)) ([]byte, error) {
	script, err := gzipped([]byte(installscript.Script))
	if err != nil {
		return nil, err
	}
	type part struct {
		platform Platform
		data     []byte
	}
	var parts []part
	for _, p := range Platforms {
		bin, err := read(p.Binary)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("offlinepkg: read %s: %w", p.Binary, err)
		}
		packed, err := gzipped(bin)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part{p, packed})
	}
	if len(parts) == 0 {
		return nil, ErrNoAgents
	}
	// The header names byte offsets of the payloads that follow it, and its
	// own length depends on how many digits those take: render until the
	// length stops changing (two passes in practice).
	var header string
	for length := 0; ; {
		offset := length + 1 // tail -c +N counts from 1
		cases := make([]string, 0, len(parts))
		labels := make([]string, 0, len(parts))
		scriptAt := offset
		offset += len(script)
		for _, p := range parts {
			cases = append(cases, fmt.Sprintf("  %s) at=%d; size=%d ;;", p.platform.Uname, offset, len(p.data)))
			labels = append(labels, p.platform.Label)
			offset += len(p.data)
		}
		header = renderHeader(version, strings.Join(labels, ", "), strings.Join(cases, "\n"), scriptAt, len(script))
		if len(header) == length {
			break
		}
		length = len(header)
	}
	var b bytes.Buffer
	b.WriteString(header)
	b.Write(script)
	for _, p := range parts {
		b.Write(p.data)
	}
	return b.Bytes(), nil
}

func renderHeader(version, labels, cases string, scriptAt, scriptSize int) string {
	return strings.NewReplacer(
		"@VERSION@", version,
		"@FILE@", FileName,
		"@PLATFORMS@", labels,
		"@CASES@", cases,
		"@SCRIPT_AT@", fmt.Sprint(scriptAt),
		"@SCRIPT_SIZE@", fmt.Sprint(scriptSize),
	).Replace(headerTemplate)
}

// headerTemplate is the package's shell part. It stops at `exit` before the
// payload, so the shell never reads the binary data after it.
const headerTemplate = `#!/bin/sh
# gpuworks agent offline package @VERSION@: the agent for @PLATFORMS@ and
# its installer, for a node that cannot download from the server. Copy it to
# the node and run it with the command the console gives, for example
#   sh @FILE@ --token TOKEN --connect-mode server_dial --listen 0.0.0.0:18800
# The arguments are install.sh's.
set -eu
self="$0"
if [ ! -f "$self" ]; then
  echo "gpuworks: run the package as a file: sh @FILE@ --token ..." >&2
  exit 2
fi
case "$(uname -s)/$(uname -m)" in
@CASES@
  *) echo "gpuworks: this package has no agent for $(uname -s) $(uname -m); it carries @PLATFORMS@" >&2; exit 1 ;;
esac
for tool in tail head gzip mktemp; do
  command -v "$tool" >/dev/null 2>&1 || { echo "gpuworks: the offline package needs $tool" >&2; exit 1; }
done
work=$(mktemp -d "${TMPDIR:-/tmp}/gpuworks-offline.XXXXXX")
trap 'rm -rf "$work"' EXIT
trap 'exit 130' INT TERM
unpack() {
  tail -c +"$1" "$self" | head -c "$2" | gzip -dc > "$3"
}
unpack @SCRIPT_AT@ @SCRIPT_SIZE@ "$work/install.sh"
unpack "$at" "$size" "$work/gpuworks-agent"
chmod +x "$work/gpuworks-agent"
status=0
GPUWORKS_AGENT_BINARY="$work/gpuworks-agent" sh "$work/install.sh" "$@" || status=$?
exit "$status"
`

func gzipped(data []byte) ([]byte, error) {
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	if _, err := gz.Write(data); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Server serves the package and its checksum, building it on the first
// request into dir (it is tens of megabytes; a restart builds it again, so
// a new server version never serves an old package).
type Server struct {
	dir     string
	version string
	read    func(name string) ([]byte, error)

	mu    sync.Mutex
	built bool
	sum   string
}

// NewServer serves the package built from read's agents into dir.
func NewServer(dir, version string, read func(name string) ([]byte, error)) *Server {
	return &Server{dir: dir, version: version, read: read}
}

// Handles reports whether path is one Serve answers.
func Handles(path string) bool {
	return path == "/dl/"+FileName || path == "/dl/"+ChecksumName
}

func (s *Server) path() string { return filepath.Join(s.dir, FileName) }

// ensure builds the package once; a failed build is tried again next time.
func (s *Server) ensure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.built {
		return nil
	}
	data, err := Build(s.version, s.read)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, "."+FileName+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.path()); err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	s.sum, s.built = hex.EncodeToString(sum[:]), true
	return nil
}

// ServeHTTP serves /dl/FileName and /dl/ChecksumName.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := s.ensure(); err != nil {
		if errors.Is(err, ErrNoAgents) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, "build the offline package: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if r.URL.Path == "/dl/"+ChecksumName {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "%s  %s\n", s.sum, FileName)
		return
	}
	f, err := os.Open(s.path())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+FileName+`"`)
	http.ServeContent(w, r, FileName, st.ModTime(), f)
}
