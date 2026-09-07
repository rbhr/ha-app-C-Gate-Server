#!/usr/bin/env python3
"""Exercise the actual entrypoint restart loop under set -e, with fake commands."""
from pathlib import Path
import os
import subprocess
import tempfile

source = Path('cgate-server/run.sh').read_text()
start = source.index('(\n    while true; do')
end = source.index('\n# --- Launch C-Gate', start)
loop = source[start:end].strip().removesuffix(' &')
for status in (0, 7, 137):
    with tempfile.TemporaryDirectory(prefix='cgate-startup-') as temp:
        directory = Path(temp)
        bridge = directory / 'bridge'
        bridge.write_text('#!/bin/sh\necho run >> "$PROBE_COUNT"\nexit '+str(status)+'\n')
        bridge.chmod(0o755)
        body = loop.replace('/cgate/cgate-web', str(bridge)).replace('sleep 2', '[ "$(wc -l < "$PROBE_COUNT")" -lt 2 ] || exit 0')
        env = dict(os.environ, PROBE_COUNT=str(directory/'count'))
        result = subprocess.run(['sh','-c','set -e\n'+body],env=env,capture_output=True,text=True,timeout=5)
        assert result.returncode == 0 and (directory/'count').read_text().count('run') == 2, result
        assert 'exited ('+str(status)+')' in result.stderr, result
print('PASS: restart after normal exit, failure and killed-process exit')
