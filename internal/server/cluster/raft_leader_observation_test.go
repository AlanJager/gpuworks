package cluster

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestObserveRaftLeaderUsesIndependentCandidateKeyAndLiteralFrames(t *testing.T) {
	f := newObservedLeaderFixture(t, observedLeaderHint)
	forCall := 0
	member, key, err := ObserveRaftLeader(t.Context(), observedSeedAddress, func(ctx context.Context, address string) (net.Conn, string, error) {
		forCall++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			return nil, "", errors.New("missing shared five-second attempt")
		}
		return f.dial(ctx, address)
	})
	if err != nil || member != (Member{ID: 11, Address: "node.example:9001", Role: "voter"}) || key != f.keys[1] || key == f.keys[0] || forCall != 2 {
		t.Fatalf("actual candidate observation failed: %+v key=%q err=%v", member, key, err)
	}
	f.check(t, 2, true)
}

func TestObserveRaftLeaderRefusesInvalidSetupWithoutDial(t *testing.T) {
	for _, name := range []string{"noncanonical", "missing port", "nil dial", "canceled"} {
		t.Run(name, func(t *testing.T) {
			address := observedSeedAddress
			calls := 0
			dial := func(context.Context, string) (net.Conn, string, error) {
				calls++
				return nil, "", errors.New("unexpected dial")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch name {
			case "noncanonical":
				address = "SEED.example:9002"
			case "missing port":
				address = "seed.example"
			case "nil dial":
				dial = nil
			case "canceled":
				cancel()
			}
			member, key, err := ObserveRaftLeader(ctx, address, dial)
			assertEmptyLeaderObservation(t, observedLeaderResult{member, key, err}, nil)
			if calls != 0 {
				t.Fatal("invalid setup performed a physical dial")
			}
			if name == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatal("parent cancellation cause lost", err)
			}
		})
	}
}

func TestObserveRaftLeaderRejectsInvalidHintBeforeCandidate(t *testing.T) {
	for _, name := range []string{"no leader", "zero ID", "noncanonical address", "type", "schema", "extra", "zero words", "oversize", "legacy body", "unterminated", "padding", "trailing", "failure"} {
		t.Run(name, func(t *testing.T) {
			packet := append([]byte(nil), observedLeaderHint...)
			switch name {
			case "no leader":
				packet = []byte{2, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
			case "zero ID":
				binary.LittleEndian.PutUint64(packet[8:16], 0)
			case "noncanonical address":
				packet[16] = 'N'
			case "type":
				packet[4] = 4
			case "schema":
				packet[5] = 1
			case "extra":
				packet[6] = 1
			case "zero words":
				binary.LittleEndian.PutUint32(packet[:4], 0)
			case "oversize":
				binary.LittleEndian.PutUint32(packet[:4], raftLeaderMaxBody/8+1)
			case "legacy body":
				packet = []byte{1, 0, 0, 0, 1, 0, 0, 0, 'a', ':', '9', '0', '0', '1', 0, 0}
			case "unterminated":
				for i := 16; i < len(packet); i++ {
					packet[i] = 'a'
				}
			case "padding":
				packet[len(packet)-1] = 1
			case "trailing":
				packet = append(packet, make([]byte, 8)...)
				binary.LittleEndian.PutUint32(packet[:4], 5)
			case "failure":
				packet = []byte{2, 0, 0, 0, 0, 0, 0, 0, 5, 0, 0, 0, 0, 0, 0, 0, 'n', 'o', 0, 0, 0, 0, 0, 0}
			}
			f := newObservedLeaderFixture(t, packet)
			member, key, err := ObserveRaftLeader(t.Context(), observedSeedAddress, f.dial)
			assertEmptyLeaderObservation(t, observedLeaderResult{member, key, err}, nil)
			f.check(t, 1, true)
		})
	}
}

func TestObserveRaftLeaderAmbiguousLegacyHintRequiresMatchingFreshProof(t *testing.T) {
	// NODE has the same type/schema in both formats. A long legacy address
	// can look like an integer ID followed by a valid address suffix. It still
	// cannot authorize a tuple without a matching independent fresh proof.
	packet := append([]byte(nil), observedLeaderHint[:8]...)
	packet = append(packet, observedLeaderHint[16:]...)
	binary.LittleEndian.PutUint32(packet[:4], 3)
	f := newObservedLeaderFixture(t, packet)
	f.candidateAddress = "mple:9001"
	member, key, err := ObserveRaftLeader(t.Context(), observedSeedAddress, f.dial)
	assertEmptyLeaderObservation(t, observedLeaderResult{member, key, err}, nil)
	f.check(t, 2, true)
}

func TestObserveRaftLeaderRejectsChangedOrInvalidCandidateProof(t *testing.T) {
	for _, name := range []string{"leader ID", "leader address", "nonvoter", "invalid barrier", "old format"} {
		t.Run(name, func(t *testing.T) {
			f := newObservedLeaderFixture(t, observedLeaderHint)
			packet := append([]byte(nil), quorumOwnerSuccess...)
			switch name {
			case "leader ID":
				binary.LittleEndian.PutUint64(packet[40:48], 12)
				binary.LittleEndian.PutUint64(packet[56:64], 12)
			case "leader address":
				packet = bytes.Replace(packet, []byte("node.example"), []byte("evil.example"), 1)
			case "nonvoter":
				binary.LittleEndian.PutUint64(packet[len(packet)-8:], 1)
			case "invalid barrier":
				binary.LittleEndian.PutUint64(packet[16:24], 0)
			case "old format":
				packet[5] = 0
			}
			f.packets[1] = packet
			member, key, err := ObserveRaftLeader(t.Context(), observedSeedAddress, f.dial)
			assertEmptyLeaderObservation(t, observedLeaderResult{member, key, err}, nil)
			f.check(t, 2, true)
		})
	}
}

func TestObserveRaftLeaderRejectsInvalidVerifiedPeerKeys(t *testing.T) {
	for _, stage := range []string{"seed", "candidate"} {
		for _, name := range []string{"empty", "short", "uppercase", "nonhex"} {
			t.Run(stage+"/"+name, func(t *testing.T) {
				f := newObservedLeaderFixture(t, observedLeaderHint)
				index := 0
				if stage == "candidate" {
					index = 1
				}
				switch name {
				case "empty":
					f.keys[index] = ""
				case "short":
					f.keys[index] = strings.Repeat("a", 63)
				case "uppercase":
					f.keys[index] = strings.Repeat("A", 64)
				case "nonhex":
					f.keys[index] = strings.Repeat("g", 64)
				}
				member, key, err := ObserveRaftLeader(t.Context(), observedSeedAddress, f.dial)
				assertEmptyLeaderObservation(t, observedLeaderResult{member, key, err}, nil)
				f.check(t, index+1, false)
			})
		}
	}
}

func TestObserveRaftLeaderDialFailuresJoinReturnedOriginals(t *testing.T) {
	for _, stage := range []string{"seed", "candidate"} {
		for _, shape := range []string{"nil connection", "connection with error"} {
			t.Run(stage+"/"+shape, func(t *testing.T) {
				f := newObservedLeaderFixture(t, observedLeaderHint)
				index := 1
				if stage == "candidate" {
					index = 2
				}
				failure := errors.New("authenticated physical dial failed")
				f.dialErr = func(call int) error {
					if call == index {
						return failure
					}
					return nil
				}
				dial := f.dial
				if shape == "nil connection" {
					dial = func(ctx context.Context, address string) (net.Conn, string, error) {
						if int(f.calls.Load())+1 == index {
							f.calls.Add(1)
							return nil, strings.Repeat("a", 64), nil
						}
						return f.dial(ctx, address)
					}
				}
				member, key, err := ObserveRaftLeader(t.Context(), observedSeedAddress, dial)
				want := failure
				if shape == "nil connection" {
					want = nil
				}
				assertEmptyLeaderObservation(t, observedLeaderResult{member, key, err}, want)
				if f.calls.Load() != int32(index) {
					t.Fatal("dial failure retried another physical connection")
				}
				if shape == "connection with error" {
					f.check(t, index, false)
				} else if index == 2 {
					f.mu.Lock()
					seed := f.rounds[0]
					f.mu.Unlock()
					waitQuorumOwnerDone(t, seed.done)
					if seed.err != nil || !seed.original.closed.Load() || seed.original.calls.Load() != 1 {
						t.Fatal("failed candidate dial lost the original seed cleanup", seed.err)
					}
				}
			})
		}
	}
}

func TestObserveRaftLeaderJoinsHeldOriginalClose(t *testing.T) {
	for _, stage := range []string{"seed", "candidate"} {
		for _, mode := range []string{"success", "close failure", "cancel during close"} {
			t.Run(stage+"/"+mode, func(t *testing.T) {
				f := newObservedLeaderFixture(t, observedLeaderHint)
				index := 1
				if stage == "candidate" {
					index = 2
				}
				failure := errors.New("actual original Close acknowledgement failed")
				held := &quorumHeldCloseConn{entered: make(chan struct{}), release: make(chan struct{})}
				if mode == "close failure" {
					held.failure = failure
				}
				f.wrap = func(call int, conn net.Conn) net.Conn {
					if call == index {
						held.Conn = conn
						return held
					}
					return conn
				}
				var once sync.Once
				release := func() { once.Do(func() { close(held.release) }) }
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan struct{})
				result := make(chan observedLeaderResult, 1)
				go func() {
					defer close(done)
					member, key, err := ObserveRaftLeader(ctx, observedSeedAddress, f.dial)
					result <- observedLeaderResult{member, key, err}
				}()
				t.Cleanup(func() { release(); cancel(); waitQuorumOwnerDone(t, done) })
				waitQuorumOwnerDone(t, held.entered)
				if mode == "cancel during close" {
					cancel()
				}
				quorumOwnerStillWaiting(t, done)
				if f.calls.Load() != int32(index) {
					t.Fatal("next stage started before original Close acknowledged")
				}
				release()
				waitQuorumOwnerDone(t, done)
				got := <-result
				if mode == "success" {
					if got.err != nil || got.member.ID != 11 || got.key != f.keys[1] {
						t.Fatalf("joined observation failed: %+v", got)
					}
					f.check(t, 2, true)
				} else {
					want := failure
					if mode == "cancel during close" {
						want = context.Canceled
					}
					assertEmptyLeaderObservation(t, got, want)
					f.check(t, index, true)
				}
				if held.calls.Load() != 1 {
					t.Fatal("held original Close was repeated")
				}
			})
		}
	}
}

func TestObserveRaftLeaderCancellationJoinsLatePhysicalDial(t *testing.T) {
	for _, stage := range []string{"seed", "candidate"} {
		t.Run(stage, func(t *testing.T) {
			f := newObservedLeaderFixture(t, observedLeaderHint)
			index := 1
			if stage == "candidate" {
				index = 2
			}
			started, canceled, releaseDial := make(chan struct{}), make(chan struct{}), make(chan struct{})
			held := &quorumHeldCloseConn{entered: make(chan struct{}), release: make(chan struct{})}
			f.wrap = func(call int, conn net.Conn) net.Conn {
				if call == index {
					held.Conn = conn
					return held
				}
				return conn
			}
			var dialOnce, closeOnce sync.Once
			unblockDial := func() { dialOnce.Do(func() { close(releaseDial) }) }
			unblockClose := func() { closeOnce.Do(func() { close(held.release) }) }
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan struct{})
			result := make(chan observedLeaderResult, 1)
			go func() {
				defer close(done)
				member, key, err := ObserveRaftLeader(ctx, observedSeedAddress, func(ctx context.Context, address string) (net.Conn, string, error) {
					if int(f.calls.Load())+1 == index {
						close(started)
						<-ctx.Done()
						close(canceled)
						<-releaseDial
					}
					return f.dial(ctx, address)
				})
				result <- observedLeaderResult{member, key, err}
			}()
			t.Cleanup(func() { unblockDial(); unblockClose(); cancel(); waitQuorumOwnerDone(t, done) })
			waitQuorumOwnerDone(t, started)
			cancel()
			waitQuorumOwnerDone(t, canceled)
			quorumOwnerStillWaiting(t, done)
			unblockDial()
			waitQuorumOwnerDone(t, held.entered)
			quorumOwnerStillWaiting(t, done)
			unblockClose()
			waitQuorumOwnerDone(t, done)
			assertEmptyLeaderObservation(t, <-result, context.Canceled)
			f.check(t, index, false)
		})
	}
}

type observedRequestFault struct {
	net.Conn
	deadlineErr error
	shortWrite  bool
}

func (c *observedRequestFault) SetDeadline(deadline time.Time) error {
	if c.deadlineErr != nil {
		return c.deadlineErr
	}
	return c.Conn.SetDeadline(deadline)
}

func (c *observedRequestFault) Write(data []byte) (int, error) {
	if c.shortWrite {
		return c.Conn.Write(data[:len(data)-1])
	}
	return c.Conn.Write(data)
}

func TestObserveRaftLeaderHintTransportFailuresCloseOriginal(t *testing.T) {
	for _, name := range []string{"deadline", "short write", "truncated header", "truncated body"} {
		t.Run(name, func(t *testing.T) {
			f := newObservedLeaderFixture(t, observedLeaderHint)
			failure := errors.New("actual deadline installation failed")
			switch name {
			case "deadline", "short write":
				f.wrap = func(call int, conn net.Conn) net.Conn {
					if call != 1 {
						return conn
					}
					fault := &observedRequestFault{Conn: conn, shortWrite: name == "short write"}
					if name == "deadline" {
						fault.deadlineErr = failure
					}
					return fault
				}
			case "truncated header":
				f.packets[0] = f.packets[0][:7]
			case "truncated body":
				f.packets[0] = f.packets[0][:len(f.packets[0])-1]
			}
			member, key, err := ObserveRaftLeader(t.Context(), observedSeedAddress, f.dial)
			want := error(nil)
			if name == "deadline" {
				want = failure
			} else if name == "short write" {
				want = io.ErrShortWrite
			}
			assertEmptyLeaderObservation(t, observedLeaderResult{member, key, err}, want)
			f.check(t, 1, name != "deadline" && name != "short write")
		})
	}
}

func TestObserveRaftLeaderHonorsShorterParentDeadline(t *testing.T) {
	f := newObservedLeaderFixture(t, observedLeaderHint)
	f.stall = 1
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	parentDeadline, _ := ctx.Deadline()
	member, key, err := ObserveRaftLeader(ctx, observedSeedAddress, func(ctx context.Context, address string) (net.Conn, string, error) {
		deadline, ok := ctx.Deadline()
		if !ok || !deadline.Equal(parentDeadline) {
			return nil, "", errors.New("attempt extended parent's deadline")
		}
		return f.dial(ctx, address)
	})
	assertEmptyLeaderObservation(t, observedLeaderResult{member, key, err}, context.DeadlineExceeded)
	f.check(t, 1, true)
}
