"""plc_seal.py: seal a reviewed power-loss packet candidate for one run. It never approves the seal.

python3 -I plc_seal.py --candidate DIR --input-list FILE --input-pin SHA256 --prefix NAME/ \
                       --config CONFIG.json --producer DIR --tools DIR --product-head SHA --out NEWDIR

  --candidate   the unsealed candidate directory (never written)
  --input-list  the reviewed sha256 list (`<sha256>  <prefix>/<path>` lines) the candidate's oracle ran on
  --input-pin   sha256 of that list file, as the review recorded it
  --prefix      the list's path prefix for the packet's own files (other prefixes, e.g. staged binaries, are
                checked by their own pins and are not copied)
  --config      the run's prepared seaweed-cloud config; its sha256 is pinned in drill/plc-run-pins.json
  --producer    the producer the controller runs: exactly the files its SHA256SUMS names, plc_select.py among
                them, each verifying; that manifest's sha256 is pinned
  --tools       this repository's scenarios/plc: plc-tools.sha256 must name exactly the tools the scenario
                executes (REQUIRED_TOOLS), each verifying; that manifest's sha256 is pinned
  --product-head  the full product commit the run tests; pinned, and the scenario's product_head must equal it
  --out         a new directory, claimed exclusively (refused if it exists); its parent must exist

Every pinned or copied file is read ONCE: the bytes that are hashed are the bytes that are parsed or written,
so a file changed between check and copy cannot yield a new self-consistent seal. The staged copies are
hashed again against the reviewed rows, and the candidate is rescanned after the copy (a file that appeared
meanwhile refuses). Refuses: a list whose sha256 is not the pin; a listed file missing or changed; a file in
the candidate the list does not name (a __pycache__, a stray edit); a symlink; a text file with a CR byte; a
config that is not a JSON object with an output path; a producer or tool set off its manifest or missing a
required file; a product head that is not a full commit id; an --out that exists or overlaps the candidate,
producer or tools. On a refusal --out is left as it was: the tool removes only a directory it created.
The sealed packet is the listed files minus SHA256SUMS and UNSEALED, plus drill/plc-run-pins.json and a new
SHA256SUMS over every other file, written last. Prints the seal (sha256 of the new SHA256SUMS) and the
approved-seals line for the approver to record.
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
# Every controller-side tool scenarios/plc-powerloss-counted.yaml executes (test_plc_tools_manifest keeps this,
# plc_seal_check.sh's copy, plc-tools.sha256 and the YAML's calls equal).
REQUIRED_TOOLS = {'plc_seal_check.sh', 'plc_launch.sh', 'plc_settle.sh', 'plc_judge.py', 'plc_gate.sh',
                  'plc_owned.sh', 'plc_cleanup.sh'}
PRODUCER_ENTRY = 'plc_select.py'


class Refused(Exception):
    pass


def digest(data):
    return hashlib.sha256(data).hexdigest()


def read_file(path):
    """The one read of a file whose bytes are hashed and then used."""
    with open(path, 'rb') as f:
        return f.read()


def parse_list(data, prefix, what):
    rows = {}
    try:
        text = data.decode('utf-8')
    except UnicodeDecodeError:
        raise Refused('%s is not UTF-8' % what)
    for n, line in enumerate(text.splitlines(), 1):
        m = re.fullmatch(r'([0-9a-f]{64}) [ *](\S+)', line)
        if m is None:
            raise Refused('%s line %d is not "<sha256>  <path>"' % (what, n))
        sha, name = m.groups()
        if not name.startswith(prefix):
            continue
        rel = name[len(prefix):]
        if not rel or rel.startswith('/') or '..' in Path(rel).parts or rel in rows:
            raise Refused('%s names a bad or duplicate path: %s' % (what, name))
        rows[rel] = sha
    if not rows:
        raise Refused('%s names no file under %r' % (what, prefix))
    return rows


def files_under(d):
    out = set()
    for root, dirs, files in os.walk(d):
        for name in dirs + files:
            if (Path(root)/name).is_symlink():
                raise Refused('symlink in %s: %s' % (d, (Path(root)/name).relative_to(d)))
        out.update((Path(root)/name).relative_to(d).as_posix() for name in files)
    return out


def verify_rows(d, rows, what):
    for rel, sha in rows.items():
        try:
            data = read_file(Path(d)/rel)
        except OSError:
            raise Refused('%s file missing: %s' % (what, rel))
        if digest(data) != sha:
            raise Refused('%s file changed: %s' % (what, rel))


def pin_producer(d):
    """d holds exactly the files d/SHA256SUMS names (plc_select.py among them), each verifying."""
    try:
        raw = read_file(Path(d)/'SHA256SUMS')
    except OSError:
        raise Refused('producer has no SHA256SUMS: %s' % d)
    rows = parse_list(raw, '', 'producer SHA256SUMS')
    if PRODUCER_ENTRY not in rows:
        raise Refused('producer SHA256SUMS does not name %s' % PRODUCER_ENTRY)
    present = files_under(d) - {'SHA256SUMS'}
    if present != set(rows):
        raise Refused('producer files and its SHA256SUMS differ: %s' % ', '.join(sorted(present ^ set(rows))[:5]))
    verify_rows(d, rows, 'producer')
    return digest(raw)


def pin_tools(d):
    """d/plc-tools.sha256 names exactly REQUIRED_TOOLS, each verifying (d may hold other files, such as tests)."""
    try:
        raw = read_file(Path(d)/'plc-tools.sha256')
    except OSError:
        raise Refused('tools have no plc-tools.sha256: %s' % d)
    rows = parse_list(raw, '', 'plc-tools.sha256')
    if set(rows) != REQUIRED_TOOLS:
        raise Refused('plc-tools.sha256 does not name exactly the executed tools: %s'
                      % ', '.join(sorted(set(rows) ^ REQUIRED_TOOLS)))
    verify_rows(d, rows, 'tool')
    return digest(raw)


def overlaps(a, b):
    a, b = Path(a).resolve(), Path(b).resolve()
    return a == b or a in b.parents or b in a.parents


def seal(candidate, input_list, input_pin, prefix, config, producer, tools, product_head, out):
    candidate, out = Path(candidate), Path(out)
    if not re.fullmatch('[0-9a-f]{64}', input_pin or ''):
        raise Refused('--input-pin must be a sha256')
    if not re.fullmatch('[0-9a-f]{40}', product_head or ''):
        raise Refused('--product-head must be a full 40-hex commit id')
    for name, d in (('candidate', candidate), ('producer', producer), ('tools', tools)):
        if overlaps(out, d):
            raise Refused('--out overlaps the %s: %s' % (name, d))
    if not out.resolve().parent.is_dir():
        raise Refused('--out parent does not exist: %s' % out.resolve().parent)
    raw_list = read_file(input_list)
    if digest(raw_list) != input_pin:
        raise Refused('the input list is not the reviewed one (its sha256 is not the pin)')
    rows = parse_list(raw_list, prefix, 'input list')
    scanned = files_under(candidate)
    extra = sorted(scanned - set(rows))
    if extra:
        raise Refused('candidate files the reviewed list does not name: %s' % ', '.join(extra[:5]))
    missing = sorted(set(rows) - scanned)
    if missing:
        raise Refused('listed file missing: %s' % ', '.join(missing[:5]))
    raw_config = read_file(config)
    try:
        cfg = json.loads(raw_config.decode('utf-8'))
        if not isinstance(cfg, dict) or not isinstance(cfg.get('output'), str) or not cfg['output']:
            raise ValueError('no output path')
    except ValueError as error:
        raise Refused('config is not a seaweed-cloud run config: %s' % error)
    producer_pin = pin_producer(producer)
    tools_pin = pin_tools(tools)
    try:
        os.mkdir(out, 0o700)  # the exclusive claim: a concurrent seal or an existing directory refuses here
    except FileExistsError:
        raise Refused('--out exists: %s' % out)
    try:
        for rel, sha in sorted(rows.items()):
            if Path(rel).name in DROPPED and Path(rel).parent == Path('.'):
                continue
            data = read_file(candidate/rel)
            if digest(data) != sha:
                raise Refused('listed file changed since review: %s' % rel)
            if Path(rel).suffix in TEXT and b'\r' in data:
                raise Refused('CR byte in text file: %s' % rel)
            dst = out/rel
            dst.parent.mkdir(parents=True, exist_ok=True)
            with open(dst, 'xb') as f:
                f.write(data)
        if files_under(candidate) != scanned:
            raise Refused('the candidate changed while it was being sealed')
        staged = {rel: sha for rel, sha in rows.items() if not (Path(rel).name in DROPPED and Path(rel).parent == Path('.'))}
        if files_under(out) != set(staged):
            raise Refused('the staged files are not exactly the reviewed ones')
        verify_rows(out, staged, 'staged')
        pins = {'schema': 'plc-run-pins/v1', 'config_sha256': digest(raw_config), 'config_output': cfg['output'],
                'candidate_input_pin': input_pin, 'producer_sha256': producer_pin, 'tools_sha256': tools_pin,
                'product_head': product_head}
        (out/'drill').mkdir(exist_ok=True)
        with open(out/'drill'/'plc-run-pins.json', 'xb') as f:
            f.write((json.dumps(pins, indent=1, sort_keys=True) + '\n').encode('utf-8'))
        files = sorted(files_under(out))
        manifest = ''.join('%s  %s\n' % (digest(read_file(out/rel)), rel) for rel in files).encode('utf-8')
        with open(out/'SHA256SUMS', 'xb') as f:
            f.write(manifest)
    except BaseException:
        shutil.rmtree(out, ignore_errors=True)  # created by this call (the exclusive mkdir above)
        raise
    return digest(manifest), len(files)


def main(argv):
    ap = argparse.ArgumentParser(description=__doc__.split('\n')[0])
    for flag in ('--candidate', '--input-list', '--input-pin', '--prefix', '--config', '--producer', '--tools',
                 '--product-head', '--out'):
        ap.add_argument(flag, required=True)
    a = ap.parse_args(argv[1:])
    try:
        sha, n = seal(a.candidate, a.input_list, a.input_pin, a.prefix, a.config, a.producer, a.tools,
                      a.product_head, a.out)
    except (Refused, OSError) as error:
        print('REFUSED: %s' % error, file=sys.stderr)
        return 2
    print('SEALED %s files=%d' % (sha, n))
    print('approved-seals line (for the approver to record): s %s approver=<approver> <utc> %s' % (sha, os.path.abspath(a.out)))
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv))
