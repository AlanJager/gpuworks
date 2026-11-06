package tokenizer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// A tiktoken vocabulary as Kimi ships it: "<base64 bytes> <rank>" per line,
// including byte-level entries that are not whole UTF-8 text on their own.
func tiktokenFixture() string {
	var b strings.Builder
	entries := []string{"Hello", " world", "\n", "的", " token", "\xe4\xbd"} // the last ends inside a character
	for i, e := range entries {
		fmt.Fprintf(&b, "%s %d\n", base64.StdEncoding.EncodeToString([]byte(e)), i)
	}
	return b.String()
}

func TestTiktokenVocabularySamplesWholeTextOnly(t *testing.T) {
	tok, err := LoadTiktoken(strings.NewReader(tiktokenFixture()))
	if err != nil {
		t.Fatal(err)
	}
	if len(tok.tokens) != 5 {
		t.Fatalf("tokens = %q, want the five whole-text entries", tok.tokens)
	}
	text := tok.Sample(rand.New(rand.NewSource(1)), 200)
	if strings.ContainsRune(text, '�') || !strings.Contains(text, "Hello") {
		t.Fatalf("sample = %q", text)
	}
	for _, bad := range []string{"", "not-base64! 1\n", "SGVsbG8= x\n", "SGVsbG8=\n"} {
		if _, err := LoadTiktoken(strings.NewReader(bad)); err == nil {
			t.Errorf("LoadTiktoken(%q) accepted", bad)
		}
	}
}

func TestLoadFileTellsTokenizerJSONFromTiktoken(t *testing.T) {
	if tok, err := LoadFile(strings.NewReader("\n  " + validTokenizer)); err != nil || tok.decoded {
		t.Fatalf("tokenizer.json: %v decoded=%v", err, tok != nil && tok.decoded)
	}
	if tok, err := LoadFile(strings.NewReader(tiktokenFixture())); err != nil || !tok.decoded {
		t.Fatalf("tiktoken: %v", err)
	}
	if _, err := LoadFile(strings.NewReader("   ")); err == nil {
		t.Fatal("an empty file loaded")
	}
}

// A repository without tokenizer.json (404) is asked for its tiktoken
// vocabulary; any other failure of tokenizer.json is not covered up.
func TestRepositoryRefFallsBackToTiktokenOnlyWhenTokenizerJSONIsMissing(t *testing.T) {
	var asked []string
	tokenizerClient(t, func(r *http.Request) (*http.Response, error) {
		asked = append(asked, r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/tokenizer.json") && strings.Contains(r.URL.Path, "/kimi/"):
			return &http.Response{StatusCode: 404, Status: "404 Not Found", Body: io.NopCloser(strings.NewReader("Entry not found")), Header: make(http.Header)}, nil
		case strings.HasSuffix(r.URL.Path, "/tiktoken.model"):
			return tokenizerResponse(tiktokenFixture()), nil
		default:
			return &http.Response{StatusCode: 403, Status: "403 Forbidden", Body: io.NopCloser(strings.NewReader("gated")), Header: make(http.Header)}, nil
		}
	})
	tok, err := LoadRef(context.Background(), "hf://moonshotai/kimi/@abc")
	if err != nil || !tok.decoded {
		t.Fatalf("tiktoken repository: tok=%v err=%v asked=%v", tok, err, asked)
	}
	asked = nil
	if _, err := LoadRef(context.Background(), "hf://org/gated"); err == nil || !errors.Is(err, ErrDownload) {
		t.Fatalf("gated repository err = %v", err)
	}
	for _, path := range asked {
		if strings.HasSuffix(path, "/tiktoken.model") {
			t.Fatalf("a 403 on tokenizer.json fell back to %s", path)
		}
	}
}

// fakeEngine serves /v1/tokenize and /v1/detokenize as vLLM or SGLang do
// over a vocabulary of size entries: id 198 is the newline, an id past the
// vocabulary is refused with the engine's own status, and detokenize answers
// in the engine's own field.
type fakeEngine struct {
	sglang   bool
	size     int
	disabled bool // the gateway's answer for a deployment with tokenize off
	requests atomic.Int32
	auth     atomic.Value
}

func (f *fakeEngine) token(id int) string {
	if id == 198 {
		return "\n"
	}
	return fmt.Sprintf(" w%d", id)
}

func (f *fakeEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	f.auth.Store(r.Header.Get("Authorization"))
	if f.disabled {
		http.Error(w, `{"error":{"message":"deployment m does not expose POST /v1/tokenize: tokenize is turned off for it"}}`, http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
		Tokens []int  `json:"tokens"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "m" {
		http.Error(w, `{"object":"error","message":"bad request"}`, http.StatusBadRequest)
		return
	}
	switch r.URL.Path {
	case "/v1/tokenize":
		tokens := []int{}
		if body.Prompt == "\n" {
			tokens = []int{198}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"count": len(tokens), "max_model_len": 262144, "tokens": tokens, "token_strs": nil})
	case "/v1/detokenize":
		var b strings.Builder
		for _, id := range body.Tokens {
			if id < 0 || id >= f.size {
				if f.sglang {
					http.Error(w, `{"error":{"message":"piece id is out of range."}}`, http.StatusInternalServerError)
				} else {
					http.Error(w, `{"object":"error","message":"Token id `+fmt.Sprint(id)+` is out of vocabulary","type":"BadRequestError","code":400}`, http.StatusBadRequest)
				}
				return
			}
			b.WriteString(f.token(id))
		}
		if f.sglang {
			_ = json.NewEncoder(w).Encode(map[string]any{"text": b.String()})
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{"prompt": b.String()})
		}
	default:
		http.NotFound(w, r)
	}
}

func TestEngineVocabularyComesFromTheTargetsOwnDetokenizer(t *testing.T) {
	for _, sglang := range []bool{false, true} {
		t.Run(map[bool]string{false: "vllm", true: "sglang"}[sglang], func(t *testing.T) {
			engine := &fakeEngine{sglang: sglang, size: 151_669}
			srv := httptest.NewServer(engine)
			defer srv.Close()
			tok, err := LoadEngine(context.Background(), Engine{BaseURL: srv.URL, APIKey: "sk-test", Model: "m"}, 7)
			if err != nil {
				t.Fatal(err)
			}
			if n := engine.requests.Load(); n > 48 {
				t.Fatalf("%d requests to build the vocabulary; it should take a few dozen", n)
			}
			if got := engine.auth.Load(); got != "Bearer sk-test" {
				t.Fatalf("authorization = %v", got)
			}
			if len(tok.tokens) < engineMinTokens {
				t.Fatalf("only %d tokens sampled", len(tok.tokens))
			}
			for _, entry := range tok.tokens {
				var id int
				if _, err := fmt.Sscanf(entry, " w%d", &id); err != nil || id >= engine.size {
					t.Fatalf("entry %q is not a token of the vocabulary", entry)
				}
			}
			if text := tok.Sample(rand.New(rand.NewSource(1)), 50); !strings.HasPrefix(text, " w") || strings.Count(text, " w") != 50 {
				t.Fatalf("sample = %q, want 50 vocabulary tokens joined as they are", text)
			}
		})
	}
}

func TestEngineThatDoesNotServeTokenizeSaysSo(t *testing.T) {
	srv := httptest.NewServer(&fakeEngine{size: 1000, disabled: true})
	defer srv.Close()
	if _, err := LoadEngine(context.Background(), Engine{BaseURL: srv.URL, Model: "m"}, 1); !errors.Is(err, ErrEngineTokenize) {
		t.Fatalf("err = %v, want ErrEngineTokenize", err)
	}
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/tokenize" {
			_, _ = io.WriteString(w, `{"count":1,"max_model_len":4096,"tokens":[198],"token_strs":null}`)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer broken.Close()
	if _, err := LoadEngine(context.Background(), Engine{BaseURL: broken.URL, Model: "m"}, 1); err == nil {
		t.Fatal("an engine that cannot decode id 0 gave a vocabulary")
	}
}
