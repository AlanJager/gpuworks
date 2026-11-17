package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/distribution"
)

// An edition build publishes archives under its own package prefix (the
// binary inside is still gpuworks) and an index naming its edition, which
// only a client of that edition accepts.
func TestAssembleEditionTree(t *testing.T) {
	in := realInput(t, distDir(t,
		"gpuworks-acme-v26.11.0-linux-amd64.tar.gz", "gpuworks-acme-v26.11.0-linux-arm64.tar.gz",
		// A community archive in the same directory is not part of the tree.
		"gpuworks-v26.11.0-linux-amd64.tar.gz"))
	in.Edition, in.ArchivePrefix = "acme", "gpuworks-acme"
	index, err := assemble(in)
	if err != nil {
		t.Fatal(err)
	}
	if index.Edition != "acme" || index.Server.ReleaseID != "v26.11.0" || len(index.Server.Files) != 2 {
		t.Fatalf("index = %+v", index)
	}
	asset, err := index.Server.Asset("linux", "amd64")
	if err != nil || asset.Path != "releases/v26.11.0/gpuworks-acme-v26.11.0-linux-amd64.tar.gz" {
		t.Fatalf("asset = %+v, %v", asset, err)
	}
	payload, _, err := distribution.Open(readTree(t, in.OutDir, "nightly/index.json"), distribution.Keys{"test": testPublic})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"edition": "acme"`) {
		t.Fatalf("signed payload has no edition: %s", payload)
	}
	if _, err := distribution.ParseIndex(payload, "nightly"); err == nil {
		t.Fatal("a community parse accepted the acme index")
	}

	keys := distribution.Keys{"test": testPublic}
	server := serveTree(t, in.OutDir)
	var out bytes.Buffer
	if err := verify(context.Background(), verifyInput{Channel: "nightly", Edition: "acme", Sources: []string{server.URL}, Keys: keys}, &out); err != nil {
		t.Fatalf("verify as acme: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := verify(context.Background(), verifyInput{Channel: "nightly", Sources: []string{server.URL}, Keys: keys}, &out); err == nil ||
		!strings.Contains(out.String(), "for the acme edition, not community") {
		t.Fatalf("verify as community = %v\n%s", err, out.String())
	}
}

// -edition community writes no edition, so the tree is what clients from
// before editions read.
func TestAssembleCommunityEditionNamesNone(t *testing.T) {
	in := realInput(t, distDir(t, "gpuworks-abc123-linux-amd64.tar.gz"))
	in.Edition = "community"
	index, err := assemble(in)
	if err != nil {
		t.Fatal(err)
	}
	if index.Edition != "" || bytes.Contains(readTree(t, in.OutDir, "nightly/index.json"), []byte("edition")) {
		t.Fatalf("community index names an edition: %+v", index)
	}
}

func TestAssembleRefusesBadEditionOrPrefix(t *testing.T) {
	cases := map[string]struct {
		mutate func(*assembleInput)
		want   string
	}{
		"bad edition":    {func(in *assembleInput) { in.Edition = "Acme" }, "not an edition name"},
		"bad prefix":     {func(in *assembleInput) { in.ArchivePrefix = "../x" }, "not an archive name prefix"},
		"prefix unfound": {func(in *assembleInput) { in.ArchivePrefix = "gpuworks-acme" }, "holds no gpuworks-acme-<id>"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := realInput(t, distDir(t, "gpuworks-a1-linux-amd64.tar.gz"))
			tc.mutate(&in)
			if _, err := assemble(in); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// An edition's channel carries over its own history, and a community
// publication never adopts another edition's index as its history.
func TestAssembleEditionKeepsItsOwnHistory(t *testing.T) {
	var prior string
	for _, id := range []string{"aaaa", "bbbb"} {
		in := realInput(t, distDir(t, "gpuworks-acme-"+id+"-linux-amd64.tar.gz"))
		in.Edition, in.ArchivePrefix, in.Keep, in.Previous = "acme", "gpuworks-acme", 5, prior
		index, err := assemble(in)
		if err != nil {
			t.Fatal(err)
		}
		if id == "bbbb" && (len(index.Servers()) != 2 || index.PreviousServers[0].ReleaseID != "aaaa" || index.Edition != "acme") {
			t.Fatalf("index = %+v", index)
		}
		prior = filepath.Join(in.OutDir, "nightly", "index.json")
	}
	in := realInput(t, distDir(t, "gpuworks-cccc-linux-amd64.tar.gz"))
	in.Previous = prior
	if _, err := assemble(in); err == nil || !strings.Contains(err.Error(), "acme edition") {
		t.Fatalf("community publication over the acme history: err = %v", err)
	}
}

// newer reads a channel as the edition's clients do: another edition's
// builds are not newer, so they never hold back this edition's publish.
func TestNewerReadsOnlyItsEdition(t *testing.T) {
	in := realInput(t, distDir(t, "gpuworks-abc123-linux-amd64.tar.gz"))
	in.Edition = "acme"
	if _, err := assemble(in); err != nil {
		t.Fatal(err)
	}
	served := serveTree(t, in.OutDir)
	keys := distribution.Keys{"test": testPublic}
	before := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	acme := &distribution.Client{Edition: "acme", Sources: []string{served.URL}, Keys: keys, IndexTimeout: time.Second}
	if got, err := publishedNewer(context.Background(), acme, "nightly", before); err != nil || !strings.Contains(got, "serves abc123") {
		t.Fatalf("acme newer = %q, %v", got, err)
	}
	community := &distribution.Client{Sources: []string{served.URL}, Keys: keys, IndexTimeout: time.Second}
	if got, _ := publishedNewer(context.Background(), community, "nightly", before); got != "" {
		t.Fatalf("a community publish saw the acme build as newer: %q", got)
	}
}
