package dto

// ServerListen is the control plane's listen configuration as server.yaml
// stores it, with what the running process actually bound. Agent and gateway
// are empty when they inherit console.
type ServerListen struct {
	Console []string `json:"console"`
	Agent   []string `json:"agent"`
	Gateway []string `json:"gateway"`
	// Running lists the sockets open now; it differs from the saved addresses
	// until the server restarts.
	Running        []ServerBinding `json:"running"`
	RestartPending bool            `json:"restart_pending"`
	ConfigPath     string          `json:"config_path"`
	// Editable is false when the server was started without server.yaml or
	// when --addr overrides it (Overridden).
	Editable   bool `json:"editable"`
	Overridden bool `json:"overridden"`
	// RestartMode is "self" when the service manager brings the server back
	// after it exits, and "command" when the operator must run RestartCommand.
	RestartMode    string `json:"restart_mode"`
	RestartCommand string `json:"restart_command,omitempty"`
}

// ServerBinding is one open socket and the functions it serves.
type ServerBinding struct {
	Addr      string   `json:"addr"`
	Functions []string `json:"functions"`
}

// ServerListenUpdate replaces the saved listen configuration.
type ServerListenUpdate struct {
	Console []string `json:"console"`
	Agent   []string `json:"agent"`
	Gateway []string `json:"gateway"`
}
