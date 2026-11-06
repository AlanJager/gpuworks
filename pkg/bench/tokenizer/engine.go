package tokenizer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"unicode/utf8"
)

// EngineRef is the tokenizer reference that names the target's own
// tokenizer, reached through its /v1/tokenize and /v1/detokenize. What
// follows the prefix, when anything does, is the reference to load instead
// when the endpoint does not answer them.
const EngineRef = "engine:"

// IsEngineRef reports whether ref names the target's tokenizer, and returns
// the fallback reference after it ("" for none).
func IsEngineRef(ref string) (fallback string, ok bool) {
	if !strings.HasPrefix(ref, EngineRef) {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(ref, EngineRef)), true
}

// ErrEngineTokenize is an endpoint that does not serve tokenization: it is
// not vLLM or SGLang, or a platform deployment with tokenize turned off.
var ErrEngineTokenize = errors.New("the endpoint does not serve /v1/tokenize and /v1/detokenize")

// Engine is how the target's tokenizer is reached.
type Engine struct {
	HTTP            *http.Client
	BaseURL, APIKey string
	Model           string
}

const (
	// engineSampleTokens is how many vocabulary entries are drawn: enough
	// that a sampled prompt reads like the vocabulary, few enough for a
	// handful of requests.
	engineSampleTokens = 8192
	engineBatch        = 1024
	// engineMaxVocab bounds the vocabulary-size search; the largest
	// vocabularies in use are a few hundred thousand entries.
	engineMaxVocab = 1 << 22
	// engineMinTokens is the fewest whole-text entries a usable sample has.
	engineMinTokens = 256
)

// LoadEngine builds the sampling vocabulary from the target's own
// detokenizer, so prompts are made of the tokens the engine counts with,
// whatever format its tokenizer ships in (tokenizer.json, tiktoken,
// SentencePiece). It finds the vocabulary size, draws entries at random and
// detokenizes them in batches separated by the newline token: about thirty
// requests, all before any measured request.
func LoadEngine(ctx context.Context, e Engine, seed int64) (*Tokenizer, error) {
	if e.HTTP == nil {
		e.HTTP = http.DefaultClient
	}
	newline, err := e.tokenize(ctx, "\n")
	if err != nil {
		return nil, err
	}
	if len(newline) != 1 {
		return nil, fmt.Errorf("the endpoint tokenizes a newline as %d tokens; cannot separate sampled tokens", len(newline))
	}
	size, err := e.vocabularySize(ctx)
	if err != nil {
		return nil, err
	}
	r := rand.New(rand.NewSource(seed))
	vocab := map[string]int{}
	for drawn := 0; drawn < engineSampleTokens; drawn += engineBatch {
		ids := make([]int, 0, 2*engineBatch)
		for i := 0; i < engineBatch; i++ {
			ids = append(ids, r.Intn(size), newline[0])
		}
		text, err := e.detokenize(ctx, ids)
		if err != nil {
			return nil, err
		}
		pieces := strings.Split(text, "\n")
		// A drawn token that itself holds a newline shifts the pieces; the
		// pieces are still whole decoded text, which is all sampling needs.
		for i, piece := range pieces {
			if piece != "" && utf8.ValidString(piece) && !strings.ContainsRune(piece, utf8.RuneError) {
				vocab[piece] = drawn + i
			}
		}
	}
	tok, err := fromDecodedVocab(vocab)
	if err != nil || len(tok.tokens) < engineMinTokens {
		return nil, fmt.Errorf("the endpoint's detokenizer gave too few usable tokens to sample from")
	}
	return tok, nil
}

// vocabularySize finds the number of token ids the detokenizer accepts:
// the first id it refuses, found by doubling and then bisecting.
func (e Engine) vocabularySize(ctx context.Context) (int, error) {
	known := 0 // an id the detokenizer accepts
	if _, err := e.detokenize(ctx, []int{0}); err != nil {
		return 0, err
	}
	refused := 1024
	for {
		_, err := e.detokenize(ctx, []int{refused})
		if errors.Is(err, errTokenRefused) {
			break
		}
		if err != nil {
			return 0, err
		}
		known = refused
		if refused >= engineMaxVocab {
			return 0, fmt.Errorf("the endpoint accepts token ids beyond %d; cannot find its vocabulary size", engineMaxVocab)
		}
		refused *= 2
	}
	for refused-known > 1 {
		mid := known + (refused-known)/2
		_, err := e.detokenize(ctx, []int{mid})
		switch {
		case err == nil:
			known = mid
		case errors.Is(err, errTokenRefused):
			refused = mid
		default:
			return 0, err
		}
	}
	return refused, nil
}

// errTokenRefused is a detokenize request refused for its token ids: an id
// past the vocabulary.
var errTokenRefused = errors.New("token id refused")

func (e Engine) tokenize(ctx context.Context, prompt string) ([]int, error) {
	var out struct {
		Tokens []int `json:"tokens"`
	}
	status, err := e.post(ctx, "/v1/tokenize", map[string]any{"model": e.Model, "prompt": prompt, "add_special_tokens": false}, &out)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: /v1/tokenize answered HTTP %d", ErrEngineTokenize, status)
	}
	return out.Tokens, nil
}

// detokenize decodes ids. vLLM answers {"prompt": text}; SGLang {"text": text}.
func (e Engine) detokenize(ctx context.Context, ids []int) (string, error) {
	var out struct {
		Prompt *string `json:"prompt"`
		Text   *string `json:"text"`
	}
	status, err := e.post(ctx, "/v1/detokenize", map[string]any{"model": e.Model, "tokens": ids}, &out)
	if err != nil {
		return "", err
	}
	switch {
	case status == http.StatusNotFound || status == http.StatusMethodNotAllowed:
		return "", fmt.Errorf("%w: /v1/detokenize answered HTTP %d", ErrEngineTokenize, status)
	case status >= 400 && status < 500, status == http.StatusInternalServerError:
		// vLLM refuses an id past the vocabulary with 400; SGLang's decode
		// raises and answers 500. Id 0 must decode, so an engine that answers
		// 500 to everything fails the first probe instead of shrinking the
		// vocabulary.
		return "", errTokenRefused
	case status != http.StatusOK:
		return "", fmt.Errorf("/v1/detokenize answered HTTP %d", status)
	case out.Prompt != nil:
		return *out.Prompt, nil
	case out.Text != nil:
		return *out.Text, nil
	}
	return "", fmt.Errorf("/v1/detokenize answered without text")
}

// post sends body and decodes a 200 answer into out, returning the status.
func (e Engine) post(ctx context.Context, path string, body any, out any) (int, error) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(e.BaseURL, "/")+path, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.APIKey)
	}
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return 0, err
	}
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(data, out); err != nil {
			return 0, fmt.Errorf("%s answered unreadable JSON: %w", path, err)
		}
	}
	return resp.StatusCode, nil
}
