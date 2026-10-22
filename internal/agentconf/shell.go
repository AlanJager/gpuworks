package agentconf

import "strings"

// ShellFuncs are the shell functions every script uses to learn what an
// agent on a host is configured with, so that no script parses an agent's
// command line its own way. Values come from the file in the state
// directory the command line names; an agent installed before the file
// existed carries them as arguments instead, and the functions read those.
// They print nothing for a value that cannot be read (a file another user
// owns) and never print a command line, which may hold a token.
//
//	gw_conf DIR KEY        KEY from DIR's configuration file
//	gw_line_value LINE KEY KEY of the agent started by command line LINE
//	gw_unit_value KEY      KEY of the agent the gpuworks-agent unit runs
//	gw_unit_state          the unit agent's state directory
//
// KEY is a file key: SERVER, TOKEN, CONNECT_MODE, LISTEN. Variables they set
// start with _gw_.
var ShellFuncs = strings.NewReplacer("@FILE@", FileName).Replace(`gw_conf() {
  [ -r "$1/@FILE@" ] || return 0
  sed -n "s/^$2=//p" "$1/@FILE@" | tail -n 1
}
gw_arg() {
  if [ "$1" = state ]; then
    # ps flattens argv without quoting spaces; systemctl appends unit fields.
    # Keep the state value up to the next option or systemctl field.
    sed -n 's/.*--state[ =]\(.*\)/\1/p' |
      sed 's/ --[a-zA-Z][a-zA-Z0-9_-]*[ =].*//; s/ ; [a-zA-Z_][a-zA-Z0-9_]*=.*//; s/ ; }$//' | head -n 1
    return
  fi
  sed -n "s/.*--$1[ =]\([^ ;]*\).*/\1/p" | head -n 1
}
gw_line_value() {
  _gw_state=$(printf '%s\n' "$1" | gw_arg state)
  if [ -n "$_gw_state" ] && [ -e "$_gw_state/@FILE@" ]; then
    gw_conf "$_gw_state" "$2"
    return 0
  fi
  case "$2" in
    SERVER) _gw_flag=server ;;
    TOKEN) _gw_flag=token ;;
    CONNECT_MODE) _gw_flag=connect-mode ;;
    LISTEN) _gw_flag=listen ;;
    *) return 0 ;;
  esac
  printf '%s\n' "$1" | gw_arg "$_gw_flag"
}
gw_unit_start() {
  systemctl show gpuworks-agent -p ExecStart --value 2>/dev/null
}
gw_unit_state() {
  gw_unit_start | gw_arg state
}
gw_unit_value() {
  gw_line_value "$(gw_unit_start)" "$1"
}
`)
