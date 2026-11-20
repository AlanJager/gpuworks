//go:build linux && dqlite

package app

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const interruptedMaterialBody = "material upload interrupted by a killed process"

func TestMaterialReceiverSIGKILLChild(t *testing.T) {
	root := os.Getenv("GPUWORKS_MATERIAL_KILL_ROOT")
	if root == "" {
		return
	}
	receiver, err := newMaterialReceiver(root, 7)
	if err != nil {
		t.Fatal(err)
	}
	marker := os.Getenv("GPUWORKS_MATERIAL_KILL_MARKER")
	source := io.MultiReader(strings.NewReader(interruptedMaterialBody[:12]), &blockedMaterialSource{marker: marker})
	_, err = receiver.receive(t.Context(), materialTestDigest([]byte(interruptedMaterialBody)), source)
	t.Fatalf("interrupted upload returned without a process kill: %v", err)
}

type blockedMaterialSource struct{ marker string }

func (source *blockedMaterialSource) Read([]byte) (int, error) {
	if err := os.WriteFile(source.marker, []byte("partial bytes written"), 0600); err != nil {
		return 0, err
	}
	time.Sleep(time.Hour)
	return 0, io.EOF
}

func TestMaterialReceiverRecoversAfterSIGKILL(t *testing.T) {
	root := t.TempDir()
	receiver, err := newMaterialReceiver(root, 7)
	if err != nil {
		t.Fatal(err)
	}
	stable := []byte("already published material remains available")
	stableDigest := materialTestDigest(stable)
	if _, err := receiver.receive(t.Context(), stableDigest, bytes.NewReader(stable)); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "upload-blocked")
	child := exec.Command(os.Args[0], "-test.run=^TestMaterialReceiverSIGKILLChild$")
	child.Env = append(os.Environ(), "GPUWORKS_MATERIAL_KILL_ROOT="+root, "GPUWORKS_MATERIAL_KILL_MARKER="+marker)
	var childOutput bytes.Buffer
	child.Stdout, child.Stderr = &childOutput, &childOutput
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if child.ProcessState == nil {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	}()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("child did not write partial upload before deadline: %s", childOutput.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatalf("kill blocked material upload: %v; child output: %s", err, childOutput.String())
	}
	if err := child.Wait(); err == nil {
		t.Fatal("blocked upload exited successfully before recovery")
	}
	digest := materialTestDigest([]byte(interruptedMaterialBody))
	prefix := filepath.Join(root, digest[:2])
	entries, err := os.ReadDir(prefix)
	if err != nil {
		t.Fatal(err)
	}
	var orphan string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".receiving-") {
			if orphan != "" {
				t.Fatal("killed upload left multiple temporary files")
			}
			orphan = filepath.Join(prefix, entry.Name())
		}
	}
	if orphan == "" {
		t.Fatal("killed upload did not leave its temporary file")
	}
	if partial, err := os.ReadFile(orphan); err != nil || string(partial) != interruptedMaterialBody[:12] {
		t.Fatalf("killed upload left unexpected partial bytes: %q %v", partial, err)
	}
	if _, err := os.Stat(filepath.Join(prefix, digest)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("killed upload published an incomplete file: %v", err)
	}
	if err := recoverOrphanedMaterialTransfers(t.Context(), receiver); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restart retained the killed upload: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(root, stableDigest[:2], stableDigest)); err != nil || !bytes.Equal(got, stable) {
		t.Fatalf("restart changed published material: %q %v", got, err)
	}
	if receipt, err := receiver.receive(t.Context(), digest, strings.NewReader(interruptedMaterialBody)); err != nil || receipt.Bytes != int64(len(interruptedMaterialBody)) {
		t.Fatalf("retry after process kill failed: %+v %v", receipt, err)
	}
}
