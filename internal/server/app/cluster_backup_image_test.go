package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster"
)

// backupDatabase behaves like the members a backup reads: one leads in a
// term; at chosen attempts the leadership moves while the image is read
// (the read from the former leader fails, or succeeds and the second barrier
// names the new term), or no member answers as the leader.
type backupDatabase struct {
	term, index uint64
	leader      string
	// moveDuringRead lists attempts (from 1) in which the leadership moves
	// while the image is read; readFails makes that read fail instead of
	// returning. noLeader lists attempts in which no member leads.
	moveDuringRead, noLeader map[int]bool
	readFails                bool
	// reelected makes the member that led win the election again: the
	// second barrier is its own, in a later term.
	reelected       bool
	attempts, reads int
}

func (d *backupDatabase) read() clusterBackupRead {
	ids := map[string]uint64{"a:9001": 1, "b:9001": 2}
	proof := func() cluster.QuorumProof {
		d.index++
		return cluster.QuorumProof{Term: d.term, Index: d.index, LeaderID: ids[d.leader]}
	}
	return clusterBackupRead{
		pause: time.Millisecond,
		leader: func(ctx context.Context) (cluster.QuorumProof, string, error) {
			d.attempts++
			if d.noLeader[d.attempts] {
				return cluster.QuorumProof{}, "", errors.Join(ErrClusterBackupUnavailable, errors.New("no member answered as the leader"))
			}
			return proof(), d.leader, nil
		},
		image: func(ctx context.Context, leader string) ([]byte, error) {
			d.reads++
			if d.moveDuringRead[d.attempts] {
				d.term++
				if d.reelected {
					return []byte("image read from " + leader), nil
				}
				if d.leader == "a:9001" {
					d.leader = "b:9001"
				} else {
					d.leader = "a:9001"
				}
				if d.readFails {
					return nil, errors.New("read tcp: connection reset by peer")
				}
			}
			return []byte("image read from " + leader), nil
		},
		proof: func(ctx context.Context, leader string) (cluster.QuorumProof, error) {
			if leader != d.leader {
				return cluster.QuorumProof{}, errors.New("not leader")
			}
			return proof(), nil
		},
	}
}

func TestClusterBackupReadsAgainWhenTheLeadershipMoves(t *testing.T) {
	cases := map[string]struct {
		db          backupDatabase
		wantFrom    string
		wantReads   int
		wantFailure string
	}{
		"a leader that stays":                           {backupDatabase{}, "a:9001", 1, ""},
		"moved during the first read, the read failed":  {backupDatabase{moveDuringRead: map[int]bool{1: true}, readFails: true}, "b:9001", 2, ""},
		"moved during the first read, the read went on": {backupDatabase{moveDuringRead: map[int]bool{1: true}}, "b:9001", 2, ""},
		"no leader at the first attempt":                {backupDatabase{noLeader: map[int]bool{1: true}}, "a:9001", 1, ""},
		"moved twice":                                   {backupDatabase{moveDuringRead: map[int]bool{1: true, 2: true}, readFails: true}, "a:9001", 3, ""},
		"moved in every attempt, reads failing":         {backupDatabase{moveDuringRead: map[int]bool{1: true, 2: true, 3: true}, readFails: true}, "", 3, "connection reset by peer"},
		"moved in every attempt, reads going on":        {backupDatabase{moveDuringRead: map[int]bool{1: true, 2: true, 3: true}}, "", 3, "not leader"},
		"re-elected during the first read":              {backupDatabase{moveDuringRead: map[int]bool{1: true}, reelected: true}, "a:9001", 2, ""},
		"re-elected in every attempt":                   {backupDatabase{moveDuringRead: map[int]bool{1: true, 2: true, 3: true}, reelected: true}, "", 3, "the leadership moved while the database was read"},
		"no leader in any attempt":                      {backupDatabase{noLeader: map[int]bool{1: true, 2: true, 3: true}}, "", 0, "no member answered as the leader"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			db := c.db
			db.term, db.leader = 7, "a:9001"
			image, before, after, err := clusterBackupImage(t.Context(), db.read())
			if c.wantFailure != "" {
				if err == nil || !errors.Is(err, ErrClusterBackupUnavailable) || !strings.Contains(err.Error(), c.wantFailure) || !strings.Contains(err.Error(), "3 attempts") || image != nil {
					t.Fatalf("backup = %q, %v; want no image and a failure naming %q after three attempts", image, err, c.wantFailure)
				}
			} else {
				if err != nil || string(image) != "image read from "+c.wantFrom {
					t.Fatalf("backup = %q, %v; want the image read from %s", image, err, c.wantFrom)
				}
				if before.Term != after.Term || before.LeaderID != after.LeaderID || after.Index < before.Index {
					t.Fatalf("the image is not between two barriers of one leader in one term: %+v, %+v", before, after)
				}
			}
			if db.reads != c.wantReads || db.attempts > clusterBackupAttempts {
				t.Fatalf("%d reads in %d attempts; want %d reads and at most %d attempts", db.reads, db.attempts, c.wantReads, clusterBackupAttempts)
			}
		})
	}
}

// A caller that gives up ends the backup between two attempts, with the
// failure of the attempt before.
func TestClusterBackupStopsWhenTheCallerGivesUp(t *testing.T) {
	db := backupDatabase{term: 7, leader: "a:9001", moveDuringRead: map[int]bool{1: true}, readFails: true}
	ctx, cancel := context.WithCancel(t.Context())
	read := db.read()
	read.pause = time.Hour
	inner := read.image
	read.image = func(ctx context.Context, leader string) ([]byte, error) {
		defer cancel()
		return inner(ctx, leader)
	}
	_, _, _, err := clusterBackupImage(ctx, read)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "connection reset by peer") || db.reads != 1 {
		t.Fatalf("backup = %v after %d reads; want the caller's end and the first failure", err, db.reads)
	}
}
