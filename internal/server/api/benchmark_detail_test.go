package api

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
)

func TestBenchmarkDetailConcurrentAddsProduceValidJSONL(t *testing.T) {
	root := t.TempDir()
	detail, err := newBenchmarkDetail(root, 7, 1, true, 1)
	if err != nil {
		t.Fatal(err)
	}
	const records = 64
	var wg sync.WaitGroup
	for i := 0; i < records; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			detail.Add(loadgen.Result{Text: "ok"})
		}()
	}
	wg.Wait()
	path, err := detail.Close()
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(root, "run-7-point-1.jsonl.gz") {
		t.Fatalf("path=%q", path)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	scanner := bufio.NewScanner(gz)
	count := 0
	for scanner.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("line %d: %v", count, err)
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if count != records {
		t.Fatalf("records=%d want=%d", count, records)
	}
}

func TestBenchmarkDetailCapRetainsValidTruncatedGzip(t *testing.T) {
	dir := t.TempDir()
	d, err := newBenchmarkDetail(dir, 17, 1, true, 1)
	if err != nil {
		t.Fatal(err)
	}
	d.cw.max = 32
	d.Add(loadgen.Result{Text: strings.Repeat("x", 4096)})
	path, err := d.Close()
	if err != nil || path == "" {
		t.Fatalf("path=%q err=%v", path, err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	scanner := bufio.NewScanner(gz)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0] != `{"truncated":true}` {
		t.Fatalf("lines=%q", lines)
	}
}
