// Package store owns all persistence: GORM entities, migrations, and the
// repository methods that write them. Per the project rules, event publication
// happens here (after the transaction commits), never via ORM hooks, and DTOs
// live in the api layer — never leak an entity past this package's methods.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

// Store wraps the database handle and the event bus.
type Store struct {
	db  *gorm.DB
	Bus *bus.Bus
	// sharing keeps SharingDeployments' rows between deployment changes.
	sharing sharingCache
	// readDB is a second connection pool (same database file) dedicated to
	// hot, pure-read paths (ListNodes, ListDeployments, ListInstances, the
	// timeline reads, InstanceByUUID — see reader()): under WAL, readers do
	// not block the writer or each other, but sharing db's single connection
	// (SetMaxOpenConns(1), the one SQLite allows a writer) forced every read
	// to queue behind whatever write transaction was in flight — at fleet
	// scale (hundreds of status-update writes/sec) that queuing was measured
	// as the dominant cost of a browser's list-page poll (scale-review.md §3).
	//
	// Nil for an in-memory database (see Open) and on a Store value built by
	// hand outside Open (a handful of test fixtures); reader() falls back to
	// db in both cases. In-memory is deliberately excluded: it is SQLite's
	// shared-cache mode, not WAL (WAL needs a file to put the log in), and
	// shared-cache's own cross-connection locking can hand a second
	// connection SQLITE_LOCKED against an in-flight writer transaction —
	// unlike SQLITE_BUSY, busy_timeout does not retry that, and this driver
	// does not implement sqlite3_unlock_notify, so it surfaces as an
	// immediate query error instead of a wait. That surfaced in practice as
	// spurious "no running instance" 503s from a real integration harness
	// (all of which run in-memory): a second pool query and the writer's
	// commit collided mid-flight and the pool's read simply failed, which
	// several read call sites (proxy.selectDeploymentInstances among them)
	// treat the same as "not found yet" because they discard the error.
	readDB    *gorm.DB
	secretbox *auth.Secretbox
	// apiKeys remembers verified gateway keys so /v1 does not pay argon2id
	// and a last_used write on every request (apikey_cache.go).
	apiKeys apiKeyCache
	// platformTokens remembers verified platform tokens and spaces out their
	// last_used_at writes (platform_tokens.go).
	platformTokens platformTokenCache
	// weightRepos caches the catalog's repository index
	// (inference_catalog_index.go).
	weightRepos weightRepoCache
	// pullCache holds the pull source rules and sync groups between writes
	// (pull_settings.go).
	pullCache pullSettingsCache
	// cacheLifecycle bridges model readiness checks and persisted instance
	// references with cache deletion on the same node.
	cacheLifecycle cacheLifecycleGuard
	// observer receives the events the scheduling activity counts
	// (scheduling_activity.go).
	observer atomic.Pointer[EventObserver]
}

func (s *Store) SetSecretbox(box *auth.Secretbox) { s.secretbox = box }

// reader returns the read-only connection pool for hot, pure-read query
// paths, falling back to the writer connection when a Store was constructed
// without one (a handful of test fixtures build &Store{db: ...} directly) or
// when the database is in-memory (see the readDB field doc).
func (s *Store) reader() *gorm.DB {
	if s.readDB != nil {
		return s.readDB
	}
	return s.db
}

// isInMemoryDSN reports whether dsn (in any form Open accepts) names an
// in-memory database rather than a real file: the empty string, literal
// ":memory:", a "mode=memory" query parameter, or the shared-cache
// ":memory:" URI Open itself normalizes "" to below. A second connection
// pool is never opened for these — see the readDB field doc for why.
func isInMemoryDSN(dsn string) bool {
	return dsn == "" || dsn == ":memory:" ||
		strings.Contains(dsn, "mode=memory") || strings.Contains(dsn, ":memory:")
}

// readOnlyDSN turns a file-backed dsn (Open only reaches this when !isInMemoryDSN,
// so dsn is a real path or an existing file: URI) into a read-only URI for the
// read pool: mode=ro is SQLite's query-parameter form for opening a connection
// that cannot write, enforced at the driver/OS file-descriptor level regardless
// of anything the SQL layer does (mode=ro requires the "file:" scheme, which a
// bare path does not have). query_only(1) and busy_timeout(5000) are carried as
// _pragma= connection-string parameters (the same form tsdb.OpenSQLite uses),
// not as a PRAGMA statement run once after Open: gorm.Exec on a pooled *sql.DB
// only reaches whichever single physical connection happens to serve that one
// call, so a pool with SetMaxOpenConns(4) would leave up to 3 connections
// without query_only actually applied to them. A DSN parameter, by contrast, is
// applied by the driver to every connection it opens for this DSN, including
// ones opened later under concurrent load.
func readOnlyDSN(dsn string) string {
	if !strings.HasPrefix(dsn, "file:") {
		dsn = "file:" + dsn
	}
	const params = "mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"
	if strings.Contains(dsn, "?") {
		return dsn + "&" + params
	}
	return dsn + "?" + params
}

// Open connects to SQLite (pure-Go driver) and runs migrations.
// dsn "" or ":memory:" opens an in-memory database (used by tests).
func Open(dsn string, b *bus.Bus) (*Store, error) {
	inMemory := isInMemoryDSN(dsn)
	if dsn == "" {
		dsn = "file::memory:?cache=shared"
	}
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger:  logger.Default.LogMode(logger.Silent),
		NowFunc: func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite: one writer. Keep a small pool and enable WAL for concurrent reads.
	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(1)
	gdb.Exec("PRAGMA journal_mode=WAL;")
	// WAL + NORMAL fsyncs at checkpoints instead of at every commit. Under WAL
	// the worst case is losing the most recent commits to an OS crash, never a
	// corrupt database - and the control plane's writes are agent reports that
	// the next reconnect replays anyway. FULL made every status report wait on
	// a disk flush, which is what let progress traffic block node heartbeats.
	// The observability stores (tsdb, logstore) already run this way.
	gdb.Exec("PRAGMA synchronous=NORMAL;")
	gdb.Exec("PRAGMA busy_timeout=5000;")

	s := &Store{db: gdb, Bus: b}
	if !inMemory {
		// Opened with mode=ro (belt) and query_only (braces, see readOnlyDSN)
		// so the read pool can never write even by accident — a bug that
		// routed a write through reader() should fail loudly at the
		// connection level, not silently succeed and let two pools drift.
		//
		// This must come after the writer above has already run its pragmas:
		// those force sqlite to actually create the database file (and,
		// under journal_mode=WAL, the -wal/-shm sidecars) on disk. Opening a
		// mode=ro connection to a path that does not exist yet fails with
		// SQLITE_CANTOPEN — mode=ro can only ever open an existing file, never
		// create one — so the writer's side effects are a hard prerequisite
		// here, not just a matter of establishing WAL first.
		readGDB, err := gorm.Open(sqlite.Open(readOnlyDSN(dsn)), &gorm.Config{
			Logger:  logger.Default.LogMode(logger.Silent),
			NowFunc: func() time.Time { return time.Now().UTC() },
		})
		if err != nil {
			return nil, fmt.Errorf("open sqlite (read pool): %w", err)
		}
		readSQLDB, err := readGDB.DB()
		if err != nil {
			return nil, err
		}
		// A handful of concurrent readers, never the writer's single
		// connection: WAL lets them run alongside an in-flight write without
		// blocking on it.
		readSQLDB.SetMaxOpenConns(4)
		s.readDB = readGDB
	}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	// The sqlite driver creates the database 0600 for the account that opens
	// it, but the control plane's install and reset commands are deliberately
	// run by the operator: the tree's access comes from its modes, not its
	// owner, so the database (and its write-ahead sidecars, which sqlite
	// creates with the same permissions) is opened up here.
	chmodDBPermissive(dsn)
	if err := s.EnsureTerminalSettings(); err != nil {
		return nil, err
	}
	if err := s.EnsureDeploymentPolicy(); err != nil {
		return nil, err
	}
	// Terminal sessions live only as long as the process that serves them, so any
	// record still marked open here belongs to a previous run and would otherwise
	// claim to be live forever.
	if _, err := s.CloseOrphanTerminalSessions(); err != nil {
		return nil, err
	}
	return s, nil
}

// chmodDBPermissive makes the on-disk database readable and writable by other
// local accounts. In-memory databases have no file to open up.
func chmodDBPermissive(dsn string) {
	path := dbFilePath(dsn)
	if path == "" {
		return
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		_ = os.Chmod(p, 0o664)
	}
}

// dbFilePath extracts the database path from the dsn forms this package
// accepts: a plain path or a file: URI (with or without a query string).
func dbFilePath(dsn string) string {
	if dsn == "" || dsn == ":memory:" || strings.Contains(dsn, "mode=memory") {
		return ""
	}
	if strings.HasPrefix(dsn, "file:") {
		rest := strings.TrimPrefix(dsn, "file:")
		rest = strings.TrimPrefix(rest, "//")
		if i := strings.IndexByte(rest, '?'); i >= 0 {
			rest = rest[:i]
		}
		return rest
	}
	return dsn
}

// DB exposes the handle for advanced/read-only queries in other packages.
func (s *Store) DB() *gorm.DB { return s.db }

// Close releases the underlying SQLite connection pool. Applications usually
// keep a Store for their entire process lifetime, while tests may create
// several in-memory stores with the same named shared-cache DSN (for example
// under go test -count). Closing the pool is what allows SQLite to discard
// that shared in-memory database between test runs.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	if s.readDB != nil {
		if readSQLDB, err := s.readDB.DB(); err == nil {
			_ = readSQLDB.Close()
		}
	}
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// ---- token helpers ----

// HashToken returns the sha256 hex of a token for storage/comparison.
func HashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

var ErrNotFound = errors.New("not found")
var ErrReferenced = errors.New("resource is referenced")

// ---- node queries ----

// NodeByUUID loads a node (without GPUs) by its UUID.
func (s *Store) NodeByUUID(uuid string) (*Node, error) {
	var n Node
	err := s.db.Where("uuid = ?", uuid).First(&n).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err == nil {
		n.HasSSHCredential, err = s.HasNodeSSHCredential(n.ID)
	}
	return &n, err
}

// NodeByTokenHash is the primary lookup used by the agent handshake.
func (s *Store) NodeByTokenHash(hash string) (*Node, error) {
	var n Node
	err := s.db.Where("token_hash = ?", strings.TrimSpace(hash)).First(&n).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &n, err
}

// ListNodes returns all nodes with their GPUs preloaded, newest first.
func (s *Store) ListNodes() ([]Node, error) {
	var nodes []Node
	err := preloadCards(s.reader()).Order("id desc").Find(&nodes).Error
	if err == nil {
		err = s.markNodeCredentialFlags(nodes)
	}
	return nodes, err
}

// ServerDialNodes returns nodes whose listening endpoint can be redialed by
// the control plane after a restart or transient connection loss.
func (s *Store) ServerDialNodes() ([]Node, error) {
	var nodes []Node
	err := s.db.Where("connect_mode = ? AND server_dial_address <> '' AND state IN (?, ?)", "server_dial", NodeStateConnecting, NodeStateOffline).Order("id").Find(&nodes).Error
	return nodes, err
}

// NodeByID loads a node with GPUs by primary key.
func (s *Store) NodeByID(id uint) (*Node, error) {
	var n Node
	err := preloadCards(s.db).First(&n, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err == nil {
		n.HasSSHCredential, err = s.HasNodeSSHCredential(n.ID)
	}
	return &n, err
}

// NodeWithGPUs loads a single node with GPUs by UUID.
func (s *Store) NodeWithGPUs(uuid string) (*Node, error) {
	var n Node
	err := preloadCards(s.db).Where("uuid = ?", uuid).First(&n).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err == nil {
		n.HasSSHCredential, err = s.HasNodeSSHCredential(n.ID)
	}
	return &n, err
}

// ---- node writes (event-publishing repository methods) ----

// NodeLabels decodes a node's JSON labels map. Empty labels are returned as an
// empty map so callers can update the result and persist it directly.
func NodeLabels(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]string{}, nil
	}
	var labels map[string]string
	if err := json.Unmarshal([]byte(raw), &labels); err != nil {
		return nil, fmt.Errorf("decode node labels: %w", err)
	}
	if labels == nil {
		labels = map[string]string{}
	}
	return labels, nil
}

// RegisterNode creates a node during bootstrap and stores the token hash.
// Returns the created node. Publishes a node.changed event after commit.
// legacyTokenPrefix marks a Token column that holds no plaintext token: rows
// registered with only a token hash.
const legacyTokenPrefix = "legacy-"

// PlaintextToken is the node's token as its agent holds it, false for a row
// registered with only a hash, whose Token column is a placeholder.
func (n Node) PlaintextToken() (string, bool) {
	token := strings.TrimSpace(n.Token)
	if token == "" || strings.HasPrefix(token, legacyTokenPrefix) {
		return "", false
	}
	return token, true
}

func (s *Store) RegisterNode(uuid, tokenHash, hostname string, connectModeOpt ...string) (*Node, error) {
	now := time.Now().UTC()
	connectMode := "agent_dial"
	if len(connectModeOpt) > 0 && connectModeOpt[0] != "" {
		connectMode = connectModeOpt[0]
	}
	n := &Node{
		UUID: uuid,
		// Legacy/provider callers only supplied a hash. Keep the new plaintext
		// column non-empty and unique for those rows; token authentication still
		// uses TokenHash until those nodes are re-enrolled.
		Token:       legacyTokenPrefix + uuid,
		TokenHash:   tokenHash,
		State:       NodeStateConnecting,
		Hostname:    hostname,
		ConnectMode: connectMode,
		LastSeenAt:  &now,
	}
	if err := s.db.Create(n).Error; err != nil {
		return nil, err
	}
	s.publishNode(n.UUID, "registered")
	return n, nil
}

// CreateNodePending creates a node before an agent has connected and returns
// the one-time generated display token. The token is also kept in plaintext by
// design so an administrator can copy it again from the management UI.
func (s *Store) CreateNodePending(name string, connectModeOpt ...string) (*Node, string, error) {
	name = strings.TrimSpace(name)
	if len(name) > 255 {
		return nil, "", errors.New("node name is too long")
	}
	connectMode := "agent_dial"
	if len(connectModeOpt) > 0 && strings.TrimSpace(connectModeOpt[0]) == "server_dial" {
		connectMode = "server_dial"
	}
	token := auth.NewToken(24)
	if token == "" {
		return nil, "", errors.New("generate node token")
	}
	n := &Node{
		UUID:        auth.NewUUID(),
		Name:        name,
		Token:       token,
		TokenHash:   HashToken(token),
		State:       NodeStateConnecting,
		ConnectMode: connectMode,
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		return tx.Create(n).Error
	}); err != nil {
		return nil, "", err
	}
	s.publishNode(n.UUID, "pending")
	return n, token, nil
}

// SetNodeAgentInfo records values observed during a successful handshake.
func (s *Store) SetNodeAgentInfo(uuid, version, remoteAddr string, connectModeOpt ...string) error {
	updates := map[string]any{
		"agent_version": strings.TrimSpace(version), "last_remote_addr": strings.TrimSpace(remoteAddr),
		"last_reject_reason": "", "last_reject_at": nil,
	}
	if len(connectModeOpt) > 0 {
		mode := "agent_dial"
		if strings.TrimSpace(connectModeOpt[0]) == "server_dial" {
			mode = "server_dial"
		}
		updates["connect_mode"] = mode
	}
	res := s.db.Model(&Node{}).Where("uuid = ?", uuid).Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	s.publishNode(uuid, "agent_info")
	return nil
}

// SetNodeRuntimePreference persists the user's runtime policy. An empty value
// is normalized to auto so older nodes can opt back into detection.
func (s *Store) SetNodeRuntimePreference(uuid, preference string) error {
	preference = strings.ToLower(strings.TrimSpace(preference))
	if preference == "" {
		preference = proto.RuntimePreferenceAuto
	}
	if !proto.ValidRuntimePreference(preference) {
		return fmt.Errorf("invalid runtime preference %q", preference)
	}
	result := s.db.Model(&Node{}).Where("uuid = ?", uuid).Update("runtime_preference", preference)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	s.publishNode(uuid, "runtime_preference")
	return nil
}

// SetNodeReject records the latest authentication/protocol rejection.
func (s *Store) SetNodeReject(uuid, reason string) error {
	now := time.Now().UTC()
	res := s.db.Model(&Node{}).Where("uuid = ?", uuid).Updates(map[string]any{
		"last_reject_reason": strings.TrimSpace(reason), "last_reject_at": now,
	})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	s.publishNode(uuid, "reject:"+strings.TrimSpace(reason))
	return nil
}

// RenameNode changes only the user-managed display name; hostname remains an
// independent observation from the agent.
func (s *Store) RenameNode(uuid, name string) (*Node, error) {
	name = strings.TrimSpace(name)
	if len(name) > 255 {
		return nil, errors.New("node name is too long")
	}
	res := s.db.Model(&Node{}).Where("uuid = ?", uuid).Update("name", name)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, ErrNotFound
	}
	s.publishNode(uuid, "name")
	return s.NodeWithGPUs(uuid)
}

// SetNodeNameIfEmpty fills the display name once from the first observed
// hostname. It intentionally does nothing after a user has named the node.
func (s *Store) SetNodeNameIfEmpty(uuid, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	res := s.db.Model(&Node{}).Where("uuid = ? AND (name = '' OR name IS NULL)", uuid).Update("name", name)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected > 0 {
		s.publishNode(uuid, "name")
	}
	return nil
}

// RegenerateNodeToken replaces the authentication token and returns the
// plaintext value for display/copying by an administrator.
func (s *Store) RegenerateNodeToken(uuid string) (*Node, string, error) {
	token := auth.NewToken(24)
	if token == "" {
		return nil, "", errors.New("generate node token")
	}
	res := s.db.Model(&Node{}).Where("uuid = ?", uuid).Updates(map[string]any{
		"token": token, "token_hash": HashToken(token),
	})
	if res.Error != nil {
		return nil, "", res.Error
	}
	if res.RowsAffected == 0 {
		return nil, "", ErrNotFound
	}
	s.publishNode(uuid, "token")
	n, err := s.NodeWithGPUs(uuid)
	return n, token, err
}

// SetNodeState updates the connection state and last-seen time.
func (s *Store) SetNodeState(uuid, state string) error {
	now := time.Now().UTC()
	res := s.db.Model(&Node{}).Where("uuid = ?", uuid).
		Updates(map[string]any{"state": state, "last_seen_at": now})
	if res.Error != nil {
		return res.Error
	}
	s.publishNode(uuid, "state:"+state)
	return nil
}

// SetNodeServerDialAddress persists the endpoint learned from a successful
// reverse connection so the server can reconnect without repeating SSH setup.
func (s *Store) SetNodeServerDialAddress(uuid, addr string) error {
	res := s.db.Model(&Node{}).Where("uuid = ?", uuid).Update("server_dial_address", addr)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	s.publishNode(uuid, "server_dial_address")
	return nil
}

func (s *Store) SetNodeServerDialListen(uuid, listen string) error {
	res := s.db.Model(&Node{}).Where("uuid = ?", uuid).Update("server_dial_listen", strings.TrimSpace(listen))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	s.publishNode(uuid, "server_dial_listen")
	return nil
}

// SetNodeInstallDir records where an SSH install put the node's agent.
func (s *Store) SetNodeInstallDir(uuid, dir string) error {
	res := s.db.Model(&Node{}).Where("uuid = ?", uuid).Update("install_dir", strings.TrimRight(strings.TrimSpace(dir), "/"))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	s.publishNode(uuid, "install_dir")
	return nil
}

// SetNodeServerURL persists the control-plane address used to enroll a node.
func (s *Store) SetNodeServerURL(uuid, serverURL string) error {
	res := s.db.Model(&Node{}).Where("uuid = ?", uuid).Update("server_url", strings.TrimRight(strings.TrimSpace(serverURL), "/"))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	s.publishNode(uuid, "server_url")
	return nil
}

// DeleteNode removes an idle node and clears records that point at it; its
// name stays behind in DeletedNode for the statistics it leaves. Runtime
// instances must be removed through the deployment/controller path first.
func (s *Store) DeleteNode(uuid string) error {
	var nodeID uint
	var removals []InstanceEvent
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var n Node
		err := tx.Where("uuid = ?", uuid).First(&n).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		// Rows only waiting to be removed from the node (a replica moved
		// away while it was offline keeps its row until the node confirms)
		// go with it; any other instance means a deployment still runs here.
		leaving := []string{proto.InstRemoving, proto.InstPendingRemoval}
		var instanceCount int64
		if err := tx.Model(&Instance{}).Where("node_id = ? AND state NOT IN ?", n.ID, leaving).Count(&instanceCount).Error; err != nil {
			return err
		}
		if instanceCount > 0 {
			return fmt.Errorf("node %s has %d instance(s); stop deployments before deleting the node", uuid, instanceCount)
		}
		var gone []Instance
		if err := tx.Where("node_id = ? AND state IN ?", n.ID, leaving).Find(&gone).Error; err != nil {
			return err
		}
		for i := range gone {
			removed, err := deleteInstanceRecords(tx, &gone[i])
			if err != nil {
				return err
			}
			removals = append(removals, removed...)
		}
		if err := checkNoCacheDeletion(tx.Where("node_id = ?", n.ID)); err != nil {
			return err
		}
		if err := deleteNodeStorageRecords(tx, n.ID); err != nil {
			return err
		}
		if err := tx.Where("node_id = ?", n.ID).Delete(&ModelFile{}).Error; err != nil {
			return err
		}
		if err := tx.Where("node_id = ?", n.ID).Delete(&ImageNode{}).Error; err != nil {
			return err
		}
		if err := tx.Where("node_id = ?", n.ID).Delete(&ProviderBootstrapToken{}).Error; err != nil {
			return err
		}
		if err := tx.Where("node_id = ?", n.ID).Delete(&NodeSSHCredential{}).Error; err != nil {
			return err
		}
		if err := tx.Model(&ProviderInstance{}).Where("node_id = ?", n.ID).Update("node_id", 0).Error; err != nil {
			return err
		}
		if err := tx.Where("node_id = ?", n.ID).Delete(&NetworkAttachment{}).Error; err != nil {
			return err
		}
		if err := tx.Where("node_id = ?", n.ID).Delete(&GPU{}).Error; err != nil {
			return err
		}
		if err := rememberDeletedNode(tx, n, time.Now().UTC()); err != nil {
			return err
		}
		if err := tx.Delete(&Node{}, n.ID).Error; err != nil {
			return err
		}
		nodeID = n.ID
		return nil
	})
	if err != nil {
		return err
	}
	for _, ev := range removals {
		s.observe(ev, nil)
	}
	s.Bus.PublishChange("node", uuid, fmt.Sprintf("deleted:%d", nodeID))
	return nil
}

// UpdateNodeStatus applies a status.node report: host summary + GPU set.
// GPUs are reconciled (upsert by node+index, delete missing) in one transaction.
func (s *Store) UpdateNodeStatus(uuid string, st proto.NodeStatus) error {
	cordoned, uncordoned := false, false
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var n Node
		if err := tx.Where("uuid = ?", uuid).First(&n).Error; err != nil {
			return err
		}
		now := time.Now().UTC()
		gpuVendor := nodeGPUVendor(st)
		gpuDriver := accelerator.NormalizeDriverVersion(gpuVendor, nodeGPUDriver(st))
		nvidiaDriver := st.Sys.NvidiaDriver
		if nvidiaDriver == "" && gpuVendor == "nvidia" {
			nvidiaDriver = gpuDriver
		}
		updates := map[string]any{
			"hostname":                  st.Sys.Hostname,
			"os":                        st.Sys.OS,
			"kernel":                    st.Sys.Kernel,
			"arch":                      st.Sys.Arch,
			"cpu_model":                 st.Sys.CPUModel,
			"cpu_cores":                 st.Sys.CPUCores,
			"mem_total_kb":              st.Sys.MemTotalKB,
			"disk_total_kb":             st.Sys.DiskTotalKB,
			"gpu_vendor":                gpuVendor,
			"gpu_driver":                gpuDriver,
			"nvidia_driver":             nvidiaDriver,
			"framework":                 st.Sys.Framework,
			"framework_version":         st.Sys.FrameworkVersion,
			"gpu_count":                 ownedCardCount(st.GPUs),
			"partition_safety_protocol": st.Sys.PartitionSafetyProtocol,
			"state":                     NodeStateOnline,
			"last_seen_at":              now,
		}
		if len(st.PathUsage) > 0 {
			if b, err := json.Marshal(st.PathUsage); err == nil {
				updates["path_usage"] = string(b)
				updates["path_usage_observed_at"] = now
			}
		}
		adopt := adoptCards(st)
		acceleratorReportUpdates(updates, st, now, adopt, n.GPUProbeFailedAt)
		if st.Sys.RuntimeDriver != "" {
			updates["runtime_driver"] = st.Sys.RuntimeDriver
		}
		updates["runtime_plugin"] = st.Sys.RuntimePlugin
		updates["runtime_gpu_blocked"] = truncate(st.Sys.RuntimeGPUBlocked, 64)
		updates["runtime_fallback_from"] = truncate(st.Sys.RuntimeFallbackFrom, 16)
		updates["runtime_fallback_reason"] = truncate(st.Sys.RuntimeFallbackReason, 64)
		updates["accelerator_vendors"] = encodeAcceleratorVendors(st.Sys.AcceleratorVendors)
		switch {
		case st.Sys.RuntimeGPUBlocked != "" && n.Scheduling == NodeSchedulable:
			// The cards are fine and nothing can be given them, so every
			// deployment placed here would fail at container start. The
			// operator is shown why and may turn scheduling back on; this
			// cordons it again while the cause remains.
			updates["scheduling"] = NodeCordoned
			cordoned = true
		case n.RuntimeGPUBlocked != "" && st.Sys.RuntimeGPUBlocked == "" && st.Sys.RuntimeDriver != "" && n.Scheduling == NodeCordoned:
			// The cause the platform cordoned for is gone: the plugin was
			// installed, or the agent moved to a runtime that needs none.
			// Who cordoned is not recorded, so a cordon an operator added
			// on top of the platform's is lifted with it: rare, and the
			// price of nodes that come back on their own.
			updates["scheduling"] = NodeSchedulable
			uncordoned = true
		}
		if len(st.Sys.Runtimes) > 0 {
			if b, err := json.Marshal(st.Sys.Runtimes); err == nil {
				updates["runtimes"] = string(b)
			}
		}
		if b, err := json.Marshal(st.Sys.PartitionAssets); err == nil {
			// Asset reports are complete snapshots. Persisting [] when an old or
			// asset-less agent reports prevents stale readiness after a restart.
			updates["partition_assets"] = string(b)
		}
		if b, err := json.Marshal(st.Sys.PartitionProbes); err == nil {
			// Like assets, probe results are a complete snapshot. A nil slice is
			// deliberately persisted as null so the server can recognize an old
			// agent instead of retaining stale readiness.
			updates["partition_probes"] = string(b)
		}
		if st.Network != nil {
			if b, err := json.Marshal(st.Network); err == nil {
				updates["network_inventory"] = string(b)
			}
		}
		if err := tx.Model(&Node{}).Where("id = ?", n.ID).Updates(updates).Error; err != nil {
			return err
		}
		if adopt != adoptNothing {
			if err := reconcileAcceleratorReport(tx, n.ID, st, adopt); err != nil {
				return err
			}
		}
		return reconcileImagePresence(tx, n.ID, st.Images)
	})
	if err != nil {
		return err
	}
	// An agent that predates storages reports only its models root; record it
	// as the default-directory local storage's state on the node. Newer agents
	// report storages on their own and leave the field empty.
	if root := strings.TrimSpace(st.Sys.ModelsRoot); root != "" {
		if node, nodeErr := s.NodeByUUID(uuid); nodeErr == nil {
			_ = s.RecordLegacyModelsRoot(node.ID, root)
		}
	}
	s.publishNode(uuid, NodeChangeStatus)
	if cordoned {
		// The platform, not an operator, took the node out of scheduling:
		// say so where an operator looks, and to every reader of
		// scheduling changes.
		slog.Warn("node cordoned: its container runtime cannot give a container its accelerators",
			"node", uuid, "reason", st.Sys.RuntimeGPUBlocked)
		s.publishNode(uuid, NodeChangeScheduling)
	}
	if uncordoned {
		slog.Info("node schedulable again: its container runtime can give a container its accelerators",
			"node", uuid, "runtime", st.Sys.RuntimeDriver)
		s.publishNode(uuid, NodeChangeScheduling)
	}
	return nil
}

// NodeChangeStatus is the change reason of a node's routine status report
// (UpdateNodeStatus), which every agent sends once a minute.
const NodeChangeStatus = "status"

func nodeGPUVendor(st proto.NodeStatus) string {
	if st.Sys.GPUVendor != "" {
		return st.Sys.GPUVendor
	}
	if st.Sys.NvidiaDriver != "" {
		return "nvidia"
	}
	for _, g := range st.GPUs {
		if g.Vendor != "" {
			return g.Vendor
		}
	}
	return ""
}

func nodeGPUDriver(st proto.NodeStatus) string {
	if st.Sys.GPUDriver != "" {
		return st.Sys.GPUDriver
	}
	if st.Sys.NvidiaDriver != "" {
		return st.Sys.NvidiaDriver
	}
	for _, g := range st.GPUs {
		if g.DriverVersion != "" {
			return g.DriverVersion
		}
	}
	return ""
}

// publishNode tells the bus this node changed, naming what changed it.
func (s *Store) publishNode(uuid, reason string) {
	if s.Bus == nil {
		return
	}
	s.Bus.Publish(bus.Event{Topic: "node", Key: uuid, Reason: reason})
}

// MarkStaleOffline flips nodes whose last-seen is older than the cutoff to
// offline. Used as a safety net; the hub also flips state on disconnect.
func (s *Store) MarkStaleOffline(ctx context.Context, olderThan time.Duration) error {
	cutoff := time.Now().UTC().Add(-olderThan)
	var nodes []Node
	if err := s.db.WithContext(ctx).Select("uuid").
		Where("state = ? AND (last_seen_at IS NULL OR last_seen_at < ?)", NodeStateOnline, cutoff).
		Find(&nodes).Error; err != nil {
		return err
	}
	for _, node := range nodes {
		if err := s.SetNodeState(node.UUID, NodeStateOffline); err != nil {
			return err
		}
	}
	return nil
}
