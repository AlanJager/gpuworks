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
//
// Reports are JSON lines on stderr with msg "probe".
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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

func main() {
	if len(os.Args) < 2 || os.Args[1] != "serve" {
		fmt.Fprintln(os.Stderr, "usage: fakeplugin serve [flags]")
		os.Exit(64)
	}
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	id := fs.String("id", "acme_fake", "package id")
	version := fs.String("version", "1.0.0", "package version")
	manifest := fs.String("manifest", "", "manifest digest")
	requires := fs.String("requires", "table.banners@1-1", "required capability cap@min-max")
	refuse := fs.String("refuse", "", "refuse initialize with CODE:reason")
	var push pushes
	fs.Var(&push, "push", "kind=<json> to push once initialized")
	crashAfter := fs.Duration("crash-after", -1, "exit 2 this long after initialize")
	exitAtStart := fs.Int("exit-at-start", -1, "exit with this code before serving")
	hang := fs.String("hang", "", "init or health")
	probeOpen := fs.String("probe-open", "", "path to try to read")
	probePID := fs.Int("probe-pid", 0, "another process to try to reach")
	_ = fs.Parse(os.Args[2:])

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
		name, rng, err := parseRange(*requires)
		if err != nil {
			log.Error("bad --requires", "err", err)
			os.Exit(64)
		}
		cfg.Requires = map[string]extapi.Range{name: rng}
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
		for _, kv := range push {
			kind, body, _ := strings.Cut(kv, "=")
			r, err := p.PushState(ctx, kind, json.RawMessage(body))
			if err != nil {
				log.Error("push refused", "kind", kind, "err", err.Error())
				continue
			}
			log.Info("pushed", "kind", kind, "generation", r.Generation, "skipped", r.Skipped)
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
