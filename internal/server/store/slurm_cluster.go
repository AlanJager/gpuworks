package store

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

// A Slurm cluster joins like a Kubernetes one (kube_cluster.go): the entry
// holds one enrollment token, a health-check program the administrator puts
// in slurm.conf runs on every compute node, and the one on a GPU node
// installs an agent that trades the token for a node token of its own
// (EnrollSlurmNode). After that each one is an ordinary node; the entry only
// answers "which cluster is this node in", renders the scripts again, and
// stops new nodes joining when revoked.

// Defaults of the settings a Slurm entry hands its agents.
const (
	// SlurmDefaultIdleReturnSeconds returns a card a minute after the last
	// instance on it stopped (the leaser's own default).
	SlurmDefaultIdleReturnSeconds = 60
	// SlurmDefaultPendingTimeoutSeconds is how long a placeholder job may wait
	// for Slurm to start it before the instance fails (the leaser's default).
	SlurmDefaultPendingTimeoutSeconds = 600
	// SlurmDefaultRunAs is the account the agents run as.
	SlurmDefaultRunAs = "root"
	// SlurmDefaultInstallDir is where each agent is installed, the same path
	// on every node.
	SlurmDefaultInstallDir = "/var/lib/gpuworks"
	// SlurmDefaultScriptDir is where the health-check program lives, the same
	// path on every node (it should be on shared storage).
	SlurmDefaultScriptDir = "/opt/gpuworks-slurm"
)

// SlurmCluster is a Slurm cluster whose GPU nodes join through its health
// check. The fields below the token are the scripts' and the agents'
// parameters, stored so the same scripts can be served again.
type SlurmCluster struct {
	ID   uint   `gorm:"primaryKey"`
	UUID string `gorm:"uniqueIndex;size:64"`
	Name string `gorm:"uniqueIndex;size:128"`
	// Token is the enrollment token, kept in plaintext like Node.Token so the
	// scripts that carry it can be rendered again; TokenHash is the index
	// enrollment looks it up by.
	Token     string `gorm:"size:128"`
	TokenHash string `gorm:"uniqueIndex;size:128"`
	// SlurmClusterName is Slurm's own ClusterName, recorded by the first node
	// that enrolls. From then on the token is refused to nodes of any other
	// Slurm cluster.
	SlurmClusterName string `gorm:"size:128"`
	// ServerURL is the address the agents connect to (ADR 0025: stated by the
	// operator, never inferred).
	ServerURL string `gorm:"size:512"`
	// RunAs is the account the agents run as (the systemd unit's User=).
	RunAs string `gorm:"size:32"`
	// AccountUser is the account the placeholder jobs are submitted as when
	// the agent runs as root (sbatch --uid); empty submits as the agent's own
	// account.
	AccountUser string `gorm:"size:32"`
	// InstallDir is each agent's install and state directory; ScriptDir holds
	// the health-check program and is the same path on every node.
	InstallDir string `gorm:"size:255"`
	ScriptDir  string `gorm:"size:255"`
	// SbatchArgs are extra options of every placeholder job, split on spaces
	// by the agent.
	SbatchArgs string `gorm:"size:512"`
	// IdleReturnSeconds is how long a borrowed card stays the platform's after
	// the last instance on it stopped; PendingTimeoutSeconds bounds how long a
	// placeholder job may wait to start.
	IdleReturnSeconds     int
	PendingTimeoutSeconds int
	// ProviderID and ProviderClusterID name the cloud account's HPC cluster
	// this entry was made from (an E-HPC cluster); both are empty for an entry
	// made by hand, and ProviderID is cleared when the account is deleted.
	ProviderID        *uint  `gorm:"index:idx_slurm_cluster_source,priority:1"`
	ProviderClusterID string `gorm:"size:128;index:idx_slurm_cluster_source,priority:2"`
	RevokedAt         *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Errors EnrollSlurmNode returns for a token it will not honour.
var (
	ErrSlurmTokenUnknown    = errors.New("unknown or revoked enrollment token")
	ErrSlurmClusterMismatch = errors.New("the enrollment token belongs to another Slurm cluster")
	ErrSlurmNodeTaken       = errors.New("another entry of the cluster holds this node")
)

// ErrSlurmClusterNameTaken refuses naming an entry after another one.
var ErrSlurmClusterNameTaken = errors.New("another Slurm cluster entry has this name")

// ErrSlurmClusterSourceTaken: another entry was already made from the same
// cloud account's HPC cluster.
var ErrSlurmClusterSourceTaken = errors.New("an entry for this cluster already exists")

// ErrSlurmClusterHasNodes refuses deleting a cluster whose nodes are still
// registered: they would be left pointing at nothing.
var ErrSlurmClusterHasNodes = errors.New("the cluster still has nodes")

// slurmNodeIs selects the node of one Slurm entry by its Slurm name. The
// `slurm_cluster_id > 0` is what lets SQLite use the partial unique index
// idx_node_slurm, so enrollment, which every agent does on every start, never
// reads the whole node table.
const slurmNodeIs = "slurm_cluster_id > 0 AND slurm_cluster_id = ? AND slurm_node = ?"

// SlurmNodeTakenFor is how long after its agent was last seen a node keeps
// belonging to the entry that has it: within it another entry of the same
// Slurm cluster cannot enroll the same node (two agents would manage one
// node's cards).
const SlurmNodeTakenFor = 15 * time.Minute

// SlurmNodeTakenError names the entry that holds the node.
type SlurmNodeTakenError struct{ Entry string }

func (e *SlurmNodeTakenError) Error() string {
	return fmt.Sprintf("%s: %s", ErrSlurmNodeTaken, e.Entry)
}

func (e *SlurmNodeTakenError) Unwrap() error { return ErrSlurmNodeTaken }

// slurmClusterEditable are the columns UpdateSlurmCluster writes. The server
// URL, the agents' account and the two directories stay as the entry was made:
// the scripts already on the nodes name them, so changing one would leave the
// agents and the files behind. The settings an agent takes when it starts
// (sbatch arguments, idle return, pending timeout, accounting user) are
// editable.
var slurmClusterEditable = []string{
	"name", "account_user", "sbatch_args", "idle_return_seconds", "pending_timeout_seconds",
}

// CreateSlurmCluster stores a cluster with a fresh enrollment token.
func (s *Store) CreateSlurmCluster(c SlurmCluster) (*SlurmCluster, error) {
	c.ID = 0
	c.UUID = auth.NewUUID()
	c.Token = auth.NewToken(24)
	if c.Token == "" {
		return nil, errors.New("generate enrollment token")
	}
	c.TokenHash = HashToken(c.Token)
	c.SlurmClusterName = ""
	c.RevokedAt = nil
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var n int64
		if err := tx.Model(&SlurmCluster{}).Where("name = ?", c.Name).Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return ErrSlurmClusterNameTaken
		}
		if c.ProviderID != nil {
			if err := tx.Model(&SlurmCluster{}).Where("provider_id = ? AND provider_cluster_id = ?", *c.ProviderID, c.ProviderClusterID).Count(&n).Error; err != nil {
				return err
			}
			if n > 0 {
				return ErrSlurmClusterSourceTaken
			}
		}
		return tx.Create(&c).Error
	})
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// SlurmClusters lists every cluster, oldest first.
func (s *Store) SlurmClusters() ([]SlurmCluster, error) {
	var rows []SlurmCluster
	err := s.db.Order("id").Find(&rows).Error
	return rows, err
}

// SlurmClusterRefs returns the id, UUID and name of the clusters with the
// given ids, keyed by id; a missing id is absent. It reads only the identity
// columns, so a label computation never loads enrollment tokens.
func (s *Store) SlurmClusterRefs(ids []uint) (map[uint]SlurmCluster, error) {
	out := map[uint]SlurmCluster{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []SlurmCluster
	if err := s.reader().Select("id", "uuid", "name").Where("id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.ID] = row
	}
	return out, nil
}

// SlurmClusterByUUID returns one cluster or ErrNotFound.
func (s *Store) SlurmClusterByUUID(uuid string) (*SlurmCluster, error) {
	var c SlurmCluster
	err := s.db.Where("uuid = ?", uuid).First(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &c, err
}

// SlurmClusterByID returns one cluster by primary key or ErrNotFound.
func (s *Store) SlurmClusterByID(id uint) (*SlurmCluster, error) {
	var c SlurmCluster
	err := s.reader().Where("id = ?", id).First(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &c, err
}

// SlurmClusterByToken returns the live cluster an enrollment token belongs to.
func (s *Store) SlurmClusterByToken(token string) (*SlurmCluster, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, ErrSlurmTokenUnknown
	}
	var c SlurmCluster
	err := s.db.Where("token_hash = ? AND revoked_at IS NULL", HashToken(token)).First(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrSlurmTokenUnknown
	}
	return &c, err
}

// SlurmClusterByAnyToken returns the cluster an enrollment token belongs to,
// revoked or not: the scripts that take the health check out again must still
// be served after the entry stopped taking new nodes.
func (s *Store) SlurmClusterByAnyToken(token string) (*SlurmCluster, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, ErrSlurmTokenUnknown
	}
	var c SlurmCluster
	err := s.db.Where("token_hash = ?", HashToken(token)).First(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrSlurmTokenUnknown
	}
	return &c, err
}

// SlurmNodeCounts is how many nodes of an entry enrolled, and how many of
// them have their agent online.
type SlurmNodeCounts struct {
	Enrolled, Online int
}

// SlurmClusterNodeCounts counts each entry's nodes, and its nodes online, in
// one query.
func (s *Store) SlurmClusterNodeCounts() (map[uint]SlurmNodeCounts, error) {
	var rows []struct {
		SlurmClusterID uint
		N, Online      int
	}
	err := s.reader().Model(&Node{}).Select("slurm_cluster_id, count(*) AS n, coalesce(sum(CASE WHEN state = ? THEN 1 ELSE 0 END), 0) AS online", NodeStateOnline).
		Where("slurm_cluster_id > 0").Group("slurm_cluster_id").Scan(&rows).Error
	out := map[uint]SlurmNodeCounts{}
	for _, r := range rows {
		out[r.SlurmClusterID] = SlurmNodeCounts{Enrolled: r.N, Online: r.Online}
	}
	return out, err
}

// SlurmClustersLastSeen is when an agent of each entry was last seen, in
// one query for all of them: with no agent online, the newest list of the
// entry's nodes is at most this new, since only its agents report one. An
// entry none of whose nodes was ever seen is absent. It reads one row per
// entry, by the partial slurm node index (hence slurm_cluster_id > 0).
func (s *Store) SlurmClustersLastSeen(clusterIDs ...uint) (map[uint]time.Time, error) {
	out := make(map[uint]time.Time, len(clusterIDs))
	if len(clusterIDs) == 0 {
		return out, nil
	}
	// SQLite gives a bare column of an aggregate query the row the MAX came
	// from, so last_seen_at is that row's, read with its own column type (a
	// MAX(...) column would come back as text).
	var nodes []Node
	err := s.reader().Select("slurm_cluster_id", "last_seen_at", "MAX(last_seen_at) AS newest_seen").
		Where("slurm_cluster_id > 0 AND slurm_cluster_id IN ? AND last_seen_at IS NOT NULL", clusterIDs).
		Group("slurm_cluster_id").
		Find(&nodes).Error
	for _, n := range nodes {
		if seen := n.LastSeenAt.UTC(); seen.After(out[n.SlurmClusterID]) {
			out[n.SlurmClusterID] = seen
		}
	}
	return out, err
}

// SlurmUnlistedNodeCounts counts each entry's nodes that enrolled at or after
// since and that no list the entry's agents reported names (as a GPU node or
// as a listed-only node), in one query: the nodes whose enrollment a report
// may simply not have caught up with. A node that enrolled earlier and is
// still not named has been left out by a list reported since, so it is not
// among them.
func (s *Store) SlurmUnlistedNodeCounts(since time.Time) (map[uint]int, error) {
	var rows []struct {
		SlurmClusterID uint
		N              int
	}
	err := s.reader().Raw(`SELECT n.slurm_cluster_id AS slurm_cluster_id, count(*) AS n FROM nodes n
		WHERE n.slurm_cluster_id > 0 AND n.created_at >= ?
		AND NOT EXISTS (SELECT 1 FROM slurm_gpu_nodes g WHERE g.cluster_id = n.slurm_cluster_id AND g.name = n.slurm_node)
		GROUP BY n.slurm_cluster_id`, since).Scan(&rows).Error
	out := map[uint]int{}
	for _, r := range rows {
		out[r.SlurmClusterID] = r.N
	}
	return out, err
}

// UpdateSlurmCluster writes c's editable settings onto the entry with c's
// UUID and returns the entry as stored.
func (s *Store) UpdateSlurmCluster(c SlurmCluster) (*SlurmCluster, error) {
	var out SlurmCluster
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("uuid = ?", c.UUID).First(&out).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		var n int64
		if err := tx.Model(&SlurmCluster{}).Where("name = ? AND id <> ?", c.Name, out.ID).Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return ErrSlurmClusterNameTaken
		}
		if err := tx.Model(&out).Select(slurmClusterEditable).Updates(&SlurmCluster{
			Name: c.Name, AccountUser: c.AccountUser, SbatchArgs: c.SbatchArgs,
			IdleReturnSeconds: c.IdleReturnSeconds, PendingTimeoutSeconds: c.PendingTimeoutSeconds,
		}).Error; err != nil {
			return err
		}
		return tx.Where("id = ?", out.ID).First(&out).Error
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// RevokeSlurmCluster stops new nodes joining with the cluster's token. Nodes
// that already joined keep their own tokens.
func (s *Store) RevokeSlurmCluster(uuid string) error {
	now := time.Now().UTC()
	res := s.db.Model(&SlurmCluster{}).Where("uuid = ? AND revoked_at IS NULL", uuid).Update("revoked_at", &now)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		if _, err := s.SlurmClusterByUUID(uuid); err != nil {
			return err
		}
	}
	return nil
}

// DeleteSlurmCluster removes a cluster none of whose nodes remain, with the
// GPU node list it last reported.
func (s *Store) DeleteSlurmCluster(uuid string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		var c SlurmCluster
		if err := tx.Where("uuid = ?", uuid).First(&c).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		var n int64
		if err := tx.Model(&Node{}).Where("slurm_cluster_id > 0 AND slurm_cluster_id = ?", c.ID).Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return ErrSlurmClusterHasNodes
		}
		if err := tx.Where("cluster_id = ?", c.ID).Delete(&SlurmGPUNode{}).Error; err != nil {
			return err
		}
		return tx.Delete(&c).Error
	})
}

// EnrollSlurmNode trades a cluster's enrollment token for the node token of
// the Slurm node nodeName. It is idempotent: the same node of the same
// cluster always gets the same platform node back, so a reinstalled agent
// keeps its labels and model files. The first enrollment binds the token to
// slurmCluster (Slurm's ClusterName); any other cluster is refused.
func (s *Store) EnrollSlurmNode(token, slurmCluster, nodeName string) (*Node, *SlurmCluster, error) {
	slurmCluster, nodeName = strings.TrimSpace(slurmCluster), strings.TrimSpace(nodeName)
	if slurmCluster == "" || nodeName == "" || len(nodeName) > 64 || len(slurmCluster) > 128 {
		return nil, nil, errors.New("slurm_cluster_name and node_name are required")
	}
	var node Node
	var cluster SlurmCluster
	created := false
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("token_hash = ? AND revoked_at IS NULL", HashToken(strings.TrimSpace(token))).First(&cluster).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrSlurmTokenUnknown
			}
			return err
		}
		switch {
		case cluster.SlurmClusterName == "":
			cluster.SlurmClusterName = slurmCluster
			if err := tx.Model(&cluster).Update("slurm_cluster_name", slurmCluster).Error; err != nil {
				return err
			}
		case cluster.SlurmClusterName != slurmCluster:
			return ErrSlurmClusterMismatch
		}
		// Another entry of the same Slurm cluster holding the node refuses it
		// even when this entry has a row for it from before: two agents would
		// manage one node's cards.
		if holder, err := slurmNodeHolder(tx, cluster, nodeName, time.Now().UTC()); err != nil || holder != "" {
			if err != nil {
				return err
			}
			return &SlurmNodeTakenError{Entry: holder}
		}
		err := tx.Where(slurmNodeIs, cluster.ID, nodeName).First(&node).Error
		if err == nil {
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		nodeToken := auth.NewToken(24)
		if nodeToken == "" {
			return errors.New("generate node token")
		}
		node = Node{
			UUID: auth.NewUUID(), Name: nodeName, Token: nodeToken, TokenHash: HashToken(nodeToken),
			State: NodeStateConnecting, ConnectMode: "agent_dial",
			SlurmClusterID: cluster.ID, SlurmNode: nodeName,
		}
		created = true
		if err := tx.Create(&node).Error; err != nil {
			return err
		}
		return linkSlurmNodeToInstance(tx, cluster, node)
	})
	if err != nil {
		return nil, nil, err
	}
	if created {
		s.publishNode(node.UUID, "pending")
	}
	return &node, &cluster, nil
}

// slurmNodeHolder names another entry of cluster's Slurm cluster whose agent
// on nodeName is online or was seen within SlurmNodeTakenFor; "" when none.
// The entries of one Slurm cluster are few and read first; the nodes are then
// found by the unique index, not by scanning.
func slurmNodeHolder(tx *gorm.DB, cluster SlurmCluster, nodeName string, now time.Time) (string, error) {
	var others []SlurmCluster
	if err := tx.Select("id", "name").Where("slurm_cluster_name = ? AND id <> ?", cluster.SlurmClusterName, cluster.ID).Find(&others).Error; err != nil {
		return "", err
	}
	for _, other := range others {
		var node Node
		err := tx.Select("state", "last_seen_at").Where(slurmNodeIs, other.ID, nodeName).Take(&node).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			continue
		}
		if err != nil {
			return "", err
		}
		if node.State != NodeStateOffline || (node.LastSeenAt != nil && now.Sub(*node.LastSeenAt) < SlurmNodeTakenFor) {
			return other.Name, nil
		}
	}
	return "", nil
}

// linkSlurmNodeToInstance ties a node that enrolled through an entry made from
// a cloud account's cluster to the instance it runs on, so the account's
// instance list shows the node as connected. A node whose instance the last
// sync did not list yet is linked by the next sync (upsertProviderInstance).
func linkSlurmNodeToInstance(tx *gorm.DB, cluster SlurmCluster, node Node) error {
	if cluster.ProviderID == nil {
		return nil
	}
	return tx.Model(&ProviderInstance{}).
		Where("provider_id = ? AND hpc_cluster_id = ? AND name = ? AND node_id = 0", *cluster.ProviderID, cluster.ProviderClusterID, HPCInstanceName(cluster.ProviderClusterID, node.SlurmNode)).
		Update("node_id", node.ID).Error
}

// slurmNodeOfInstance is the id of the node of a Slurm entry made from the
// instance's cloud cluster that runs on the instance, 0 when there is none.
func slurmNodeOfInstance(db *gorm.DB, pi *ProviderInstance) uint {
	if pi.HPCClusterID == "" {
		return 0
	}
	name, ok := HPCNodeName(pi.HPCClusterID, pi.Name)
	if !ok {
		return 0
	}
	var id uint
	// slurm_cluster_id > 0 is what lets SQLite use the node's partial unique index.
	if err := db.Raw(`SELECT n.id FROM nodes n JOIN slurm_clusters c ON c.id = n.slurm_cluster_id
		WHERE n.slurm_cluster_id > 0 AND c.provider_id = ? AND c.provider_cluster_id = ? AND n.slurm_node = ? LIMIT 1`,
		pi.ProviderID, pi.HPCClusterID, name).Scan(&id).Error; err != nil {
		return 0
	}
	return id
}
