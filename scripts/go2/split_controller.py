#!/usr/bin/env python3
"""Extract controller-only modules from an explicitly pinned reference packet.

Never edits the source or seals an output. The retained compatibility launcher
sources modules in its existing shell so local claim and cleanup decisions keep
exactly their original state. The YAML port must expand remote calls itself;
executing this compatibility launcher is not runner unit custody.
"""
import argparse
import hashlib
import json
import pathlib
import re
import shutil


def one(text, marker):
    if text.count(marker) != 1:
        raise ValueError("reference anchor must occur exactly once: " + marker)
    return text.index(marker)


def split_launch(text):
    pre = one(text, "# Pre-claim (")
    hosts = one(text, "# ---- 1. contact both hosts")
    final = one(text, "# ---- 9. closeout:")
    dead = one(text, "# T1-E1 outage observation (")
    phases = list(re.finditer(r'^phase\(\) \{(?:  # 0 rc[^\n]*)?$', text, re.M))
    if len(phases) != 1:
        raise ValueError("reference must contain exactly one phase anchor across both forms")
    phase = phases[0].start()
    if not pre < hosts < dead < phase < final:
        raise ValueError("reference sections are not in the expected order")
    preflight, finalize = text[pre:hosts], text[final:]
    work = text[hosts:dead] + text[phase:final]
    replacements = {
        '  case "$n" in first|hop-*) t1phase "$n" ;; esac': '',
        '    case "$n" in first|hop-*) obswait "$h" ;; *) sleep "$POLL" ;; esac': '    sleep "$POLL"\n',
        '  case "$n" in first|hop-*) [ "$T1ST" = idle ] || t1step "$h" ;; esac   # window open: one more read at the phase end': '',
    }
    work = ''.join(replacements.get(line.rstrip('\n'), line) for line in work.splitlines(keepends=True))
    if re.search(r'\b(t1phase|t1step|obswait|T1ST)\b', work):
        raise ValueError("unrecognized remaining legacy observation call")
    prefix = text[:pre]
    launch = prefix + '. "$PK/preflight.sh"\n' + work + '. "$PK/finalize.sh"\n'
    return launch, preflight, finalize


def separate_paths(source, output):
    source, output = source.resolve(), output.resolve()
    return source != output and source not in output.parents and output not in source.parents


def materialize(source, output, launch_sha):
    if not separate_paths(source, output) or output.exists():
        raise ValueError("output must be fresh and separate from the reference")
    raw = (source / 'launch.sh').read_bytes()
    if hashlib.sha256(raw).hexdigest() != launch_sha:
        raise ValueError("reference launch SHA256 differs from the declared pin")
    if any(p.is_symlink() for p in source.rglob('*')):
        raise ValueError("reference packet contains symlinks")
    launch, preflight, finalize = split_launch(raw.decode().replace('\r\n', '\n'))
    shutil.copytree(source, output)
    for name, body in [('launch.sh', launch), ('preflight.sh', preflight), ('finalize.sh', finalize)]:
        (output / name).write_bytes(body.encode())
    hashes = {str(p.relative_to(output)).replace('\\', '/'): hashlib.sha256(p.read_bytes()).hexdigest()
              for p in sorted(output.rglob('*')) if p.is_file()}
    manifest = {'status': 'UNSEALED_REVIEW_ONLY', 'reference_launch_sha256': launch_sha,
                'modules': ['preflight.sh', 'finalize.sh'], 'files': hashes}
    (output / 'CONTROLLER-SPLIT.json').write_text(json.dumps(manifest, indent=2)+'\n')
    return manifest


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--packet', type=pathlib.Path, required=True)
    p.add_argument('--output', type=pathlib.Path, required=True)
    p.add_argument('--launch-sha256', required=True)
    a = p.parse_args()
    if not re.fullmatch('[0-9a-f]{64}', a.launch_sha256):
        p.error('launch-sha256 must be 64 lowercase hex digits')
    materialize(a.packet, a.output, a.launch_sha256)
    print('UNSEALED REVIEW ONLY: source unchanged; new modules require review and reseal')


if __name__ == '__main__':
    main()
