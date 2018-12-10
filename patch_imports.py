#!/usr/bin/env python3

"""Script for rewriting imports from github.com/biogo -> github.com/grailbio/hts.

Usage:

  cd $GRAIL/go/srcgitthub.com/grailbio/hts
  ./patch_imports.py

"""


import os
import logging
import re
from typing import List

def patch(path: str) -> None:
    """Patch the import lines of the given python. It updates the file in place."""
    changed = False
    lines: List[str] = []
    with open(path) as fd:
        for line in fd.readlines():
            new_line = re.sub(r'"github.com/biogo/hts', '"github.com/grailbio/hts', line)
            if new_line != line:
                line = new_line
                changed = True
            lines.append(line)
    if not changed:
        return
    logging.info('Patching %s', path)
    with open(path, 'w') as fd:
        for line in lines:
            fd.write(line)

def main() -> None:
    """Entry point"""
    logging.basicConfig(level=logging.DEBUG)
    for root, _, files in os.walk('.'):
        for path in files:
            if path.endswith('.go'):
                patch(os.path.join(root, path))

main()
