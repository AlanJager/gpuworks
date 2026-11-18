package planning

import (
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// SlurmClusterRef is what a node's Slurm cluster label is made of: the
// entry's UUID (the label value, stable across renames) and its name (the
// text shown).
type SlurmClusterRef struct {
	UUID string
	Name string
}

// SlurmClusters maps a Slurm cluster entry id to its UUID and name.
type SlurmClusters map[uint]SlurmClusterRef

// SlurmClusterRefReader is the one read the labels need of the store
// (*store.Store satisfies it): the identity of entries by id.
type SlurmClusterRefReader interface {
	SlurmClusterRefs(ids []uint) (map[uint]store.SlurmCluster, error)
}

// LoadSlurmClusters reads the entries nodes joined from with one query over
// the distinct entry ids, naming columns that hold no enrollment token; nil
// when no node joined from one or the read fails (a node then carries no
// cluster label, never a wrong one).
func LoadSlurmClusters(st SlurmClusterRefReader, nodes []store.Node) SlurmClusters {
	seen := map[uint]bool{}
	var ids []uint
	for i := range nodes {
		if id := nodes[i].SlurmClusterID; id != 0 && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := st.SlurmClusterRefs(ids)
	if err != nil {
		return nil
	}
	out := make(SlurmClusters, len(rows))
	for id, row := range rows {
		out[id] = SlurmClusterRef{UUID: row.UUID, Name: row.Name}
	}
	return out
}

// RequirementsNeedSlurmClusters reports whether matching reqs reads the name
// or UUID of a node's Slurm cluster entry. The Slurm node label comes from the
// node row alone and needs nothing loaded.
func RequirementsNeedSlurmClusters(reqs nodelabel.Requirements) bool {
	return len(reqs[nodelabel.KeySlurmCluster]) > 0
}

// slurmLabels say which Slurm cluster entry and which Slurm node a node
// joined from; a node added any other way carries none (a node that only
// borrows cards from a Slurm it was installed on by hand is not one that
// joined from an entry). The cluster label is left off when the entries were
// not loaded or the entry is gone.
func slurmLabels(node *store.Node, clusters SlurmClusters) []SystemLabel {
	if node.SlurmClusterID == 0 {
		return nil
	}
	var labels []SystemLabel
	if cluster, ok := clusters[node.SlurmClusterID]; ok && cluster.UUID != "" {
		display := cluster.Name
		if display == "" {
			display = cluster.UUID
		}
		labels = append(labels, SystemLabel{Key: nodelabel.KeySlurmCluster, Value: cluster.UUID, Display: display})
	}
	if node.SlurmNode != "" {
		labels = append(labels, SystemLabel{Key: nodelabel.KeySlurmNode, Value: node.SlurmNode, Display: node.SlurmNode})
	}
	return labels
}
