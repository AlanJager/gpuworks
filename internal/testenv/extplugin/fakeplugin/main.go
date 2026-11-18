// Command fakeplugin is a plugin built only by tests. It imports what a
// third-party plugin may import (pkg/pluginsdk, pkg/extapi and the
// standard library), and its flags make it misbehave on purpose:
//
//	--id, --version, --manifest    what it answers initialize with
//	--requires cap@min-max         a capability it cannot run without
//	--refuse CODE:reason           refuse initialize
//	--push kind=<json>             push this table once initialized (repeatable)
//	--crash-after d                exit 2 this long after initialize
//	--exit-at-start n              exit n before serving anything
//	--hang init|health             never answer initialize, or health
//	--probe-open path              try to read path and write in its data
//	                               directory, and report on stderr
//	--probe-pid pid                try to reach another process's sockets
//	                               and signal it, and report on stderr
//	--push-raw kind@gen=<json>     push this body at this generation with a
//	                               bare host API request, past the SDK's
//	                               own generation counter (repeatable)
//	--push-oversize kind           push an envelope over the size limit
//	--push-every d                 push a node_holds table every d, as an
//	                               authority plugin refreshing its state does
//	--push-size n                  hold nodes 1..n in that table (default
//	                               100); each push changes 1% of them
//	--grow-rss MiB                 once initialized, hold this much more
//	                               resident memory, and start a child (in
//	                               its process group) that holds as much
//	--read-nodes salt              read the first inventory page and the
//	                               identity of its nodes with salt, and
//	                               record the outcome as kind "read_nodes"
//
// --requires takes a comma-separated list. Without --manifest, --id and
// --version the plugin answers with its installed package's: the manifest
// found above its executable. A file fakeplugin.args in its data directory
// (its working directory) adds one flag or value per line, so a test sets
// how an installed copy behaves. Reports are JSON lines on stderr with msg
// "probe"; the outcome of every push is also appended to results.jsonl in
// the data directory.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
	"gpuworks.ai/gpuworks/pkg/pluginsdk"
)

type pushes []string

func (p *pushes) String() string     { return strings.Join(*p, ",") }
func (p *pushes) Set(v string) error { *p = append(*p, v); return nil }

// held keeps --grow-rss's memory reachable.
var held []byte

func main() {
	if len(os.Args) == 3 && os.Args[1] == "grow" {
		growChild(os.Args[2])
		return
	}
	if len(os.Args) < 2 || os.Args[1] != "serve" {
		fmt.Fprintln(os.Stderr, "usage: fakeplugin serve [flags]")
		os.Exit(64)
	}
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	id := fs.String("id", "", "package id")
	version := fs.String("version", "", "package version")
	manifest := fs.String("manifest", "", "manifest digest")
	requires := fs.String("requires", "table.banners@1-1", "required capabilities cap@min-max[,...]")
	var raw pushes
	fs.Var(&raw, "push-raw", "kind@generation=<json> to push with a bare request")
	oversize := fs.String("push-oversize", "", "kind to push over the size limit")
	refuse := fs.String("refuse", "", "refuse initialize with CODE:reason")
	var push pushes
	fs.Var(&push, "push", "kind=<json> to push once initialized")
	crashAfter := fs.Duration("crash-after", -1, "exit 2 this long after initialize")
	exitAtStart := fs.Int("exit-at-start", -1, "exit with this code before serving")
	hang := fs.String("hang", "", "init or health")
	probeOpen := fs.String("probe-open", "", "path to try to read")
	probePID := fs.Int("probe-pid", 0, "another process to try to reach")
	pushEvery := fs.Duration("push-every", 0, "push a node_holds table this often")
	pushSize := fs.Int("push-size", 100, "nodes held by each periodic push")
	growRSS := fs.Int("grow-rss", 0, "MiB of resident memory to hold, in this process and a child")
	readNodes := fs.String("read-nodes", "", "salt to read the node inventory and identity with")
	_ = fs.Parse(append(os.Args[2:], argsFile()...))
	fillFromPackage(id, version, manifest)

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if *exitAtStart >= 0 {
		log.Error("exiting at start on purpose", "code", *exitAtStart)
		os.Exit(*exitAtStart)
	}
	if *probePID > 0 {
		probeSibling(log, *probePID)
	}
	cfg := pluginsdk.Config{ID: *id, Version: *version, ManifestSHA256: extapi.Digest(*manifest), Logger: log}
	if *requires != "" {
		cfg.Requires = map[string]extapi.Range{}
		for _, one := range strings.Split(*requires, ",") {
			name, rng, err := parseRange(one)
			if err != nil {
				log.Error("bad --requires", "err", err)
				os.Exit(64)
			}
			cfg.Requires[name] = rng
		}
	}
	if code, reason, ok := strings.Cut(*refuse, ":"); ok {
		cfg.Init = func(context.Context, *pluginsdk.Plugin) error {
			return pluginsdk.Refuse(reason, code, "refused on purpose", nil)
		}
	}
	if *hang == "init" {
		cfg.Init = func(context.Context, *pluginsdk.Plugin) error { select {} }
	}
	if *hang == "health" {
		cfg.Health = func(context.Context) extapi.Health { select {} }
	}
	cfg.Start = func(ctx context.Context, p *pluginsdk.Plugin) error {
		if *probeOpen != "" {
			probeFiles(log, *probeOpen)
		}
		if *growRSS > 0 {
			grow(log, *growRSS)
		}
		for _, kv := range push {
			kind, body, _ := strings.Cut(kv, "=")
			r, err := p.PushState(ctx, kind, json.RawMessage(body))
			record(kind, r.Generation, err)
			if err != nil {
				log.Error("push refused", "kind", kind, "err", err.Error())
				continue
			}
			log.Info("pushed", "kind", kind, "generation", r.Generation, "skipped", r.Skipped)
		}
		for _, kv := range raw {
			target, body, _ := strings.Cut(kv, "=")
			kind, g, _ := strings.Cut(target, "@")
			gen, _ := strconv.ParseUint(g, 10, 64)
			envelope, _ := json.Marshal(extapi.StatePush{Kind: kind, Schema: 1, Generation: gen, Body: json.RawMessage(body)})
			record(kind, gen, putRaw(ctx, p, kind, envelope))
		}
		if *readNodes != "" {
			readNodesOnce(ctx, p, *readNodes)
		}
		if *oversize != "" {
			pad := strings.Repeat("x", extapi.MaxPushBytes)
			envelope := []byte(`{"kind":"` + *oversize + `","schema":1,"generation":1,"body":{"pad":"` + pad + `"}}`)
			record(*oversize, 1, putRaw(ctx, p, *oversize, envelope))
		}
		if *pushEvery > 0 {
			// Start's context ends at shutdown; pushing alongside lets
			// --crash-after still fire.
			go pushPeriodically(ctx, log, p, *pushEvery, *pushSize)
		}
		if *crashAfter >= 0 {
			select {
			case <-time.After(*crashAfter):
				log.Error("crashing on purpose")
				os.Exit(2)
			case <-ctx.Done():
			}
		}
		return nil
	}
	pluginsdk.Main(cfg)
}

// pushPeriodically pushes a node_holds table holding nodes 1..size every
// interval until ctx ends. Each push differs from the one before in 1% of
// its entries (at least one), a different slice of them each time, so the
// server writes a full replace whose difference from the stored table is
// small.
func pushPeriodically(ctx context.Context, log *slog.Logger, p *pluginsdk.Plugin, every time.Duration, size int) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for round := 0; ; round++ {
		r, err := p.PushState(ctx, extapi.KindNodeHolds, holdsTable(size, round))
		record(extapi.KindNodeHolds, r.Generation, err)
		if err != nil && ctx.Err() == nil {
			log.Error("periodic push refused", "err", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// holdsTable is round's table of size holds. The entries fall into slices
// of 1% (at least one entry); round r flips the code of slice r modulo the
// slice count between ACME_MAINTENANCE and ACME_QUARANTINE, so rounds r-1
// and r differ in exactly that slice.
func holdsTable(size, round int) *extapi.NodeHoldsV1 {
	size = max(1, size)
	per := max(1, size/100)
	slices := (size + per - 1) / per
	t := &extapi.NodeHoldsV1{Nodes: make(map[string]extapi.Notice, size)}
	for i := 0; i < size; i++ {
		flips := 0
		if s := i / per; round >= s {
			flips = (round-s)/slices + 1
		}
		code := "ACME_MAINTENANCE"
		if flips%2 == 1 {
			code = "ACME_QUARANTINE"
		}
		t.Nodes[strconv.Itoa(i+1)] = extapi.Notice{Code: code}
	}
	return t
}

// argsFile reads fakeplugin.args from the working directory (the data
// directory): one flag or value per line.
func argsFile() []string {
	raw, err := os.ReadFile("fakeplugin.args")
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// fillFromPackage answers with the installed package's id, version and
// manifest digest, read from the manifest.json above the executable, for
// whatever the flags left empty.
func fillFromPackage(id, version, digest *string) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	for dir := filepath.Dir(exe); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
		if err != nil {
			continue
		}
		var m struct{ ID, Version string }
		if json.Unmarshal(raw, &m) == nil {
			setIfEmpty(id, m.ID)
			setIfEmpty(version, m.Version)
			setIfEmpty(digest, string(extapi.DigestOf(raw)))
		}
		break
	}
	setIfEmpty(id, "acme_fake")
	setIfEmpty(version, "1.0.0")
}

func setIfEmpty(p *string, v string) {
	if *p == "" {
		*p = v
	}
}

// putRaw sends envelope to the host API as it is.
func putRaw(ctx context.Context, p *pluginsdk.Plugin, kind string, envelope []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://gwx-host"+extapi.PathHostState+kind, bytes.NewReader(envelope))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", extapi.ContentTypeJSON)
	resp, err := p.HostClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		return nil
	}
	var w extapi.WireError
	_ = json.NewDecoder(resp.Body).Decode(&w)
	return &pluginsdk.Error{Status: resp.StatusCode, Wire: w}
}

// readNodesOnce reads the first inventory page and the identity of its
// nodes, and records how many nodes each answered and the first node's
// host digest, or why it was refused.
func readNodesOnce(ctx context.Context, p *pluginsdk.Plugin, salt string) {
	page, err := p.Nodes(ctx, extapi.NodesQuery{})
	if err != nil {
		record("read_nodes", 0, err)
		return
	}
	ids := make([]uint64, len(page.Nodes))
	for i, n := range page.Nodes {
		ids[i] = n.ID
	}
	line := map[string]any{"kind": "read_nodes", "ok": true, "nodes": len(page.Nodes)}
	if len(ids) > 0 {
		ans, err := p.NodeIdentity(ctx, salt, ids)
		if err != nil {
			record("read_nodes", 0, err)
			return
		}
		line["identities"] = len(ans.Nodes)
		if len(ans.Nodes) > 0 && ans.Nodes[0].Host != nil {
			line["host_h"] = ans.Nodes[0].Host.H
		}
	}
	appendResult(line)
}

// record appends a push's outcome to results.jsonl in the data directory.
func record(kind string, generation uint64, err error) {
	line := map[string]any{"kind": kind, "generation": generation, "ok": err == nil}
	var se *pluginsdk.Error
	var xe *extapi.Error
	switch {
	case errors.As(err, &se):
		line["code"], line["status"] = se.Wire.Code, se.Status
	case errors.As(err, &xe):
		line["code"] = xe.Code
	case err != nil:
		line["error"] = err.Error()
	}
	appendResult(line)
}

// appendResult appends one JSON line to results.jsonl in the data
// directory.
func appendResult(line map[string]any) {
	b, _ := json.Marshal(line)
	f, ferr := os.OpenFile("results.jsonl", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if ferr != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

func parseRange(s string) (string, extapi.Range, error) {
	name, rng, ok := strings.Cut(s, "@")
	lo, hi, ok2 := strings.Cut(rng, "-")
	min, err1 := strconv.Atoi(lo)
	max, err2 := strconv.Atoi(hi)
	if !ok || !ok2 || err1 != nil || err2 != nil {
		return "", extapi.Range{}, fmt.Errorf("%q is not cap@min-max", s)
	}
	return name, extapi.Range{Min: min, Max: max}, nil
}

func errName(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.EACCES:
			return "EACCES"
		case syscall.EPERM:
			return "EPERM"
		case syscall.ENOENT:
			return "ENOENT"
		case syscall.ENXIO:
			return "ENXIO"
		}
		return errno.Error()
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

// probeFiles reports whether path can be read and whether the data
// directory (the working directory) can be written.
func probeFiles(log *slog.Logger, path string) {
	var readErr error
	if f, err := os.Open(path); err != nil {
		readErr = err
	} else {
		f.Close()
	}
	wd, _ := os.Getwd()
	writeErr := os.WriteFile(filepath.Join(wd, "probe.txt"), []byte("ok"), 0o600)
	log.Info("probe", "probe", "files", "path", path, "read", errName(readErr), "write", errName(writeErr))
	appendResult(map[string]any{"probe": "files", "path": path, "read": errName(readErr), "write": errName(writeErr)})
}

// probeSibling tries to reach another process: open its socket
// descriptors through /proc and signal it. It also lists the socket
// inodes this process holds.
func probeSibling(log *slog.Logger, pid int) {
	var open []string
	for _, fd := range []int{3, 4, 5, 6, 7, 8} {
		f, err := os.Open(fmt.Sprintf("/proc/%d/fd/%d", pid, fd))
		if err == nil {
			f.Close()
			open = append(open, strconv.Itoa(fd))
		}
	}
	var inodes []string
	for fd := 0; fd < 1024; fd++ {
		var st syscall.Stat_t
		if syscall.Fstat(fd, &st) == nil && st.Mode&syscall.S_IFMT == syscall.S_IFSOCK {
			inodes = append(inodes, strconv.FormatUint(st.Ino, 10))
		}
	}
	signalErr := syscall.Kill(pid, 0)
	log.Info("probe", "probe", "sibling", "pid", pid, "opened", strings.Join(open, ","),
		"signal", errName(signalErr), "sockets", strings.Join(inodes, ","))
}

// grow starts a child that holds mib MiB, then holds as much itself, and
// records the child's pid in results.jsonl.
func grow(log *slog.Logger, mib int) {
	exe, err := os.Executable()
	if err == nil {
		cmd := exec.Command(exe, "grow", strconv.Itoa(mib))
		// The sandbox denies opening /dev/null, so the child inherits the
		// descriptors the trampoline already pointed there.
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err = cmd.Start(); err == nil {
			appendResult(map[string]any{"probe": "grow", "child": cmd.Process.Pid})
			go func() { _ = cmd.Wait() }()
		}
	}
	if err != nil {
		log.Error("starting the growing child", "err", err)
	}
	held = touched(mib)
	log.Info("holding memory", "mib", len(held)>>20)
}

// growChild holds mib MiB until it is killed.
func growChild(mib string) {
	n, err := strconv.Atoi(mib)
	if err != nil {
		os.Exit(64)
	}
	held = touched(n)
	for {
		time.Sleep(time.Hour)
		runtime.KeepAlive(held)
	}
}

// touched allocates mib MiB and writes every page, so all of it is
// resident.
func touched(mib int) []byte {
	b := make([]byte, mib<<20)
	for i := 0; i < len(b); i += 4096 {
		b[i] = 1
	}
	return b
}
