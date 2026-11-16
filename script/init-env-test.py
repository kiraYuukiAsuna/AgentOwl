#!/usr/bin/env python3
"""Check fresh deployment IDs and preservation of existing configuration."""
import pathlib
import subprocess
import tempfile
import uuid

script = pathlib.Path(__file__).with_name('init-env.sh')
with tempfile.TemporaryDirectory() as directory:
    target = pathlib.Path(directory)
    def initialize():
        subprocess.run([str(script), '--directory', directory], check=True, stdout=subprocess.DEVNULL)
    initialize()
    env = (target / '.env').read_text()
    machine_id = next(line.split('=', 1)[1] for line in env.splitlines() if line.startswith('RUNNER_MACHINE_ID='))
    assert str(uuid.UUID(machine_id)) == machine_id
    token = (target / 'data/secrets/runner_service_token').read_bytes()
    initialize()
    assert (target / '.env').read_text() == env
    assert (target / 'data/secrets/runner_service_token').read_bytes() == token
    existing = env.replace(machine_id, '')
    (target / '.env').write_text(existing)
    initialize()
    assert (target / '.env').read_text() == existing
print('Deployment initialization checks passed.')
