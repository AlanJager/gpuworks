package store

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// SlurmGPUNode is one node of a Slurm cluster as an enrolled agent last listed
// it with sinfo: the platform's expected set of nodes, which the reconciliation
// compares with the nodes that enrolled. A node whose GRES has gpu is a GPU
// node (the platform expects an agent on it); every other node of the cluster
// is kept by name alone (ListedOnly), so that "no longer listed" means "left
// the cluster" rather than "has no GRES, or sits in a hidden partition". A
// report replaces the cluster's rows as a whole when its membership changed,
// and otherwise only the rows whose state or reason did.
type SlurmGPUNode struct {
	ID        uint   `gorm:"primaryKey"`
	ClusterID uint   `gorm:"uniqueIndex:idx_slurm_gpu_node,priority:1"`
	Name      string `gorm:"size:64;uniqueIndex:idx_slurm_gpu_node,priority:2"`
	// ListedOnly marks a node of the cluster that has no gpu GRES: it counts
	// for nothing but the knowledge that Slurm still has it. Rows written
	// before the column existed are GPU nodes.
	ListedOnly bool
	// State is Slurm's base state of the node (idle, mixed, allocated, down,
	// drained, ...), lower case, as sinfo printed it. Empty for a listed-only
	// node.
	State string `gorm:"size:32"`
	// Reason is Slurm's own Reason text for a node that is down or drained,
	// truncated, shown as written; never parsed.
	Reason string `gorm:"size:256"`
	// GPUs is the number of GPUs Slurm's Gres lists for the node.
	GPUs int
}

// slurmMembershipDigest is a digest of the facts that make a list's
// membership (name, whether it is a GPU node, how many GPUs) in name order:
// what decides whether the rows are replaced. State and reason change all the
// time and are not part of it.
func slurmMembershipDigest(nodes []SlurmGPUNode) string {
	sorted := append([]SlurmGPUNode(nil), nodes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	h := sha256.New()
	for _, n := range sorted {
		h.Write([]byte(n.Name + "\x00" + strconv.FormatBool(n.ListedOnly) + "\x00" + strconv.Itoa(n.GPUs) + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// SlurmListChange is what storing a report did.
type SlurmListChange struct {
	// Replaced: the membership differed, and the rows were replaced.
	Replaced bool
	// StatesUpdated is the number of rows whose state or reason were updated
	// in place (always 0 when the rows were replaced).
	StatesUpdated int
}

// Changed says whether anything was written.
func (c SlurmListChange) Changed() bool { return c.Replaced || c.StatesUpdated > 0 }

// ReplaceSlurmGPUNodes stores nodes as the cluster's list, in one
// transaction. A list with the stored membership writes only the rows whose
// state or reason differ, and nothing when none does; a different membership
// replaces the rows.
func (s *Store) ReplaceSlurmGPUNodes(clusterID uint, nodes []SlurmGPUNode) (SlurmListChange, error) {
	var change SlurmListChange
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var have []SlurmGPUNode
		if err := tx.Where("cluster_id = ?", clusterID).Find(&have).Error; err != nil {
			return err
		}
		if len(have) == len(nodes) && slurmMembershipDigest(have) == slurmMembershipDigest(nodes) {
			stored := make(map[string]SlurmGPUNode, len(have))
			for _, n := range have {
				stored[n.Name] = n
			}
			for _, n := range nodes {
				h := stored[n.Name]
				if h.State == n.State && h.Reason == n.Reason {
					continue
				}
				if err := tx.Model(&SlurmGPUNode{}).Where("id = ?", h.ID).
					Updates(map[string]any{"state": n.State, "reason": n.Reason}).Error; err != nil {
					return err
				}
				change.StatesUpdated++
			}
			return nil
		}
		change.Replaced = true
		if err := tx.Where("cluster_id = ?", clusterID).Delete(&SlurmGPUNode{}).Error; err != nil {
			return err
		}
		rows := make([]SlurmGPUNode, len(nodes))
		for i, n := range nodes {
			rows[i] = SlurmGPUNode{ClusterID: clusterID, Name: n.Name, ListedOnly: n.ListedOnly, State: n.State, Reason: n.Reason, GPUs: n.GPUs}
		}
		if len(rows) == 0 {
			return nil
		}
		return tx.CreateInBatches(rows, 200).Error
	})
	return change, err
}

// SlurmGPUNodeCounts counts each cluster's listed nodes, in one query.
func (s *Store) SlurmGPUNodeCounts() (map[uint]int, error) {
	var rows []struct {
		ClusterID uint
		N         int
	}
	err := s.reader().Model(&SlurmGPUNode{}).Select("cluster_id, count(*) AS n").Where("listed_only = ?", false).Group("cluster_id").Scan(&rows).Error
	out := map[uint]int{}
	for _, r := range rows {
		out[r.ClusterID] = r.N
	}
	return out, err
}

// SlurmGPUNodeNames lists the names of a cluster's listed nodes, by the
// unique index.
func (s *Store) SlurmGPUNodeNames(clusterID uint) ([]string, error) {
	var names []string
	err := s.reader().Model(&SlurmGPUNode{}).Where("cluster_id = ? AND listed_only = ?", clusterID, false).Order("name").Pluck("name", &names).Error
	return names, err
}

// SlurmMissingNode is a listed GPU node that has no agent online: its agent
// never enrolled (Enrolled false), or enrolled and is offline.
type SlurmMissingNode struct {
	ClusterID uint
	Name      string
	State     string
	Reason    string
	Enrolled  bool
	// NodeUUID is the enrolled node's, "" when none enrolled.
	NodeUUID string
}

// SlurmMissingNodes lists, for the given clusters (every cluster when none is
// given), the listed GPU nodes without an online agent, by name. One query joined on the unique node index (its
// `slurm_cluster_id > 0` is what lets SQLite use that partial index); a healthy
// cluster contributes no rows.
func (s *Store) SlurmMissingNodes(clusterIDs ...uint) ([]SlurmMissingNode, error) {
	var rows []SlurmMissingNode
	filter, args := "", []any{NodeStateOnline}
	if len(clusterIDs) > 0 {
		filter, args = " AND g.cluster_id IN ?", append(args, clusterIDs)
	}
	err := s.reader().Raw(`SELECT g.cluster_id AS cluster_id, g.name AS name, g.state AS state, g.reason AS reason, n.id IS NOT NULL AS enrolled, coalesce(n.uuid, '') AS node_uuid
		FROM slurm_gpu_nodes g LEFT JOIN nodes n ON n.slurm_cluster_id > 0 AND n.slurm_cluster_id = g.cluster_id AND n.slurm_node = g.name
		WHERE NOT g.listed_only AND (n.id IS NULL OR n.state <> ?)`+filter+`
		ORDER BY g.cluster_id, g.name`, args...).Scan(&rows).Error
	return rows, err
}

// OfflineSlurmNodes lists the nodes of Slurm entries whose agents have been
// gone since before cutoff and whose Slurm node is no longer among the nodes
// their entry last reported, GPU nodes or not (a node without GRES, or in a
// hidden partition, is still in its cluster): the candidates for having left
// their cluster. An entry that has reported no node condemns nothing. Offline nodes are
// few, and the state column is indexed; the two subqueries use the list's
// unique index.
func (s *Store) OfflineSlurmNodes(cutoff time.Time) ([]Node, error) {
	var nodes []Node
	err := s.reader().Where(`state = ? AND slurm_cluster_id > 0 AND (last_seen_at IS NULL OR last_seen_at < ?)
		AND EXISTS (SELECT 1 FROM slurm_gpu_nodes g WHERE g.cluster_id = nodes.slurm_cluster_id)
		AND NOT EXISTS (SELECT 1 FROM slurm_gpu_nodes g WHERE g.cluster_id = nodes.slurm_cluster_id AND g.name = nodes.slurm_node)`,
		NodeStateOffline, cutoff).Order("id").Find(&nodes).Error
	return nodes, err
}

// OfflineCloudSlurmNodes lists the nodes of entries made from a cloud
// account's cluster whose agents have been gone since before cutoff, listed
// or not: the cloud lists the cluster's nodes itself, so it is asked about
// them (slurmsweep). Offline nodes are few, and the state column is indexed.
func (s *Store) OfflineCloudSlurmNodes(cutoff time.Time) ([]Node, error) {
	var nodes []Node
	err := s.reader().Where(`state = ? AND slurm_cluster_id > 0 AND (last_seen_at IS NULL OR last_seen_at < ?)
		AND EXISTS (SELECT 1 FROM slurm_clusters c WHERE c.id = nodes.slurm_cluster_id AND c.provider_id IS NOT NULL)`,
		NodeStateOffline, cutoff).Order("id").Find(&nodes).Error
	return nodes, err
}

// forgetSlurmListedNode takes a node that left its cluster off the list its
// entry's agents last reported, in the transaction that deletes it: the list
// may be the last one an agent reported before the node went, and no agent
// may be left to report a newer one. A node that is still in the cluster is
// listed again by the next report.
func forgetSlurmListedNode(tx *gorm.DB, n Node) error {
	if n.SlurmClusterID == 0 || n.SlurmNode == "" {
		return nil
	}
	return tx.Where("cluster_id = ? AND name = ?", n.SlurmClusterID, n.SlurmNode).Delete(&SlurmGPUNode{}).Error
}

// SlurmClustersWithSharedStorage are the entries some node of which has an
// enabled shared storage (nfs, nfs_cache, shared_dir) applied to it, as the
// node's storage states say, in one grouped query: an entry that has one is
// not told to register its cluster's shared filesystem as model storage.
func (s *Store) SlurmClustersWithSharedStorage() (map[uint]bool, error) {
	var ids []uint
	err := s.reader().Raw(`SELECT DISTINCT n.slurm_cluster_id FROM storage_node_states ns
		JOIN nodes n ON n.id = ns.node_id JOIN storages st ON st.id = ns.storage_id
		WHERE n.slurm_cluster_id > 0 AND st.enabled AND st.type IN ?`,
		[]string{proto.StorageNFS, proto.StorageNFSCache, proto.StorageSharedDir}).Scan(&ids).Error
	out := make(map[uint]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, err
}
