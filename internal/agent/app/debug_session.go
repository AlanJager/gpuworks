package app

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"time"

	"gpuworks.ai/gpuworks/internal/debuglog"
	"gpuworks.ai/gpuworks/internal/proto"
)

// debugSessionKeep is how long a finished session's file stays on the node
// for a diagnostics collection to fetch.
const debugSessionKeep = 7 * 24 * time.Hour

// debugSessionMaxBytes caps one node's session file whatever the server asks.
const debugSessionMaxBytes = 200 << 20

var debugSessionID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// agentLogDir is where agent.log and the debug session files are.
func (n *NodeRuntime) agentLogDir() string {
	if n.logDir != "" {
		return n.logDir
	}
	return n.stateDir
}

// debugSessionDir holds the agent's session files.
func (n *NodeRuntime) debugSessionDir() string { return filepath.Join(n.agentLogDir(), "debug") }

func (n *NodeRuntime) rpcDebugStart(raw json.RawMessage) (any, error) {
	if n.debugLog == nil {
		return nil, errors.New("this agent cannot open a debug session")
	}
	var p proto.DebugStartParams
	if err := json.Unmarshal(raw, &p); err != nil || !debugSessionID.MatchString(p.SessionID) || p.UntilMS <= 0 || p.MaxBytes <= 0 {
		return nil, errors.New("debug.start requires session_id, until_ms and max_bytes")
	}
	until := time.UnixMilli(p.UntilMS)
	if !until.After(time.Now()) {
		return nil, errors.New("debug session deadline has passed")
	}
	limit := min(p.MaxBytes, debugSessionMaxBytes)
	dir := n.debugSessionDir()
	debuglog.Sweep(dir, debugSessionKeep, time.Now())
	path := filepath.Join(dir, p.SessionID+".log")
	if err := n.debugLog.Start(p.SessionID, path, until, limit); err != nil {
		return nil, err
	}
	n.log.Warn("debug session started", "session", p.SessionID, "until", until.UTC().Format(time.RFC3339), "max_bytes", limit)
	return n.debugSessionResult(), nil
}

func (n *NodeRuntime) rpcDebugStop(raw json.RawMessage) (any, error) {
	if n.debugLog == nil {
		return nil, errors.New("this agent cannot open a debug session")
	}
	var p proto.DebugStopParams
	if err := json.Unmarshal(raw, &p); err != nil || p.SessionID == "" {
		return nil, errors.New("debug.stop requires session_id")
	}
	if n.debugLog.Stop(p.SessionID) {
		n.log.Warn("debug session stopped", "session", p.SessionID)
	}
	return n.debugSessionResult(), nil
}

func (n *NodeRuntime) debugSessionResult() proto.DebugSessionResult {
	st, _ := n.debugLog.Status()
	return proto.DebugSessionResult{SessionID: st.ID, Path: st.Path, Open: st.Open, WrittenBytes: st.Written, Ended: st.Ended}
}
