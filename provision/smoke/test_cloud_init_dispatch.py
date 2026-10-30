"""Local-only Terraform custom_data -> actual cloud-init dispatch control."""
import base64
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
from cloudinit import handlers, helpers, user_data

def main():
    source = Path(sys.argv[1]).resolve()
    line = next(line for line in (source / 'main.tf').read_text().splitlines()
                if line.strip().startswith('custom_data ='))
    expression = line.split('=', 1)[1].strip().replace('${path.module}', str(source))
    env = {k: v for k, v in os.environ.items() if not k.startswith('TF_')}
    env['CHECKPOINT_DISABLE'] = '1'
    with tempfile.TemporaryDirectory(prefix='cloud-init-dispatch-') as scratch:
        for label, replacement in [('null', 'null'), ('generated', '{ed25519_private="CONTROL_PRIVATE",ed25519_public="CONTROL_PUBLIC"}')]:
            output = subprocess.run(['/usr/local/bin/terraform', 'console', '-no-color'],
                input=expression.replace('var.host_keys', replacement) + '\n',
                text=True, capture_output=True, cwd=scratch, env=env, check=True)
            payload = base64.b64decode(json.loads(output.stdout.strip()))
            processed = user_data.UserDataProcessor(helpers.Paths({})).process(payload)
            types = [p.get_content_type() for p in processed.walk() if not p.is_multipart()]
            print(json.dumps(dict(branch=label, first_line=payload.splitlines()[0].decode(),
                detected=handlers.type_from_starts_with(payload), processed_types=types)))
            assert 'text/cloud-config' in types, label

if __name__ == '__main__':
    main()
