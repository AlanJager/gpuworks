//go:build linux && dqlite

package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
)

// A peer killed while streaming a material batch ends the length-framed body
// early. Field: a healthy member treated that as a definitive proof failure,
// closed its own admission and dropped the agent session. The truncated peer
// must only withhold its vote; wrong bytes still fail the proof.
func TestTruncatedMaterialBatchBodyOnlyWithholdsThePeerVote(t *testing.T) {
	payload := []byte("material bytes that the dying peer never finishes sending")
	sum := sha256.Sum256(payload)
	item := materialBatchItem{Digest: hex.EncodeToString(sum[:]), Bytes: int64(len(payload))}
	frame := func(body []byte) []byte {
		out := append([]byte(nil), materialBatchMagic...)
		out = binary.BigEndian.AppendUint16(out, 1)
		out = append(out, item.Digest...)
		out = binary.BigEndian.AppendUint64(out, uint64(item.Bytes))
		return append(out, body...)
	}
	full := frame(payload)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(full)))
		_, _ = w.Write(full[:len(full)-10])
		w.(http.Flusher).Flush()
		// Abort the connection mid-body, as a SIGKILLed member does.
		raw, _, _ := http.NewResponseController(w).Hijack()
		_ = raw.Close()
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	response, err := http.Get("http://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	err = verifyMaterialBatchBody(t.Context(), response.Body, []materialBatchItem{item})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated body error=%v, want unexpected EOF", err)
	}
	if voted, voteErr := materialPeerVote(err); voted || voteErr != nil {
		t.Fatalf("truncated peer body became a definitive failure: voted=%t err=%v (%v)", voted, voteErr, err)
	}
	// Complete but wrong bytes stay a definitive failure.
	wrong := append([]byte(nil), payload...)
	wrong[0] ^= 0xff
	err = verifyMaterialBatchBody(t.Context(), bytes.NewReader(frame(wrong)), []materialBatchItem{item})
	if voted, voteErr := materialPeerVote(err); voted || voteErr == nil {
		t.Fatalf("wrong bytes stopped failing the proof: voted=%t err=%v", voted, voteErr)
	}
}
