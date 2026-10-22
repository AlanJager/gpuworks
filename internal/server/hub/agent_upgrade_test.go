package hub

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type pushedUpgrade struct {
	hdr  proto.StreamHeader
	body []byte
}

// fakeUpgradeAgent plays the agent side of StreamAgentUpgrade the way
// internal/agent/conn does: read the header line, read exactly Size bytes,
// check the announced sha256, answer one AgentUpgradeStatus line, then wait
// for the server to close before "exec". answer overrides the verdict.
func fakeUpgradeAgent(t *testing.T, client *yamux.Session, answer func(pushedUpgrade) proto.AgentUpgradeStatus) <-chan pushedUpgrade {
	t.Helper()
	got := make(chan pushedUpgrade, 1)
	go func() {
		stream, err := client.AcceptStream()
		if err != nil {
			return
		}
		defer stream.Close()
		r := bufio.NewReader(stream)
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var hdr proto.StreamHeader
		if json.Unmarshal(line, &hdr) != nil {
			return
		}
		push := pushedUpgrade{hdr: hdr}
		status := proto.AgentUpgradeStatus{State: "verified"}
		if answer != nil {
			status = answer(push)
		}
		if status.State == "verified" {
			push.body = make([]byte, hdr.Size)
			if _, err := io.ReadFull(r, push.body); err != nil {
				return
			}
			sum := sha256.Sum256(push.body)
			if hex.EncodeToString(sum[:]) != hdr.Ref {
				status = proto.AgentUpgradeStatus{State: "failed", Error: "sha256 mismatch"}
			}
		}
		raw, _ := json.Marshal(status)
		_, _ = stream.Write(append(raw, '\n'))
		_, _ = io.Copy(io.Discard, r) // until the server closes
		got <- push
	}()
	return got
}

func newUpgradeHub(t *testing.T, caps []string) (*Hub, string, *yamux.Session) {
	t.Helper()
	h := newTestHub(t)
	node, err := h.store.RegisterNode("upgrade-node", store.HashToken("upgrade-token"), "upgrade-node")
	if err != nil {
		t.Fatal(err)
	}
	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() { _ = serverConn.Close(); _ = clientConn.Close() })
	serverYS, clientYS := yamuxPairData(t)
	session := newSession(h, serverYS, serverConn)
	session.NodeUUID = node.UUID
	session.caps = caps
	h.register(session)
	t.Cleanup(func() { h.deregister(session) })
	return h, node.UUID, clientYS
}

func waitUpgradeError(t *testing.T, h *Hub, uuid string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if msg := h.UpgradeErrors()[uuid]; msg != "" {
			return msg
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no upgrade error recorded")
	return ""
}

func TestUpgradeAgentPushesTheBinaryForTheNodeArch(t *testing.T) {
	h, uuid, client := newUpgradeHub(t, []string{"sysinfo", proto.CapUpgradePush})
	binary := []byte(strings.Repeat("agent-bytes ", 200_000)) // spans many yamux windows
	var asked string
	h.SetAgentBinaryReader(func(name string) ([]byte, error) { asked = name; return binary, nil })
	h.setUpgradeError(uuid, "stale failure from an earlier attempt")
	got := fakeUpgradeAgent(t, client, nil)

	pushed, err := h.UpgradeAgent(uuid, "darwin", "arm64")
	if err != nil || !pushed {
		t.Fatalf("pushed=%v err=%v", pushed, err)
	}
	if asked != "gpuworks-agent-darwin-arm64" {
		t.Fatalf("artifact = %q", asked)
	}
	select {
	case push := <-got:
		sum := sha256.Sum256(binary)
		if push.hdr.Kind != proto.StreamAgentUpgrade || push.hdr.Dst != asked || push.hdr.Size != int64(len(binary)) || push.hdr.Ref != hex.EncodeToString(sum[:]) {
			t.Fatalf("header = %+v", push.hdr)
		}
		if string(push.body) != string(binary) {
			t.Fatal("pushed bytes differ from the artifact")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agent never received the push")
	}
	if msg := h.UpgradeErrors()[uuid]; msg != "" {
		t.Fatalf("upgrade error after a verified push = %q", msg)
	}
}

func TestUpgradeAgentRecordsTheAgentsRefusal(t *testing.T) {
	h, uuid, client := newUpgradeHub(t, []string{proto.CapUpgradePush})
	h.SetAgentBinaryReader(func(string) ([]byte, error) { return []byte(strings.Repeat("x", 4<<20)), nil })
	// The agent refuses without reading the body, as it does when busy.
	fakeUpgradeAgent(t, client, func(pushedUpgrade) proto.AgentUpgradeStatus {
		return proto.AgentUpgradeStatus{State: "failed", Error: "agent upgrade already in progress"}
	})
	if pushed, err := h.UpgradeAgent(uuid, "", "amd64"); err != nil || !pushed {
		t.Fatalf("pushed=%v err=%v", pushed, err)
	}
	if msg := waitUpgradeError(t, h, uuid); !strings.Contains(msg, "already in progress") {
		t.Fatalf("upgrade error = %q", msg)
	}
}

func TestUpgradeAgentRecordsAnAgentThatHangsUp(t *testing.T) {
	h, uuid, client := newUpgradeHub(t, []string{proto.CapUpgradePush})
	h.SetAgentBinaryReader(func(string) ([]byte, error) { return []byte("small"), nil })
	go func() {
		stream, err := client.AcceptStream()
		if err == nil {
			_ = stream.Reset()
		}
	}()
	if pushed, err := h.UpgradeAgent(uuid, "", "amd64"); err != nil || !pushed {
		t.Fatalf("pushed=%v err=%v", pushed, err)
	}
	// The reset can land before the stream is open, while the binary is sent
	// or while the answer is awaited; each is the agent hanging up.
	if msg := waitUpgradeError(t, h, uuid); !strings.Contains(msg, "upgrade answer") && !strings.Contains(msg, "send agent binary") && !strings.Contains(msg, "open upgrade stream") {
		t.Fatalf("upgrade error = %q", msg)
	}
}

func TestUpgradeAgentFallsBackForAgentsThatCannotTakeAPush(t *testing.T) {
	h, uuid, _ := newUpgradeHub(t, []string{"sysinfo", "webterm"})
	read := 0
	h.SetAgentBinaryReader(func(string) ([]byte, error) { read++; return []byte("bin"), nil })
	if pushed, err := h.UpgradeAgent(uuid, "", "amd64"); err != nil || pushed {
		t.Fatalf("old agent: pushed=%v err=%v", pushed, err)
	}

	h2, uuid2, _ := newUpgradeHub(t, []string{proto.CapUpgradePush})
	h2.SetAgentBinaryReader(func(string) ([]byte, error) { read++; return []byte("bin"), nil })
	if pushed, err := h2.UpgradeAgent(uuid2, "", ""); err != nil || pushed {
		t.Fatalf("unknown arch: pushed=%v err=%v", pushed, err)
	}
	if read != 0 {
		t.Fatalf("artifact read %d times for a fallback", read)
	}

	h3, uuid3, _ := newUpgradeHub(t, []string{proto.CapUpgradePush})
	if pushed, err := h3.UpgradeAgent(uuid3, "", "amd64"); err != nil || pushed {
		t.Fatalf("no artifact reader: pushed=%v err=%v", pushed, err)
	}
}

func TestUpgradeAgentErrors(t *testing.T) {
	h, uuid, _ := newUpgradeHub(t, []string{proto.CapUpgradePush})
	h.SetAgentBinaryReader(func(name string) ([]byte, error) { return nil, errors.New("open " + name + ": no such file") })
	if _, err := h.UpgradeAgent(uuid, "", "riscv64"); err == nil || !strings.Contains(err.Error(), "gpuworks-agent-linux-riscv64") {
		t.Fatalf("missing artifact err = %v", err)
	}
	if _, err := h.UpgradeAgent("offline-node", "", "amd64"); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("offline err = %v", err)
	}
}
