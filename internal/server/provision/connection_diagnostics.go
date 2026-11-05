package provision

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/commandlog"
)

// ConnectionCheck is a single observation, never proof of a complete session.
type ConnectionCheck struct {
	Stage      string
	Outcome    string
	Target     string
	Source     string
	ObservedAt time.Time
	HTTPStatus int
}

// ConnectionDiagnostic contains transient SSH and node-vantage evidence only.
type ConnectionDiagnostic struct {
	SSH           ConnectionCheck
	Configuration ConnectionCheck
	Health        ConnectionCheck
}

// DiagnoseConnection reads the named installation and probes its saved URL.
// It never calls the recovery/restart/install paths or changes stored settings.
func (i *SSHInstaller) DiagnoseConnection(ctx context.Context, req SSHProbeRequest) ConnectionDiagnostic {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out := ConnectionDiagnostic{
		SSH:           ConnectionCheck{Stage: "ssh", Outcome: "unavailable"},
		Configuration: ConnectionCheck{Stage: "configuration", Outcome: "not_checked"},
		Health:        ConnectionCheck{Stage: "node_http", Outcome: "not_checked"},
	}
	if req.ConnectMode == "server_dial" {
		out.Health.Outcome = "not_applicable"
	} else if agentconf.ValidateServer(req.ServerURL) != nil {
		out.Health.Outcome = "invalid_address"
		return out
	}
	if strings.TrimSpace(req.Token) == "" || strings.ContainsAny(req.Token, "\r\n") {
		out.Configuration.Outcome = "unverified_identity"
		return out
	}
	normalized, err := NormalizeSSHProbeRequest(req)
	if err != nil {
		return out
	}
	client, _, err := DialSSH(ctx, normalized.User, normalized.Host, normalized.Port, normalized.SSHAuth, i.timeout(), i.HostKeys)
	if err != nil {
		out.SSH.ObservedAt = time.Now().UTC()
		out.SSH.Outcome = diagnosticContextOutcome(ctx, "ssh_failed")
		return out
	}
	defer client.Close()
	out.SSH.Outcome, out.SSH.ObservedAt = "connected", time.Now().UTC()
	command := connectionDiagnosticCommand(installDir(req.InstallDir), req.ServerURL, req.ConnectMode)
	raw, err := runDiagnosticWithInput(ctx, client, command, req.Token)
	observed := time.Now().UTC()
	if err != nil {
		out.Configuration = ConnectionCheck{Stage: "configuration", Outcome: diagnosticContextOutcome(ctx, "command_failed"), ObservedAt: observed}
		return out
	}
	configuration, health, ok := parseConnectionDiagnostic(raw)
	if !ok {
		out.Configuration = ConnectionCheck{Stage: "configuration", Outcome: "invalid_response", ObservedAt: observed}
		return out
	}
	out.Configuration = configuration
	out.Configuration.ObservedAt = observed
	if req.ConnectMode != "server_dial" && configuration.Outcome == "snapshot" && configuration.Target == "" {
		out.Configuration.Outcome = "invalid_address"
	}
	if out.Health.Outcome != "not_applicable" {
		out.Health = health
		if health.Outcome != "not_checked" {
			out.Health.ObservedAt = observed
			out.Health.Target = strings.TrimRight(req.ServerURL, "/") + "/healthz"
		}
	}
	return out
}

func diagnosticContextOutcome(ctx context.Context, fallback string) string {
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		return "cancelled"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "timeout"
	default:
		return fallback
	}
}

// The remote protocol contains only an identity-gated config snapshot and
// numeric curl outcomes. Neither response bodies nor stderr are returned.
func connectionDiagnosticCommand(dir, server, mode string) string {
	script := `set +x
exec 2>/dev/null
IFS= read -r expected || exit 0
` + agentconf.ShellFuncs + fmt.Sprintf(`
dir=%s
source=unavailable
address=
if [ -e "$dir/gpuworks-agent.conf" ]; then
  if [ -r "$dir/gpuworks-agent.conf" ]; then
    if [ "$(gw_conf "$dir" TOKEN)" = "$expected" ]; then
      source=state_file
      address=$(gw_conf "$dir" SERVER)
    else source=unverified_identity; fi
  fi
else
  unit=$(gw_unit_start)
  if [ "$(gw_line_value "$unit" TOKEN)" = "$expected" ]; then
    source=legacy_arguments
    address=$(gw_line_value "$unit" SERVER)
  else
    processes=$(ps axww -o args= || ps axww)
    found=$(printf '%%s\n' "$processes" | while IFS= read -r line; do
      case "$line" in *"$dir/gpuworks-agent run "*) ;; *) continue ;; esac
      state=$(printf '%%s\n' "$line" | gw_arg state)
      [ "$state" = "$dir" ] || continue
      [ "$(gw_line_value "$line" TOKEN)" = "$expected" ] || continue
      printf 'found %%s\n' "$(gw_line_value "$line" SERVER)"
      break
    done)
    case "$found" in 'found '*) source=legacy_arguments; address=${found#found } ;; esac
  fi
fi
printf 'GW_CONFIG %%s\n' "$source"
case "$address" in
  *'@'*|*'?'*|*'#'*|*' '*|*'\'*|'') address= ;;
  http://*|https://*) ;;
  *) address= ;;
esac
case "$address" in *'
'*) address= ;; esac
printf 'GW_ADDRESS %%s\n' "$address"
if [ "$source" != state_file ] && [ "$source" != legacy_arguments ]; then
  echo 'GW_HEALTH not_checked'
elif [ %s = server_dial ]; then
  echo 'GW_HEALTH not_applicable'
elif ! command -v curl >/dev/null 2>&1; then
  echo 'GW_HEALTH tool_missing'
else
  status=$(curl --silent --output /dev/null --write-out '%%{http_code}' --connect-timeout 3 --max-time 5 --max-redirs 0 --proto '=http,https' -- %s)
  code=$?
  printf 'GW_HEALTH %%s %%s\n' "$code" "$status"
fi
`, shellQuote(dir), shellQuote(mode), shellQuote(strings.TrimRight(server, "/")+"/healthz"))
	return "sh -c " + shellQuote(script)
}

func parseConnectionDiagnostic(raw string) (ConnectionCheck, ConnectionCheck, bool) {
	config := ConnectionCheck{Stage: "configuration"}
	health := ConnectionCheck{Stage: "node_http", Outcome: "not_checked"}
	if len(raw) > connectionDiagnosticLimit || strings.Contains(raw, "\x00") {
		return config, health, false
	}
	lines := strings.Split(strings.TrimSuffix(raw, "\n"), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "GW_CONFIG ") || !strings.HasPrefix(lines[1], "GW_ADDRESS ") || !strings.HasPrefix(lines[2], "GW_HEALTH ") {
		return config, health, false
	}
	source := strings.TrimPrefix(lines[0], "GW_CONFIG ")
	switch source {
	case "state_file", "legacy_arguments":
		config.Outcome, config.Source = "snapshot", source
	case "unavailable", "unverified_identity":
		config.Outcome = source
	default:
		return config, health, false
	}
	address := strings.TrimPrefix(lines[1], "GW_ADDRESS ")
	if address != "" {
		if agentconf.ValidateServer(address) != nil || len(address) > 2048 {
			config.Outcome = "invalid_address"
		} else {
			config.Target = address
		}
	}
	fields := strings.Fields(strings.TrimPrefix(lines[2], "GW_HEALTH "))
	if len(fields) == 1 {
		switch fields[0] {
		case "not_checked", "not_applicable", "tool_missing":
			health.Outcome = fields[0]
			return config, health, true
		}
	}
	if len(fields) != 2 {
		return config, health, false
	}
	code, err := strconv.Atoi(fields[0])
	status, statusErr := strconv.Atoi(fields[1])
	if err != nil || statusErr != nil || code < 0 || code > 255 || status < 0 || status > 599 {
		return config, health, false
	}
	health.HTTPStatus = status
	switch code {
	case 0:
		health.Outcome = "http_error"
		if status == 200 {
			health.Outcome = "http_ok"
		}
	case 5, 6:
		health.Outcome = "dns"
	case 7:
		health.Outcome = "connect"
	case 28:
		health.Outcome = "timeout"
	case 35, 51, 58, 60, 77, 80, 82, 83, 90, 91:
		health.Outcome = "tls"
	default:
		health.Outcome = "transport"
	}
	return config, health, true
}

// ConnectionDiagnosticURL validates display/probe addresses without echoing an
// invalid value: validation errors may themselves contain embedded credentials.
func ConnectionDiagnosticURL(value string) string {
	if len(value) > 2048 || agentconf.ValidateServer(value) != nil {
		return ""
	}
	return value
}

// ConnectionDiagnosticDetail bounds legacy recovery text at the diagnostic boundary.
func ConnectionDiagnosticDetail(detail, token string) string {
	result := commandlog.RedactScript(detail)
	if token != "" {
		result = strings.ReplaceAll(result, token, "<redacted>")
	}
	if len(result) > 1024 {
		return "recovery detail is available in the existing recovery report"
	}
	return result
}
