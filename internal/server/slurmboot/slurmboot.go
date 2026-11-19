// Package slurmboot renders the scripts that onboard a Slurm cluster: a
// health check program that every compute node's slurmd runs and that
// installs the gpuworks agent on GPU nodes by itself, and the controller-side
// setup script that puts it in place. Both are pure functions of their
// parameters; the package knows nothing of the store or of how the scripts are
// served, and imports nothing from this repository.
package slurmboot

import (
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// Defaults for the parameters a cluster entry may leave empty.
const (
	DefaultRunAs           = "root"
	DefaultInstallDir      = "/var/lib/gpuworks"
	DefaultScriptDir       = "/opt/gpuworks-slurm"
	DefaultIntervalSeconds = 300
)

//go:embed check.sh
var checkTemplate string

//go:embed setup.sh
var setupTemplate string

// The setup script is assembled from six files to keep each readable.
//
//go:embed setup_conf.sh
var setupConfPart string

//go:embed setup_nodes.sh
var setupNodesPart string

//go:embed setup_reach.sh
var setupReachPart string

//go:embed setup_check.sh
var setupCheckPart string

//go:embed setup_dir.sh
var setupDirPart string

const (
	paramsMarker = "# @@PARAMS@@"
	checkMarker  = "@@CHECK_SCRIPT@@"
	confMarker   = "# @@CONF_FUNCTIONS@@\n"
	nodesMarker  = "# @@NODE_FUNCTIONS@@\n"
	reachFuncs   = "# @@REACH_FUNCTIONS@@\n"
	checkFuncs   = "# @@CHECK_FUNCTIONS@@\n"
	dirFuncs     = "# @@DIR_FUNCTIONS@@\n"
)

// Params are one cluster entry's script parameters.
type Params struct {
	ServerURL  string // base URL, no trailing slash, e.g. https://gw.example:8800
	Token      string // cluster enrollment token (opaque, URL-safe chars)
	RunAs      string // agent account; "root" or a login name
	InstallDir string // absolute path, the agent's install+state dir on every node
	ScriptDir  string // absolute path where check.sh lives, same path on every node
}

// ErrInvalidParams is what every *FieldError unwraps to.
var ErrInvalidParams = errors.New("invalid slurm script parameters")

// FieldError names the parameter that failed validation, so a caller can map
// it to an API field error.
type FieldError struct {
	Field  string // the Params field name, e.g. "ServerURL"
	Reason string
}

func (e *FieldError) Error() string { return "slurmboot: " + e.Field + ": " + e.Reason }

// Unwrap lets errors.Is(err, ErrInvalidParams) match.
func (e *FieldError) Unwrap() error { return ErrInvalidParams }

var (
	serverURLPattern = regexp.MustCompile(`^https?://(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?)(:[0-9]{1,5})?(/[A-Za-z0-9._~%+/-]*[A-Za-z0-9._~%+-])?$`)
	tokenPattern     = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	runAsPattern     = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)
	dirPattern       = regexp.MustCompile(`^/[A-Za-z0-9_./-]+$`)
)

const (
	maxTokenLen = 512
	maxRunAsLen = 32
)

// WithDefaults fills the empty optional fields with their defaults.
func (p Params) WithDefaults() Params {
	if p.RunAs == "" {
		p.RunAs = DefaultRunAs
	}
	if p.InstallDir == "" {
		p.InstallDir = DefaultInstallDir
	}
	if p.ScriptDir == "" {
		p.ScriptDir = DefaultScriptDir
	}
	return p
}

// Validate rejects every value outside the safe character sets, so the
// renderers can embed them in shell text without escaping tricks. It does not
// fill defaults: see WithDefaults. The returned error is a *FieldError.
func (p Params) Validate() error {
	if err := validateServerURL(p.ServerURL); err != nil {
		return err
	}
	if p.Token == "" || len(p.Token) > maxTokenLen || !tokenPattern.MatchString(p.Token) {
		return &FieldError{"Token", "must be 1-" + strconv.Itoa(maxTokenLen) + " characters of A-Z a-z 0-9 _ . -"}
	}
	if p.RunAs == "" || len(p.RunAs) > maxRunAsLen || !runAsPattern.MatchString(p.RunAs) {
		return &FieldError{"RunAs", "must be a login name: lower case letters, digits, _ and -, starting with a letter or _"}
	}
	if err := validateDir("InstallDir", p.InstallDir); err != nil {
		return err
	}
	return validateDir("ScriptDir", p.ScriptDir)
}

func validateServerURL(s string) error {
	bad := func(reason string) error { return &FieldError{"ServerURL", reason} }
	if !serverURLPattern.MatchString(s) {
		return bad("must look like http(s)://host[:port][/path], without a query, quotes, spaces or a trailing slash")
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return bad("is not a valid base URL")
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return bad("has an invalid port")
		}
	}
	return nil
}

func validateDir(field, dir string) error {
	switch {
	case !dirPattern.MatchString(dir):
		return &FieldError{field, "must be an absolute path of A-Z a-z 0-9 _ . / -"}
	case strings.Contains(dir, ".."):
		return &FieldError{field, "must not contain .."}
	case path.Clean(dir) != dir:
		return &FieldError{field, "must be a clean path without a trailing or doubled slash"}
	}
	return nil
}

// shQuote single-quotes s for a POSIX shell. Validated values contain no
// single quote, but the renderers stay correct for any input.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// paramsBlock is the setup script's parameters. The health check's omit the
// token (checkParamsBlock): the check script is world readable on every node.
func paramsBlock(p Params) string {
	return strings.Replace(checkParamsBlock(p), "SERVER_URL="+shQuote(p.ServerURL)+"\n",
		"SERVER_URL="+shQuote(p.ServerURL)+"\nTOKEN="+shQuote(p.Token)+"\n", 1)
}

func checkParamsBlock(p Params) string {
	var b strings.Builder
	fmt.Fprintf(&b, "SERVER_URL=%s\n", shQuote(p.ServerURL))
	fmt.Fprintf(&b, "RUN_AS=%s\n", shQuote(p.RunAs))
	fmt.Fprintf(&b, "INSTALL_DIR=%s\n", shQuote(p.InstallDir))
	fmt.Fprintf(&b, "SCRIPT_DIR=%s\n", shQuote(p.ScriptDir))
	// The health check interval, which spreads the install over half of it;
	// the setup script writes the cluster's own in place of this default.
	fmt.Fprintf(&b, "INTERVAL=%d", DefaultIntervalSeconds)
	return b.String()
}

// TokenFileName is the file in the script directory that holds the cluster's
// enrollment token: written by the setup script (mode 0600, root), read by
// the health check, which is itself world readable and carries no token.
const TokenFileName = "token"

// CheckScript is the health check program (check.sh) with the parameters
// baked in, none of them secret: the enrollment token is read from
// TokenFileName in the script directory. Call Validate first.
func CheckScript(p Params) string {
	return strings.Replace(checkTemplate, paramsMarker, checkParamsBlock(p), 1)
}

// SetupScript is the root-run setup/remove script served at /slurm/setup.sh.
// It embeds CheckScript(p) in a quoted here-document, so it needs no further
// download. Call Validate first.
func SetupScript(p Params) string {
	s := setupSource()
	s = strings.Replace(s, paramsMarker, paramsBlock(p), 1)
	return strings.Replace(s, checkMarker, strings.TrimSuffix(CheckScript(p), "\n"), 1)
}

// setupSource is setup.sh with its fragments in place.
func setupSource() string {
	s := strings.Replace(setupTemplate, confMarker, setupConfPart, 1)
	s = strings.Replace(s, nodesMarker, setupNodesPart, 1)
	s = strings.Replace(s, reachFuncs, setupReachPart, 1)
	s = strings.Replace(s, checkFuncs, setupCheckPart, 1)
	return strings.Replace(s, dirFuncs, setupDirPart, 1)
}

func scriptURL(serverURL, token string) string {
	return serverURL + "/slurm/setup.sh?token=" + url.QueryEscape(token)
}

// CheckQuery is the query parameter of the setup script's URL that says the
// script is fetched to check only, so the server does not take the fetch for
// a setup having run.
const CheckQuery = "check"

func command(serverURL, token, args string) string {
	return "curl -fsSL " + shQuote(scriptURL(serverURL, token)) + " | sudo sh" + args
}

// SetupCommand is the command the console shows for installing the health
// check on the Slurm controller.
func SetupCommand(serverURL, token string) string { return command(serverURL, token, "") }

// CheckCommand is the command that says what the setup would do on the
// controller and changes nothing (setup.sh --check): the same script, so what
// it reports is what SetupCommand then does.
func CheckCommand(serverURL, token string) string {
	return "curl -fsSL " + shQuote(scriptURL(serverURL, token)+"&"+CheckQuery+"=1") + " | sudo sh -s -- --check"
}

// RemoveCommand is the command that takes the health check out again.
func RemoveCommand(serverURL, token string) string {
	return command(serverURL, token, " -s -- --remove")
}

// UninstallAgentsCommand is the first of the two uninstall steps: it makes
// every node's check remove its agent. RemoveCommand is the second.
func UninstallAgentsCommand(serverURL, token string) string {
	return command(serverURL, token, " -s -- --uninstall-agents")
}
