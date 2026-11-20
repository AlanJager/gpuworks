#!/bin/sh
set -eu

# The Go test also runs on developer machines with only sh. In CI the
# generated scripts must actually be checked, with the same rules on both hosts.
version=$(shellcheck --version)
printf '%s\n' "$version"
if ! printf '%s\n' "$version" | grep -Fxq 'version: 0.9.0'; then
    echo 'Slurm bootstrap CI requires ShellCheck 0.9.0' >&2
    exit 1
fi
CGO_ENABLED=0 go test -count=1 -run '^TestScriptsParseUnderEveryShell$' ./internal/server/slurmboot
