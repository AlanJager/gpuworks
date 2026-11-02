package tokenizer

import (
	"context"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAndSampleExactVocabulary(t *testing.T) {
	tok, err := Load(strings.NewReader(`{"model":{"vocab":{"hello":0," world":1,"GPU":2,"works":3}}}`))
	if err != nil {
		t.Fatal(err)
	}
	text := tok.Sample(rand.New(rand.NewSource(1)), 16)
	if strings.TrimSpace(text) == "" {
		t.Fatal("sample is empty")
	}
	if got := tok.Count("hello world"); got < 2 {
		t.Fatalf("count=%d", got)
	}
}

func TestSampleDecodesByteLevelVocabulary(t *testing.T) {
	tok, err := Load(strings.NewReader(`{"model":{"vocab":{"Ġhello":0}},"decoder":{"type":"ByteLevel"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := tok.Sample(rand.New(rand.NewSource(1)), 3); got != " hello hello hello" {
		t.Fatalf("decoded sample = %q", got)
	}
}

func TestSampleJoinsByteLevelMultibyteSequenceBeforeUTF8Decode(t *testing.T) {
	tok, err := Load(strings.NewReader(`{"model":{"vocab":{"Ã":0,"©":1}},"decoder":{"type":"ByteLevel"}}`))
	if err != nil {
		t.Fatal(err)
	}
	// The two ByteLevel code points represent the UTF-8 bytes C3 A9. Decode
	// after concatenating sampled vocabulary entries so they become one é.
	if got := decodeByteLevel("Ã©", tok.byteDecoder); got != "é" {
		t.Fatalf("decoded multibyte sample = %q", got)
	}
}

func TestLoadRefHTTPAndFallbackErrors(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tokenizer.json" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"model":{"vocab":{"hello":0,"world":1}}}`))
	}))
	defer srv.Close()
	tok, err := LoadRef(context.Background(), srv.URL+"/tokenizer.json")
	if err != nil || tok == nil || tok.Count("hello") != 1 {
		t.Fatalf("tok=%v err=%v", tok, err)
	}
	if _, err := LoadRef(context.Background(), srv.URL+"/missing"); err == nil {
		t.Fatal("expected HTTP error")
	}
}

func TestLoadRefLocalPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokenizer.json")
	if err := os.WriteFile(path, []byte(`{"model":{"vocab":{"x":0}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRef(context.Background(), path); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRejectsTokenizerWithoutVocab(t *testing.T) {
	if _, err := Load(strings.NewReader(`{"model":{}}`)); err == nil {
		t.Fatal("expected missing vocab error")
	}
}
