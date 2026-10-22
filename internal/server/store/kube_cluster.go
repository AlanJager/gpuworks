package store

import (
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
	"gpuworks.ai/gpuworks/internal/server/auth"
)

// A Kubernetes cluster joins as one DaemonSet: every GPU node it selects runs
// an agent that trades the cluster's enrollment token for a node token of its
// own (EnrollKubeNode). After that each one is an ordinary node; the cluster
// row only answers "which cluster is this node in", renders the manifest
// again, and stops new nodes joining when revoked.

// Kube GPU return policy (KubeCluster.IdleReturnSeconds).
const (
	// KubeKeepCards keeps every card the platform has borrowed: the node's
	// cards stay the platform's for as long as it is enrolled.
	KubeKeepCards = -1
	// KubeDefaultIdleReturnSeconds returns a card a minute after the last
	// instance on it stopped: long enough for a restart or redeploy to find it
	// again, short enough that the cluster's own pods get it back quickly.
	KubeDefaultIdleReturnSeconds = 60
)

// KubeCluster is a Kubernetes cluster whose GPU nodes join through one
// DaemonSet. The fields below the token are the manifest's parameters,
// stored so the same manifest can be downloaded again.
type KubeCluster struct {
	ID   uint   `gorm:"primaryKey"`
	UUID string `gorm:"uniqueIndex;size:64"`
	Name string `gorm:"uniqueIndex;size:128"`
	// Token is the enrollment token, kept in plaintext like Node.Token so the
	// manifest that carries it can be rendered again; TokenHash is the index
	// enrollment looks it up by.
	Token     string `gorm:"size:128"`
	TokenHash string `gorm:"uniqueIndex;size:128"`
	// ClusterUID is the UID of the cluster's kube-system namespace, recorded
	// by the first node that enrolls. From then on the token is refused to
	// nodes of any other cluster.
	ClusterUID string `gorm:"size:64"`
	// Namespace the agents and instance pods run in.
	Namespace string `gorm:"size:63"`
	// ServerURL is the address the agents connect to (ADR 0025: stated by the
	// operator, never inferred).
	ServerURL string `gorm:"size:512"`
	// AgentImage is the image the DaemonSet runs.
	AgentImage string `gorm:"size:512"`
	// Vendor is the accelerator vendor of the selected nodes; it decides the
	// host paths the agents mount. A cluster with cards of two vendors joins
	// as two clusters, each selecting its own nodes.
	Vendor string `gorm:"size:16"`
	// NodeSelector is the DaemonSet's node selector, "key=value" pairs joined
	// by commas; empty selects every node.
	NodeSelector string `gorm:"size:512"`
	// RuntimeClass names the RuntimeClass that injects NVIDIA devices; empty
	// lets each agent use the cluster's "nvidia" RuntimeClass when there is
	// one and the node's default runtime otherwise (managed clusters make
	// NVIDIA's runtime the default and define no class).
	RuntimeClass string `gorm:"size:63"`
	// ImagePullSecret names a docker-registry Secret in Namespace that the
	// agent pods (and the hold pods they start, from the agent image) pull
	// with; empty pulls without credentials.
	ImagePullSecret string `gorm:"size:253"`
	// ContainerdSocket overrides the containerd socket the agents find on
	// their own; empty lets them look.
	ContainerdSocket string `gorm:"size:255"`
	// StateDir is the agents' directory on the host; empty is /opt/gpuworks.
	StateDir string `gorm:"size:255"`
	// APIServer is the API server address the agents use instead of the
	// "kubernetes" Service ("https://host:port"); empty uses the Service.
	APIServer string `gorm:"size:512"`
	// EnginePorts is the engine port range the agents use, "START-END".
	EnginePorts string `gorm:"size:32"`
	// ProviderID and ProviderClusterID name the managed cluster this entry was
	// made from (a cloud account's cluster, provider.ClusterProvider); the
	// server applies the manifest there with a kubeconfig the cloud issues
	// for that one operation. Both are empty for an entry made by hand, and
	// ProviderID is cleared when the account is deleted.
	ProviderID        *uint  `gorm:"index:idx_kube_cluster_source,priority:1"`
	ProviderClusterID string `gorm:"size:128;index:idx_kube_cluster_source,priority:2"`
	// IdleReturnSeconds is how long a borrowed card stays the platform's after
	// the last instance on it stopped; KubeKeepCards keeps it.
	IdleReturnSeconds int
	RevokedAt         *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Errors EnrollKubeNode returns for a token it will not honour.
var (
	ErrKubeTokenUnknown    = errors.New("unknown or revoked enrollment token")
	ErrKubeClusterMismatch = errors.New("the enrollment token belongs to another cluster")
)

func createKubeClusters(db *gorm.DB) error {
	if err := db.AutoMigrate(&KubeCluster{}); err != nil {
		return err
	}
	if !db.Migrator().HasTable(&Node{}) {
		return nil
	}
	for _, col := range []string{"KubeClusterID", "KubeNodeName", "KubeUnschedulable"} {
		if !db.Migrator().HasColumn(&Node{}, col) {
			if err := db.Migrator().AddColumn(&Node{}, col); err != nil {
				return err
			}
		}
	}
	if !db.Migrator().HasIndex(&Node{}, "idx_node_kube") {
		return db.Migrator().CreateIndex(&Node{}, "idx_node_kube")
	}
	return nil
}

// addKubeClusterImagePullSecret adds KubeCluster.ImagePullSecret.
func addKubeClusterImagePullSecret(db *gorm.DB) error {
	if !db.Migrator().HasTable(&KubeCluster{}) || db.Migrator().HasColumn(&KubeCluster{}, "ImagePullSecret") {
		return nil
	}
	return db.Migrator().AddColumn(&KubeCluster{}, "ImagePullSecret")
}

// CreateKubeCluster stores a cluster with a fresh enrollment token.
func (s *Store) CreateKubeCluster(c KubeCluster) (*KubeCluster, error) {
	c.ID = 0
	c.UUID = auth.NewUUID()
	c.Token = auth.NewToken(24)
	if c.Token == "" {
		return nil, errors.New("generate enrollment token")
	}
	c.TokenHash = HashToken(c.Token)
	c.ClusterUID = ""
	c.RevokedAt = nil
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if c.ProviderID != nil {
			var n int64
			if err := tx.Model(&KubeCluster{}).Where("provider_id = ? AND provider_cluster_id = ?", *c.ProviderID, c.ProviderClusterID).Count(&n).Error; err != nil {
				return err
			}
			if n > 0 {
				return ErrKubeClusterSourceTaken
			}
		}
		return tx.Create(&c).Error
	})
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// KubeClusters lists every cluster, oldest first.
func (s *Store) KubeClusters() ([]KubeCluster, error) {
	var rows []KubeCluster
	err := s.db.Order("id").Find(&rows).Error
	return rows, err
}

// KubeClusterByUUID returns one cluster or ErrNotFound.
func (s *Store) KubeClusterByUUID(uuid string) (*KubeCluster, error) {
	var c KubeCluster
	err := s.db.Where("uuid = ?", uuid).First(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &c, err
}

// KubeClusterByToken returns the live cluster an enrollment token belongs to.
func (s *Store) KubeClusterByToken(token string) (*KubeCluster, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, ErrKubeTokenUnknown
	}
	var c KubeCluster
	err := s.db.Where("token_hash = ? AND revoked_at IS NULL", HashToken(token)).First(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrKubeTokenUnknown
	}
	return &c, err
}

// KubeClusterNodeCounts maps cluster ID to how many nodes joined from it.
func (s *Store) KubeClusterNodeCounts() (map[uint]int, error) {
	var rows []struct {
		KubeClusterID uint
		N             int
	}
	err := s.db.Model(&Node{}).Select("kube_cluster_id, count(*) AS n").
		Where("kube_cluster_id > 0").Group("kube_cluster_id").Scan(&rows).Error
	out := make(map[uint]int, len(rows))
	for _, r := range rows {
		out[r.KubeClusterID] = r.N
	}
	return out, err
}

// RevokeKubeCluster stops new nodes joining with the cluster's token. Nodes
// that already joined keep their own tokens.
func (s *Store) RevokeKubeCluster(uuid string) error {
	now := time.Now().UTC()
	res := s.db.Model(&KubeCluster{}).Where("uuid = ? AND revoked_at IS NULL", uuid).Update("revoked_at", &now)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		if _, err := s.KubeClusterByUUID(uuid); err != nil {
			return err
		}
	}
	return nil
}

// ErrKubeClusterHasNodes refuses deleting a cluster whose nodes are still
// registered: they would be left pointing at nothing.
var ErrKubeClusterHasNodes = errors.New("the cluster still has nodes")

// DeleteKubeCluster removes a cluster none of whose nodes remain.
func (s *Store) DeleteKubeCluster(uuid string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		var c KubeCluster
		if err := tx.Where("uuid = ?", uuid).First(&c).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		var n int64
		if err := tx.Model(&Node{}).Where("kube_cluster_id = ?", c.ID).Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return ErrKubeClusterHasNodes
		}
		return tx.Delete(&c).Error
	})
}

// EnrollKubeNode trades a cluster's enrollment token for the node token of
// the Kubernetes node nodeName. It is idempotent: the same node of the same
// cluster always gets the same platform node back, so a recreated agent pod
// or a reinstalled host keeps its labels and model files. The first
// enrollment binds the token to clusterUID; any other cluster is refused.
func (s *Store) EnrollKubeNode(token, clusterUID, nodeName string) (*Node, *KubeCluster, error) {
	clusterUID, nodeName = strings.TrimSpace(clusterUID), strings.TrimSpace(nodeName)
	if clusterUID == "" || nodeName == "" || len(nodeName) > 253 || len(clusterUID) > 64 {
		return nil, nil, errors.New("cluster_uid and node_name are required")
	}
	var node Node
	var cluster KubeCluster
	created := false
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("token_hash = ? AND revoked_at IS NULL", HashToken(strings.TrimSpace(token))).First(&cluster).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrKubeTokenUnknown
			}
			return err
		}
		switch {
		case cluster.ClusterUID == "":
			cluster.ClusterUID = clusterUID
			if err := tx.Model(&cluster).Update("cluster_uid", clusterUID).Error; err != nil {
				return err
			}
		case cluster.ClusterUID != clusterUID:
			return ErrKubeClusterMismatch
		}
		err := tx.Where("kube_cluster_id = ? AND kube_node_name = ?", cluster.ID, nodeName).First(&node).Error
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
			KubeClusterID: cluster.ID, KubeNodeName: nodeName,
		}
		created = true
		return tx.Create(&node).Error
	})
	if err != nil {
		return nil, nil, err
	}
	if created {
		s.publishNode(node.UUID, "pending")
	}
	return &node, &cluster, nil
}

// OfflineKubeNodes lists the Kubernetes nodes whose agents have been gone
// since before cutoff: the candidates for having left their cluster. Offline
// nodes are few, and the state column is indexed.
func (s *Store) OfflineKubeNodes(cutoff time.Time) ([]Node, error) {
	var nodes []Node
	err := s.reader().Where("state = ? AND kube_cluster_id > 0 AND (last_seen_at IS NULL OR last_seen_at < ?)", NodeStateOffline, cutoff).
		Order("id").Find(&nodes).Error
	return nodes, err
}

// OnlineKubePeers lists the online nodes of a cluster other than exclude.
func (s *Store) OnlineKubePeers(clusterID, exclude uint) ([]Node, error) {
	var nodes []Node
	err := s.reader().Where("kube_cluster_id = ? AND id <> ? AND state = ?", clusterID, exclude, NodeStateOnline).
		Order("id").Limit(3).Find(&nodes).Error
	return nodes, err
}
