package cli

import (
	"path/filepath"

	"gpuworks.ai/gpuworks/internal/server/selfinstall"
)

func clusterInstallDefaultDir(user string, noService bool) string {
	if !noService {
		return selfinstall.DefaultDir
	}
	if user == "root" {
		return "/root/gpuworks-cluster-new"
	}
	if user == "" {
		return "/srv/gpuworks-cluster-new"
	}
	return "/home/" + user + "/gpuworks-cluster-new"
}

// Only missing parents and the exclusive new installation directory are
// created. Existing parents are never chmod/chowned or recursively adopted.
const clusterInstallSystemDirectoryScript = `set -eu
umask 022
install_action=$1
install_target=$2
install_account=$3
install_parent=${install_target%/*}
install_leaf=${install_target##*/}
test ! -e "$install_target" && test ! -L "$install_target" || { echo 'installation directory already exists (including symlink)' >&2; exit 1; }
set -f
IFS=/
set -- $install_parent
unset IFS
cd /
install_existing=
for install_part do
  test -n "$install_part" || continue
  test ! -L "./$install_part" || { echo 'installation parent contains a symlink' >&2; exit 1; }
  if test ! -e "./$install_part"; then
    if test "$install_action" = check; then break; fi
    mkdir -m 755 -- "./$install_part"
  fi
  test -d "./$install_part" || { echo 'installation parent is not a directory' >&2; exit 1; }
  cd -P "./$install_part"
  install_existing=$install_existing/$install_part
  test "$(pwd -P)" = "$install_existing" || { echo 'installation parent changed' >&2; exit 1; }
done
if test "$install_action" = check; then
  install_probe=$(mktemp .gpuworks-cluster-check-XXXXXXXX)
  trap 'rm -f -- "$install_probe"' EXIT HUP INT TERM
  rm -f -- "$install_probe"
  trap - EXIT HUP INT TERM
else
  test "$install_action" = create
  test "$(pwd -P)" = "$install_parent"
  install_uid=$(id -u -- "$install_account")
  install_gid=$(id -g -- "$install_account")
  mkdir -m 700 -- "./$install_leaf"
  chown -h -- "$install_uid:$install_gid" "./$install_leaf"
  test ! -L "./$install_leaf" && test -d "./$install_leaf"
fi`

func clusterInstallDirectoryCommand(node clusterInstallNode, noService, create bool) string {
	if !noService {
		action := "check"
		if create {
			action = "create"
		}
		return "sudo -n sh -c " + clusterInstallQuote(clusterInstallSystemDirectoryScript) + " gpuworks-directory " + action + " " + clusterInstallQuote(node.Dir) + " " + clusterInstallQuote(node.SSH.User)
	}
	if create {
		return "mkdir -m 700 " + clusterInstallQuote(node.Dir)
	}
	parent := filepath.Dir(node.Dir)
	return "test -d " + clusterInstallQuote(parent) + "; test -w " + clusterInstallQuote(parent) + "; test \"$(cd " + clusterInstallQuote(parent) + " && pwd -P)\" = " + clusterInstallQuote(parent) + "; test ! -e " + clusterInstallQuote(node.Dir) + "; test ! -L " + clusterInstallQuote(node.Dir)
}
