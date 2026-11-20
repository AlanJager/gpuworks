package slurm

import (
	"context"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
)

// ExternalCardHolders is the cards other Slurm jobs hold on this node, each
// named by the job (the agent's externalCardHolders): Slurm handed them to
// somebody else, whether or not a process uses them yet. Keyed by card UUID.
func (l *Leaser) ExternalCardHolders(ctx context.Context) (map[string][]proto.CardProcess, error) {
	jobs, err := l.nodeJobs(ctx)
	if err != nil {
		return nil, err
	}
	l.detailMu.Lock()
	defer l.detailMu.Unlock()
	live := map[string]bool{}
	holders := map[string][]proto.CardProcess{}
	for _, j := range jobs {
		live[j.id] = true
		if strings.HasPrefix(j.name, HoldPrefix) && l.ownUser(j.user) {
			continue
		}
		jc, ok := l.details[j.id]
		if !ok {
			record, err := l.jobRecord(ctx, j.id)
			if err != nil {
				return nil, err
			}
			cards, err := l.jobCardsOnNode(ctx, record)
			if err != nil {
				return nil, err
			}
			jc = jobCards{owner: j.user, cards: cards}
			if settled(record, cards) {
				l.details[j.id] = jc
			}
		}
		for _, ident := range jc.cards {
			uuid := l.uuidOf(ident)
			if uuid == "" {
				continue
			}
			holders[uuid] = append(holders[uuid], proto.CardProcess{Name: fmt.Sprintf("Slurm job %s (%s)", j.id, jc.owner)})
		}
	}
	for id := range l.details {
		if !live[id] {
			delete(l.details, id)
		}
	}
	return holders, nil
}

// settled says a job record's cards on this node will not change: the job
// runs, and it either holds its GPUs here or asked for none. A record of a
// job still configuring or already completing is read again next round
// rather than kept for the life of the job.
func settled(record string, cards []string) bool {
	f := fields(record)
	if f["JobState"] != "RUNNING" {
		return false
	}
	return len(cards) > 0 || !strings.Contains(f["TRES"], "gres/gpu")
}

// uuidOf is the UUID of a card the ledger named.
func (l *Leaser) uuidOf(ident string) string {
	l.cardMu.Lock()
	defer l.cardMu.Unlock()
	return l.uuidOfIdent[ident]
}
