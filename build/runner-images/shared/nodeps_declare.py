#!/usr/bin/env python3
"""Declare an installed package's `pip check` complaints as deliberate.

usage: nodeps_declare.py PACKAGE REASON

Appends {name, version, reason} (the installed version of PACKAGE) to
/opt/gpuworks/pip-nodeps.json, creating it if absent. The audit gate
(overlays/audit/stack-audit.py, GPUWORKS_PIP_CHECK_NODEPS) lets through only the
`pip check` lines that start with "<name> <version> ", so the declaration is
bound to this exact package at this exact version. A package that is not
installed is not declared (nothing to excuse).
"""
import importlib.metadata
import json
import os
import sys

PATH = '/opt/gpuworks/pip-nodeps.json'


def main(argv):
    if len(argv) != 3:
        print(__doc__, file=sys.stderr)
        return 2
    name, reason = argv[1], argv[2]
    try:
        version = importlib.metadata.version(name)
    except importlib.metadata.PackageNotFoundError:
        print('nodeps_declare: %s is not installed, nothing declared' % name)
        return 0
    declared = json.load(open(PATH)) if os.path.exists(PATH) else []
    declared = [d for d in declared if d['name'].lower() != name.lower()]
    declared.append({'name': name, 'version': version, 'reason': reason})
    json.dump(declared, open(PATH, 'w'), indent=2)
    print(json.dumps(declared, indent=2))
    return 0


sys.exit(main(sys.argv))
