// Command hostenvimage writes a single-layer image archive in docker-save
// format from a layer tarball, with the image config a host environment
// package needs: its platform, environment and labels. build/host-envs uses it
// so a package is built without a container daemon and with a digest that
// depends only on its inputs.
//
//	go run ./scripts/hostenvimage -layer layer.tar.gz -tag gpuworks/gpuworks-runner:metal15.0-vllm0.31.0-r2 \
//	    -os darwin -arch arm64 -env PATH=/bin:/python/bin -label k=v -o package.tar
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

func main() {
	layerPath := flag.String("layer", "", "gzip-compressed layer tarball")
	tag := flag.String("tag", "", "repository:tag recorded in the archive")
	goos := flag.String("os", "darwin", "image OS")
	arch := flag.String("arch", "arm64", "image architecture")
	output := flag.String("o", "", "output archive")
	workdir := flag.String("workdir", "", "image WorkingDir (optional)")
	var env, labels listFlag
	flag.Var(&env, "env", "KEY=VALUE for the image config (repeatable)")
	flag.Var(&labels, "label", "KEY=VALUE label (repeatable)")
	flag.Parse()
	if *layerPath == "" || *tag == "" || *output == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*layerPath, *tag, *goos, *arch, *workdir, env, labels, *output); err != nil {
		fmt.Fprintln(os.Stderr, "hostenvimage:", err)
		os.Exit(1)
	}
}

func run(layerPath, tag, goos, arch, workdir string, env, labels []string, output string) error {
	layer, err := os.ReadFile(layerPath)
	if err != nil {
		return err
	}
	diffID, err := uncompressedDigest(layer)
	if err != nil {
		return fmt.Errorf("read layer: %w", err)
	}
	labelMap := map[string]string{}
	for _, l := range labels {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			return fmt.Errorf("label %q is not KEY=VALUE", l)
		}
		labelMap[k] = v
	}
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	config, err := json.Marshal(map[string]any{
		"architecture": arch,
		"os":           goos,
		"created":      epoch,
		"config":       map[string]any{"Env": []string(env), "Labels": labelMap, "WorkingDir": workdir},
		"rootfs":       map[string]any{"type": "layers", "diff_ids": []string{"sha256:" + diffID}},
		"history":      []map[string]any{{"created": epoch, "created_by": "gpuworks build/host-envs"}},
	})
	if err != nil {
		return err
	}
	configName := digest(config) + ".json"
	layerName := digest(layer) + "/layer.tar"
	manifest, err := json.Marshal([]map[string]any{{"Config": configName, "RepoTags": []string{tag}, "Layers": []string{layerName}}})
	if err != nil {
		return err
	}

	f, err := os.Create(output)
	if err != nil {
		return err
	}
	w := tar.NewWriter(f)
	for _, member := range []struct {
		name string
		data []byte
	}{{configName, config}, {layerName, layer}, {"manifest.json", manifest}} {
		header := &tar.Header{Name: member.name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(member.data)), ModTime: epoch}
		if err := w.WriteHeader(header); err != nil {
			f.Close()
			return err
		}
		if _, err := w.Write(member.data); err != nil {
			f.Close()
			return err
		}
	}
	if err := w.Close(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func uncompressedDigest(compressed []byte) (string, error) {
	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, zr); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
