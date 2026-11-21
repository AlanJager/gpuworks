package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

func git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

func resolve(ref string) (string, error) {
	out, err := git("rev-parse", "--verify", ref+"^{commit}")
	return strings.TrimSpace(out), err
}

// productionGo says whether path is Go the product ships: not a test, not
// generated, not a fixture.
func productionGo(path string) bool {
	return strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") &&
		!strings.HasSuffix(path, "_gen.go") && !strings.Contains(path, "/testdata/")
}

// goRoots are the directories whose Go code is measured.
var goRoots = []string{"cmd", "internal", "pkg"}

// eachGoFile calls fn with every production Go file of commit, read from
// git's archive of it.
func eachGoFile(commit string, fn func(path string, src []byte)) error {
	cmd := exec.Command("git", append([]string{"archive", "--format=tar", commit, "--"}, goRoots...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	tr := tar.NewReader(out)
	var readErr error
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			readErr = err
			break
		}
		if h.Typeflag != tar.TypeReg || !productionGo(h.Name) {
			continue
		}
		src, err := io.ReadAll(tr)
		if err != nil {
			readErr = err
			break
		}
		fn(h.Name, src)
	}
	_, _ = io.Copy(io.Discard, out)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("git archive %s: %w: %s", commit, err, strings.TrimSpace(stderr.String()))
	}
	return readErr
}
