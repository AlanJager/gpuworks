package store

import (
	"encoding/json"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// UsageFacts is what the daily usage report reads from the database: the
// fleet and its deployments as they are now, and what was started during the
// report's day. It is read once a day; every read is one query over a table
// the platform keeps small (nodes, cards, deployments, model files,
// benchmark runs), except the timeline, which is read by its created_at
// index for the day only.
type UsageFacts struct {
	Nodes       []UsageNode
	Cards       []UsageCards
	Providers   []NameCount
	Storages    []NameCount
	Deployments []UsageDeployment
	// PDDeployments counts deployments whose instances carry a PD role.
	PDDeployments  int
	InstanceStates []NameCount
	// TokenHosts are the model hubs (huggingface.co, modelscope.cn) the
	// platform holds a credential for.
	TokenHosts []string
	// Events counts the window's timeline events per code.
	Events []NameCount
	// Lifecycle is every instance creation, and every state an instance
	// reached, from the window's start until the report is read.
	Lifecycle        []UsageInstanceEvent
	ModelFiles       []UsageModelFile
	ModelFilesFailed int
	ImagesFailed     int
	Benchmarks       []UsageBenchmark
	SchemaVersion    int
}

// UsageNode is one node's makeup.
type UsageNode struct {
	AgentVersion     string
	OS               string
	Kernel           string
	Arch             string
	GPUVendor        string
	GPUDriver        string
	Framework        string
	FrameworkVersion string
	RuntimeDriver    string
	ConnectMode      string
	State            string
	// SSH is set for a node installed over SSH; Cloud for one linked to a
	// cloud provider's instance.
	SSH   bool
	Cloud bool
}

// UsageCards counts cards of one model, ownership and health code.
type UsageCards struct {
	Name       string
	Ownership  string
	HealthCode string
	Count      int
}

// UsageDeployment is one deployment's features.
type UsageDeployment struct {
	ID               uint
	Engine           string
	State            string
	NNodes           int
	Sleepable        bool
	PartitionBackend string
	TemplateSource   string
	// TemplateModel is the template's model ID when it has one.
	TemplateModel string
	ModelSource   string
	ModelSourceID string
	CreatedAt     time.Time
}

// UsageInstanceEvent is one instance's creation (Created) or a state it
// reached.
type UsageInstanceEvent struct {
	InstanceUUID string
	Created      bool
	State        string
	At           time.Time
}

// UsageModelFile is a model file created in the window.
type UsageModelFile struct {
	Acquisition string
	State       string
	SizeBytes   int64
}

// UsageBenchmark is a benchmark run created in the window. DatasetKind is
// the dataset's kind (random, multiturn, agentic, imported).
type UsageBenchmark struct {
	Mode        string
	DatasetKind string
	State       string
}

// usageLifecycleCodes are the timeline codes that say an instance was
// created or reached a state.
var usageLifecycleCodes = []string{instanceEventCreated, instanceEventStateChanged, instanceEventStatusUpdated}

// UsageFacts reads the facts for a report covering [since, until), with
// instance outcomes followed until now.
func (s *Store) UsageFacts(since, until, now time.Time) (UsageFacts, error) {
	var out UsageFacts
	db := s.reader()
	if err := db.Raw(`SELECT n.agent_version, n.os, n.kernel, n.arch, n.gpu_vendor, n.gpu_driver, n.framework,
		n.framework_version, n.runtime_driver, n.connect_mode, n.state,
		(n.install_dir <> '' AND EXISTS (SELECT 1 FROM node_ssh_credential c WHERE c.node_id = n.id)) AS ssh,
		EXISTS (SELECT 1 FROM provider_instances p WHERE p.node_id = n.id) AS cloud
		FROM nodes n ORDER BY n.id`).Scan(&out.Nodes).Error; err != nil {
		return UsageFacts{}, err
	}
	if err := db.Raw(`SELECT name, ownership, health_code, COUNT(*) AS count FROM gpus
		GROUP BY name, ownership, health_code`).Scan(&out.Cards).Error; err != nil {
		return UsageFacts{}, err
	}
	if err := db.Raw(`SELECT kind AS name, COUNT(*) AS count FROM providers GROUP BY kind`).Scan(&out.Providers).Error; err != nil {
		return UsageFacts{}, err
	}
	if err := db.Raw(`SELECT type AS name, COUNT(*) AS count FROM storages GROUP BY type`).Scan(&out.Storages).Error; err != nil {
		return UsageFacts{}, err
	}
	if err := db.Raw(`SELECT d.id, d.engine, d.desired_state AS state, d.n_nodes, d.sleepable, d.partition_backend,
		d.template_spec_source AS template_source, COALESCE(im.model_id, '') AS template_model,
		COALESCE(m.source, '') AS model_source, COALESCE(m.source_id, '') AS model_source_id, d.created_at
		FROM deployments d
		LEFT JOIN inference_models im ON im.id = d.template_id
		LEFT JOIN models m ON m.id = d.model_id`).Scan(&out.Deployments).Error; err != nil {
		return UsageFacts{}, err
	}
	if err := db.Raw(`SELECT COUNT(DISTINCT deployment_id) FROM instances WHERE role <> ''`).Scan(&out.PDDeployments).Error; err != nil {
		return UsageFacts{}, err
	}
	if err := db.Raw(`SELECT state AS name, COUNT(*) AS count FROM instances GROUP BY state`).Scan(&out.InstanceStates).Error; err != nil {
		return UsageFacts{}, err
	}
	if err := db.Raw(`SELECT host FROM source_credentials WHERE host IN ?`, []string{"huggingface.co", "modelscope.cn"}).
		Scan(&out.TokenHosts).Error; err != nil {
		return UsageFacts{}, err
	}
	if err := db.Raw(`SELECT code AS name, COUNT(*) AS count FROM instance_events
		WHERE created_at >= ? AND created_at < ? GROUP BY code`, since, until).Scan(&out.Events).Error; err != nil {
		return UsageFacts{}, err
	}
	lifecycle, err := s.usageLifecycle(since, now)
	if err != nil {
		return UsageFacts{}, err
	}
	out.Lifecycle = lifecycle
	if err := db.Raw(`SELECT acquisition, state, size_bytes FROM model_files
		WHERE created_at >= ? AND created_at < ?`, since, until).Scan(&out.ModelFiles).Error; err != nil {
		return UsageFacts{}, err
	}
	if err := db.Raw(`SELECT COUNT(*) FROM model_files WHERE state = ?`, proto.MFFailed).Scan(&out.ModelFilesFailed).Error; err != nil {
		return UsageFacts{}, err
	}
	if err := db.Raw(`SELECT COUNT(*) FROM image_nodes WHERE state = ?`, ImageStateFailed).Scan(&out.ImagesFailed).Error; err != nil {
		return UsageFacts{}, err
	}
	var runs []struct {
		Mode    string
		Dataset string
		State   string
	}
	if err := db.Raw(`SELECT mode, dataset, state FROM benchmark_runs
		WHERE created_at >= ? AND created_at < ?`, since, until).Scan(&runs).Error; err != nil {
		return UsageFacts{}, err
	}
	for _, r := range runs {
		var dataset struct {
			Kind string `json:"kind"`
		}
		_ = json.Unmarshal([]byte(r.Dataset), &dataset)
		out.Benchmarks = append(out.Benchmarks, UsageBenchmark{Mode: r.Mode, DatasetKind: dataset.Kind, State: r.State})
	}
	if out.SchemaVersion, err = s.CurrentMigration(); err != nil {
		return UsageFacts{}, err
	}
	return out, nil
}

// usageLifecycle reads instance creations and reached states from the
// timeline by its created_at index, decoding only the state each carries.
func (s *Store) usageLifecycle(since, until time.Time) ([]UsageInstanceEvent, error) {
	rows, err := s.reader().Raw(`SELECT instance_uuid, code, data, created_at FROM instance_events
		WHERE created_at >= ? AND created_at < ? AND code IN ?`, since, until, usageLifecycleCodes).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageInstanceEvent
	for rows.Next() {
		var uuid, code, data string
		var at time.Time
		if err := rows.Scan(&uuid, &code, &data, &at); err != nil {
			return nil, err
		}
		var params struct {
			State string `json:"state"`
		}
		_ = json.Unmarshal([]byte(data), &params)
		out = append(out, UsageInstanceEvent{InstanceUUID: uuid, Created: code == instanceEventCreated, State: params.State, At: at.UTC()})
	}
	return out, rows.Err()
}
