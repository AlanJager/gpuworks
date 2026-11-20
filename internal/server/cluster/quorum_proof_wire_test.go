package cluster

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"reflect"
	"testing"
)

// This packet is literal protocol bytes, independent of the production decoder:
// format version 1, term 7, index 21, config 20, leader 11, one voter a:9001.
var quorumFixtureSuccess = []byte{
	9, 0, 0, 0, 3, 2, 0, 0,
	1, 0, 0, 0, 0, 0, 0, 0,
	7, 0, 0, 0, 0, 0, 0, 0,
	21, 0, 0, 0, 0, 0, 0, 0,
	20, 0, 0, 0, 0, 0, 0, 0,
	11, 0, 0, 0, 0, 0, 0, 0,
	1, 0, 0, 0, 0, 0, 0, 0,
	11, 0, 0, 0, 0, 0, 0, 0,
	'a', ':', '9', '0', '0', '1', 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0,
}

func quorumFixtureDial(t *testing.T, packet []byte, fragment bool) (func(context.Context, string) (net.Conn, error), <-chan error) {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan error, 1)
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	go func() {
		defer server.Close()
		request := make([]byte, 24)
		if _, err := io.ReadFull(server, request); err != nil {
			done <- err
			return
		}
		want := []byte{1, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 16, 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0}
		if !bytes.Equal(request, want) {
			done <- errors.New("wire request differs from literal handshake/CLUSTER format 2")
			return
		}
		if fragment {
			for _, value := range packet {
				if _, err := server.Write([]byte{value}); err != nil {
					done <- err
					return
				}
			}
			done <- nil
			return
		}
		_, err := server.Write(packet)
		done <- err
	}()
	used := false
	return func(ctx context.Context, address string) (net.Conn, error) {
		if used || address != "node.example:9001" {
			t.Errorf("unexpected dial/fallback: used=%t address=%q", used, address)
		}
		used = true
		return client, nil
	}, done
}

func TestReadQuorumProofLiteralWireAndRoles(t *testing.T) {
	packet := append([]byte(nil), quorumFixtureSuccess...)
	packet[0] = 15
	packet[48] = 3
	packet = append(packet,
		12, 0, 0, 0, 0, 0, 0, 0, 'b', ':', '9', '0', '0', '2', 0, 0, 1, 0, 0, 0, 0, 0, 0, 0,
		13, 0, 0, 0, 0, 0, 0, 0, 'c', ':', '9', '0', '0', '3', 0, 0, 2, 0, 0, 0, 0, 0, 0, 0,
	)
	dial, done := quorumFixtureDial(t, packet, true)
	proof, err := ReadQuorumProof(t.Context(), "node.example:9001", dial)
	if err != nil {
		t.Fatal(err)
	}
	want := QuorumProof{Term: 7, Index: 21, ConfigurationIndex: 20, LeaderID: 11, Members: []Member{
		{ID: 11, Address: "a:9001", Role: "voter"},
		{ID: 12, Address: "b:9002", Role: "stand-by"},
		{ID: 13, Address: "c:9003", Role: "spare"},
	}}
	if !reflect.DeepEqual(proof, want) {
		t.Fatalf("literal quorum proof differs: %#v", proof)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReadQuorumProofRejectsUnqualifiedWire(t *testing.T) {
	change := func(offset int, word uint64) []byte {
		packet := append([]byte(nil), quorumFixtureSuccess...)
		binary.LittleEndian.PutUint64(packet[offset:offset+8], word)
		return packet
	}
	appendNode := func(id byte, address byte) []byte {
		packet := append([]byte(nil), quorumFixtureSuccess...)
		packet[0], packet[48] = 12, 2
		return append(packet, id, 0, 0, 0, 0, 0, 0, 0, address, ':', '9', '0', '0', '1', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	}
	header := func(words uint32, response, schema, extra byte) []byte {
		packet := []byte{0, 0, 0, 0, response, schema, extra, 0}
		binary.LittleEndian.PutUint32(packet, words)
		return packet
	}
	noncanonical := append([]byte(nil), quorumFixtureSuccess...)
	noncanonical[64] = 'A'
	padding := append([]byte(nil), quorumFixtureSuccess...)
	padding[71] = 1
	unterminated := append([]byte(nil), quorumFixtureSuccess...)
	for i := 64; i < len(unterminated); i++ {
		unterminated[i] = 'x'
	}
	trailing := append(append([]byte(nil), quorumFixtureSuccess...), make([]byte, 8)...)
	trailing[0] = 10
	oversizeAddress := append([]byte(nil), quorumFixtureSuccess[:64]...)
	oversizeAddress = append(oversizeAddress, bytes.Repeat([]byte{'x'}, 1025)...)
	oversizeAddress = append(oversizeAddress, make([]byte, 7+8)...)
	binary.LittleEndian.PutUint32(oversizeAddress, uint32((len(oversizeAddress)-8)/8))
	failurePadding := []byte{2, 0, 0, 0, 0, 0, 0, 0, 22, 0, 0, 0, 0, 0, 0, 0, 'x', 0, 1, 0, 0, 0, 0, 0}
	failureTrailing := append([]byte(nil), failurePadding...)
	failureTrailing[18] = 0
	failureTrailing = append(failureTrailing, make([]byte, 8)...)
	failureTrailing[0] = 3
	cases := map[string][]byte{
		"old nodes schema":        header(9, 3, 1, 0),
		"unknown type":            header(9, 4, 2, 0),
		"extra":                   header(9, 3, 2, 1),
		"empty body":              header(0, 3, 2, 0),
		"oversize body":           header(8193, 3, 2, 0),
		"truncated header":        {9, 0, 0, 0},
		"truncated body":          quorumFixtureSuccess[:len(quorumFixtureSuccess)-1],
		"format version":          change(8, 2),
		"zero term":               change(16, 0),
		"zero barrier":            change(24, 0),
		"zero configuration":      change(32, 0),
		"future configuration":    change(32, 22),
		"zero leader":             change(40, 0),
		"missing leader":          change(40, 99),
		"zero count":              change(48, 0),
		"too many nodes":          change(48, 513),
		"truncated members":       change(48, 512),
		"zero node":               change(56, 0),
		"leader standby":          change(72, 1),
		"leader spare":            change(72, 2),
		"unknown role":            change(72, 3),
		"duplicate id":            appendNode(11, 'b'),
		"duplicate address":       appendNode(12, 'a'),
		"noncanonical address":    noncanonical,
		"nonzero string padding":  padding,
		"unterminated address":    unterminated,
		"oversize address":        oversizeAddress,
		"trailing body":           trailing,
		"old server failure":      {3, 0, 0, 0, 0, 0, 0, 0, 22, 0, 0, 0, 0, 0, 0, 0, 'o', 'l', 'd', ' ', 'b', 'i', 'n', 'a', 'r', 'y', 0, 0, 0, 0, 0, 0},
		"failure wrong schema":    header(3, 0, 1, 0),
		"failure missing message": header(1, 0, 0, 0),
		"failure nonzero padding": failurePadding,
		"failure trailing data":   failureTrailing,
	}
	for name, packet := range cases {
		t.Run(name, func(t *testing.T) {
			if name == "failure missing message" {
				packet = append(packet, []byte{'x', 'x', 'x', 'x', 'x', 'x', 'x', 'x'}...)
			}
			dial, done := quorumFixtureDial(t, packet, false)
			proof, err := ReadQuorumProof(t.Context(), "node.example:9001", dial)
			if err == nil || !reflect.DeepEqual(proof, QuorumProof{}) {
				t.Fatalf("unqualified response returned proof=%#v err=%v", proof, err)
			}
			if name == "truncated body" && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("actual short body error lost: %v", err)
			}
			if name == "old server failure" && err.Error() != "quorum proof server failure 22: old binary" {
				t.Fatalf("old failure details lost: %v", err)
			}
			if fixtureErr := <-done; fixtureErr != nil && !errors.Is(fixtureErr, io.ErrClosedPipe) {
				t.Fatalf("fixture did not consume literal request: %v", fixtureErr)
			}
		})
	}
}
