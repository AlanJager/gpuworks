#!/usr/bin/env python3
"""Materialize this AMD overlay with verified node-visible images."""
from pathlib import Path
import sys
sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
from verified_import import main
if __name__ == "__main__":
    main(Path(__file__).resolve().parent)
