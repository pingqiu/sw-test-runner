"""plc_seal.py: seal a reviewed power-loss packet candidate for one run. It never approves the seal.

python3 -I plc_seal.py --candidate DIR --input-list FILE --input-pin SHA256 --prefix NAME/ \
                       --config CONFIG.json --producer DIR --tools DIR --product-head SHA --out NEWDIR

  --candidate   the unsealed candidate directory (left unchanged)
  --input-list  the reviewed sha256 list (`<sha256>  <prefix>/<path>` lines) the candidate's oracle ran on
  --input-pin   sha256 of that list file, as the review recorded it
  --prefix      the list's path prefix for the packet's own files (other prefixes, e.g. staged binaries, are
                checked by their own pins and are not copied)
  --config      the run's prepared seaweed-cloud config; its sha256 is pinned in drill/plc-run-pins.json
  --producer    the producer the controller runs (plc_select.py ...); every file must verify against its
                SHA256SUMS, whose sha256 is pinned in drill/plc-run-pins.json
  --tools       this repository's scenarios/plc: every file its plc-tools.sha256 names must verify; that
                manifest's sha256 is pinned (the controller-side tools the run executes)
  --product-head  the full product commit the run tests; pinned, and the scenario's product_head must equal it
  --out         a new directory for the sealed packet (refused if it exists)

Refuses, writing nothing under --out: a list whose sha256 is not the pin; a listed file missing or changed; a
file in the candidate that the list does not name (a __pycache__, a stray edit); a symlink; a text file with a
CR byte; a config that is not a JSON object with an output path; a producer whose files do not verify; a tool
that does not verify; a product head that is not a full commit id. The sealed packet is the listed files minus
SHA256SUMS and UNSEALED, plus drill/plc-run-pins.json and a new SHA256SUMS over every other file.
Prints the seal (sha256 of the new SHA256SUMS) and the approved-seals line for the approver to record.
"""
import argparse
import hashlib
import json
import os
import re
import shutil
import sys
from pathlib import Path

DROPPED = {'SHA256SUMS', 'UNSEALED'}  # the candidate's stale manifest and its marker never enter a seal
TEXT = {'.py', '.sh', '.md', '.json', '.yaml', '.yml', '.txt', '.tsv', '.cfg', '.env', '.sha256'}


class Refused(Exception):
    pass


def sha256(path):
    h = hashlib.sha256()
    with open(path, 'rb') as f:
        for block in iter(lambda: f.read(1 << 20), b''):
            h.update(block)
    return h.hexdigest()


def read_list(path, prefix):
    rows = {}
    for n, line in enumerate(Path(path).read_text(encoding='utf-8').splitlines(), 1):
        m = re.fullmatch(r'([0-9a-f]{64}) [ *](\S+)', line)
        if m is None:
            raise Refused('input list line %d is not "<sha256>  <path>"' % n)
        digest, name = m.groups()
        if not name.startswith(prefix):
            continue
        rel = name[len(prefix):]
        if not rel or rel.startswith('/') or '..' in Path(rel).parts or rel in rows:
            raise Refused('input list names a bad or duplicate path: %s' % name)
        rows[rel] = digest
    if not rows:
        raise Refused('input list names no file under %s' % prefix)
    return rows


def verify_dir(d):
    """Every file of d is named, with its hash, by d/SHA256SUMS, and nothing else is in d."""
    d = Path(d)
    if not (d/'SHA256SUMS').is_file():
        raise Refused('producer has no SHA256SUMS: %s' % d)
    rows = read_list(d/'SHA256SUMS', '')
    present = {p.relative_to(d).as_posix() for p in d.rglob('*') if p.is_file()} - {'SHA256SUMS'}
    if present != set(rows):
        raise Refused('producer files and its SHA256SUMS differ: %s' % ', '.join(sorted(present ^ set(rows))[:5]))
    for rel, digest in rows.items():
        if sha256(d/rel) != digest:
            raise Refused('producer file changed: %s' % rel)
    return sha256(d/'SHA256SUMS')


def verify_listed(d, manifest):
    """Every file d/manifest names verifies (d may hold other files, such as tests); returns the manifest's sha256."""
    d = Path(d)
    if not (d/manifest).is_file():
        raise Refused('tools have no %s: %s' % (manifest, d))
    rows = read_list(d/manifest, '')
    for rel, digest in rows.items():
        if not (d/rel).is_file() or sha256(d/rel) != digest:
            raise Refused('tool missing or changed: %s' % rel)
    return sha256(d/manifest)


def seal(candidate, input_list, input_pin, prefix, config, producer, tools, product_head, out):
    candidate, out = Path(candidate), Path(out)
    if not re.fullmatch('[0-9a-f]{64}', input_pin or ''):
        raise Refused('--input-pin must be a sha256')
    if not re.fullmatch('[0-9a-f]{40}', product_head or ''):
        raise Refused('--product-head must be a full 40-hex commit id')
    if sha256(input_list) != input_pin:
        raise Refused('the input list is not the reviewed one (its sha256 is not the pin)')
    rows = read_list(input_list, prefix)
    on_disk = set()
    for root, dirs, files in os.walk(candidate):
        for name in dirs + files:
            p = Path(root)/name
            if p.is_symlink():
                raise Refused('symlink in the candidate: %s' % p.relative_to(candidate))
        for name in files:
            on_disk.add((Path(root)/name).relative_to(candidate).as_posix())
    extra = sorted(on_disk - set(rows))
    if extra:
        raise Refused('candidate files the reviewed list does not name: %s' % ', '.join(extra[:5]))
    for rel, digest in sorted(rows.items()):
        p = candidate/rel
        if not p.is_file():
            raise Refused('listed file missing: %s' % rel)
        if sha256(p) != digest:
            raise Refused('listed file changed since review: %s' % rel)
        if p.suffix in TEXT and b'\r' in p.read_bytes():
            raise Refused('CR byte in text file: %s' % rel)
    try:
        cfg = json.loads(Path(config).read_text(encoding='utf-8'))
        if not isinstance(cfg, dict) or not isinstance(cfg.get('output'), str) or not cfg['output']:
            raise ValueError('no output path')
    except (OSError, ValueError) as error:
        raise Refused('config is not a seaweed-cloud run config: %s' % error)
    producer_pin = verify_dir(producer)
    tools_pin = verify_listed(tools, 'plc-tools.sha256')
    if out.exists():
        raise Refused('--out exists: %s' % out)
    staging = out.with_name(out.name + '.partial')
    if staging.exists():
        raise Refused('a partial seal is in the way: %s' % staging)
    try:
        for rel in sorted(rows):
            if Path(rel).name in DROPPED and Path(rel).parent == Path('.'):
                continue
            dst = staging/rel
            dst.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(candidate/rel, dst)
        pins = {'schema': 'plc-run-pins/v1', 'config_sha256': sha256(config), 'config_output': cfg['output'],
                'candidate_input_pin': input_pin, 'producer_sha256': producer_pin, 'tools_sha256': tools_pin,
                'product_head': product_head}
        (staging/'drill').mkdir(exist_ok=True)
        (staging/'drill'/'plc-run-pins.json').write_text(json.dumps(pins, indent=1, sort_keys=True) + '\n', encoding='utf-8')
        files = sorted(p.relative_to(staging).as_posix() for p in staging.rglob('*') if p.is_file())
        manifest = ''.join('%s  %s\n' % (sha256(staging/rel), rel) for rel in files)
        (staging/'SHA256SUMS').write_bytes(manifest.encode('utf-8'))
        os.rename(staging, out)
    except BaseException:
        shutil.rmtree(staging, ignore_errors=True)
        raise
    return sha256(out/'SHA256SUMS'), len(files)


def main(argv):
    ap = argparse.ArgumentParser(description=__doc__.split('\n')[0])
    for flag in ('--candidate', '--input-list', '--input-pin', '--prefix', '--config', '--producer', '--tools',
                 '--product-head', '--out'):
        ap.add_argument(flag, required=True)
    a = ap.parse_args(argv[1:])
    try:
        digest, n = seal(a.candidate, a.input_list, a.input_pin, a.prefix, a.config, a.producer, a.tools,
                         a.product_head, a.out)
    except Refused as error:
        print('REFUSED: %s' % error, file=sys.stderr)
        return 2
    print('SEALED %s files=%d' % (digest, n))
    print('approved-seals line (for the approver to record): s %s approver=<approver> <utc> %s' % (digest, os.path.abspath(a.out)))
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv))
