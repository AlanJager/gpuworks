package extapi

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// goldenRoot holds the frozen wire corpus: <kind>/v1/valid/*.json decode,
// validate and re-encode byte for byte; <kind>/v1/invalid/<CODE>/*.json
// fail with exactly CODE. An invalid file named "<field>--<rule>[--note]"
// must also fail at that field (and rule, where the code has one); "_root"
// is the empty field.
const goldenRoot = "testdata/golden"

// goldenSums lists the SHA-256 of every golden file. It may only grow: a
// released golden is never edited or removed (scripts/check_architecture.sh
// also compares it with the merge base). A changed wire format is a new
// version with its own directory, never an edit.
const goldenSums = goldenRoot + "/SHA256SUMS"

var goldenDecoders = map[string]func([]byte) (any, error){
	"manifest":           func(b []byte) (any, error) { return DecodeManifest(b) },
	"publisher_cert":     func(b []byte) (any, error) { return DecodePublisherCert(b) },
	"trust_update":       func(b []byte) (any, error) { return DecodeTrustUpdate(b) },
	"crl":                func(b []byte) (any, error) { return DecodeCRL(b) },
	"envelope":           func(b []byte) (any, error) { return DecodeEnvelope(b) },
	"handshake_request":  func(b []byte) (any, error) { return DecodeHandshakeRequest(b) },
	"handshake_response": func(b []byte) (any, error) { return DecodeHandshakeResponse(b) },
	"health":             func(b []byte) (any, error) { return DecodeHealth(b) },
	"error":              func(b []byte) (any, error) { return DecodeWireError(b) },
	"state_push":         func(b []byte) (any, error) { return DecodeStatePush(b) },
	"state_ack":          func(b []byte) (any, error) { return DecodeStateAck(b) },
	KindAvailability:     tableDecoder(KindAvailability),
	KindFleetCapacity:    tableDecoder(KindFleetCapacity),
	KindNodeHolds:        tableDecoder(KindNodeHolds),
	KindBanners:          tableDecoder(KindBanners),
	KindAttestations:     tableDecoder(KindAttestations),
}

func tableDecoder(kind string) func([]byte) (any, error) {
	return func(b []byte) (any, error) { return DecodeTable(kind, 1, b) }
}

func TestGoldenCorpusCoversEveryWireObject(t *testing.T) {
	for kind := range goldenDecoders {
		for _, sub := range []string{"valid", "invalid"} {
			dir := filepath.Join(goldenRoot, kind, "v1", sub)
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) == 0 {
				t.Errorf("%s: want golden files (%v)", dir, err)
			}
		}
	}
	dirs, err := os.ReadDir(goldenRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if _, ok := goldenDecoders[d.Name()]; d.IsDir() && !ok {
			t.Errorf("%s: golden directory without a decoder", d.Name())
		}
	}
}

func TestGoldenValidFilesRoundTrip(t *testing.T) {
	for kind, decode := range goldenDecoders {
		files, _ := filepath.Glob(filepath.Join(goldenRoot, kind, "v1", "valid", "*.json"))
		for _, file := range files {
			t.Run(file, func(t *testing.T) {
				raw, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				if !IsCanonical(raw) {
					t.Fatalf("golden file is not canonical JSON")
				}
				v, err := decode(raw)
				if err != nil {
					t.Fatalf("decode: %v (%s)", err, CodeOf(err))
				}
				again, err := Marshal(v)
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				if !bytes.Equal(again, raw) {
					t.Fatalf("re-encoding differs:\n got %s\nwant %s", again, raw)
				}
			})
		}
	}
}

func TestGoldenInvalidFilesFailWithTheirCode(t *testing.T) {
	for kind, decode := range goldenDecoders {
		codes, _ := os.ReadDir(filepath.Join(goldenRoot, kind, "v1", "invalid"))
		for _, codeDir := range codes {
			code := codeDir.Name()
			files, _ := filepath.Glob(filepath.Join(goldenRoot, kind, "v1", "invalid", code, "*.json"))
			for _, file := range files {
				t.Run(file, func(t *testing.T) {
					raw, err := os.ReadFile(file)
					if err != nil {
						t.Fatal(err)
					}
					_, err = decode(raw)
					if err == nil {
						t.Fatalf("decoded; want %s", code)
					}
					e, ok := err.(*Error)
					if !ok {
						t.Fatalf("error %T %v is not *Error", err, err)
					}
					if e.Code != code {
						t.Fatalf("code %s (%v), want %s", e.Code, e, code)
					}
					checkGoldenParams(t, file, e)
				})
			}
		}
	}
}

func checkGoldenParams(t *testing.T, file string, e *Error) {
	t.Helper()
	name := strings.TrimSuffix(filepath.Base(file), ".json")
	parts := strings.Split(name, "--")
	if len(parts) < 2 {
		return
	}
	field := parts[0]
	if field == "_root" {
		field = ""
	}
	if got := e.Params["field"]; got != field {
		t.Fatalf("field %v, want %q (%v)", got, field, e)
	}
	if rule, ok := e.Params["rule"]; ok && rule != parts[1] {
		t.Fatalf("rule %v, want %q (%v)", rule, parts[1], e)
	}
}

func TestGoldenSumsOnlyGrow(t *testing.T) {
	listed := readGoldenSums(t)
	for path, sum := range listed {
		raw, err := os.ReadFile(filepath.Join(goldenRoot, path))
		if err != nil {
			t.Errorf("%s: released golden removed (%v); a wire change is a new version, never an edit", path, err)
			continue
		}
		if got := sha256Hex(raw); got != sum {
			t.Errorf("%s: released golden edited (sha256 %s, released %s); add a new version instead", path, got, sum)
		}
	}
	var missing []string
	_ = filepath.WalkDir(goldenRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".json") {
			return err
		}
		rel, _ := filepath.Rel(goldenRoot, p)
		rel = filepath.ToSlash(rel)
		if _, ok := listed[rel]; !ok {
			raw, _ := os.ReadFile(p)
			missing = append(missing, fmt.Sprintf("%s  %s", sha256Hex(raw), rel))
		}
		return nil
	})
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("golden files not in %s; append these lines:\n%s", goldenSums, strings.Join(missing, "\n"))
	}
}

func readGoldenSums(t *testing.T) map[string]string {
	t.Helper()
	f, err := os.Open(goldenSums)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		sum, path, ok := strings.Cut(sc.Text(), "  ")
		if !ok || len(sum) != 64 {
			t.Fatalf("%s: malformed line %q", goldenSums, sc.Text())
		}
		if _, dup := out[path]; dup {
			t.Fatalf("%s: %s listed twice", goldenSums, path)
		}
		out[path] = sum
	}
	return out
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
