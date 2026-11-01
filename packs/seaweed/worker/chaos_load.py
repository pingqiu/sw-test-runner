"""Mixed KV+S3 load for cloud chaos stage 2 and the post-heal read-back.

Write-once: every SET/PUT targets a fresh key, so a failed write is UNKNOWN
for that key only and is never read back. Reads target ACKed keys only.
Wrong bytes, or an op that does not complete within 30 s, are fatal (rules 1
and 2); a completed error is a recorded outcome, judged by the controller.
"""
import argparse, hashlib, http.client, json, pathlib, sys, time
from urllib.error import HTTPError, URLError
from urllib.parse import quote
from urllib.request import Request, urlopen

S3_LENGTH = 256 * 1024


def kv_smoke():
    """The pinned sibling CLI, compiled from its exact source (as kv_smoke does)."""
    source = pathlib.Path(__file__).resolve().with_name('kv_smoke.py')
    module = type(sys)('_chaos_kv_smoke')
    module.__file__ = str(source)
    exec(compile(source.read_bytes(), str(source), 'exec'), module.__dict__)
    return module


def s3(url, data=None, method='GET'):
    """(ok, body or error). A socket timeout is a hang: it raises."""
    try:
        with urlopen(Request(url, data=data, method=method), timeout=30) as r:
            return True, r.read(S3_LENGTH + 1)
    except HTTPError as e:
        return False, 'http %d' % e.code
    except URLError as e:
        if isinstance(e.reason, TimeoutError):
            raise TimeoutError('S3 op exceeded 30 s')
        return False, str(e.reason)
    except (ConnectionError, http.client.HTTPException) as e:  # reset mid-response: a completed error
        return False, type(e).__name__


def s3_body(c, key):
    block = hashlib.sha256((c['run_id'] + '/' + key).encode()).digest()
    return (block * (S3_LENGTH // 32))[:S3_LENGTH]


def fids_of(c, path):
    with urlopen(c['filer_http'] + quote(path, safe='/') + '?metadata=true&resolveManifest=true', timeout=8) as r:
        chunks = json.load(r).get('chunks', [])
    return [x.get('file_id', x.get('fileId')) for x in chunks]


def load(conn, c, s3_base, seconds, journal, helper):
    bucket = s3_base + '/testops-' + c['run_id'] + '-' + c['client']
    assert s3(bucket, b'', 'PUT')[0], 'S3 bucket create'
    acked = {'kv': [], 's3': []}
    outcomes, unknown, n = [], [], 0
    started = time.monotonic()
    with open(journal, 'x', buffering=1) as out:
        while time.monotonic() - started < seconds:
            kind = ('kv_set', 'kv_get', 's3_put', 's3_get')[n % 4]
            if kind.endswith('get') and not acked[kind[:2]]:
                kind = kind[:2] + ('_set' if kind[:2] == 'kv' else '_put')
            key = '%s-w%d' % (c['client'], n) if kind in ('kv_set', 's3_put') else acked[kind[:2]][(n // 4) % len(acked[kind[:2]])]
            t0 = time.monotonic()
            if kind[:2] == 'kv':
                command = dict(action=kind[3:], key=key, seed=c['run_id'] + '/' + key, length=c['length'], operation_id=n)
                row = helper.work(conn, command)  # 30 s native bound; a hang raises
                ok = row['ok'] and row['results'] in (None, [True])
                if ok and kind == 'kv_get' and row['sha256'] != hashlib.sha256(helper.payload(command['seed'], c['length'])).hexdigest():
                    raise AssertionError('WRONG bytes for ' + key)
                error = row['error']
            else:
                body = s3_body(c, key)
                ok, got = s3(bucket + '/' + key, body if kind == 's3_put' else None, 'PUT' if kind == 's3_put' else 'GET')
                if ok and kind == 's3_get' and got != body:
                    raise AssertionError('WRONG bytes for ' + key)
                error = '' if ok else got
            t1 = time.monotonic()
            outcomes.append(dict(op=n, kind=kind, key=key, started=t0, ended=t1, exact=ok, error=str(error)))
            if kind in ('kv_set', 's3_put'):
                if ok:
                    acked[kind[:2]].append(key)
                    path = c['root'] + '/' + key if kind == 'kv_set' else '/buckets/testops-%s-%s/%s' % (c['run_id'], c['client'], key)
                    out.write(json.dumps(dict(event='ACK', kind=kind[:2], key=key, fids=fids_of(c, path))) + '\n')
                else:
                    unknown.append(key)
            n += 1
            if n > 4096:
                raise AssertionError('bounded op evidence exceeded')
            time.sleep(.25)
    ended = time.monotonic()
    return dict(status='OBSERVED', stage='load', run_id=c['run_id'], client=c['client'], started=started, ended=ended,
                outcomes=outcomes, acked_kv=len(acked['kv']), acked_s3=len(acked['s3']), unknown=unknown)


def readback(conn, c, s3_base, journal, helper):
    bucket = s3_base + '/testops-' + c['run_id'] + '-' + c['client']
    rows = [json.loads(x) for x in pathlib.Path(journal).read_text().splitlines()]
    for i, r in enumerate(rows):
        if r['kind'] == 'kv':
            command = dict(action='get', key=r['key'], seed=c['run_id'] + '/' + r['key'], length=c['length'], operation_id=i)
            row = helper.work(conn, command)
            exact = row['ok'] and row['results'] in (None, [True]) and row['sha256'] == hashlib.sha256(helper.payload(command['seed'], c['length'])).hexdigest()
        else:
            ok, got = s3(bucket + '/' + r['key'])
            exact = ok and got == s3_body(c, r['key'])
        if not exact:
            raise AssertionError('ACKed %s %s did not read back exactly' % (r['kind'], r['key']))
    return dict(status='PASS', stage='readback', run_id=c['run_id'], client=c['client'],
                kv=sum(r['kind'] == 'kv' for r in rows), s3=sum(r['kind'] == 's3' for r in rows))


def main():
    p = argparse.ArgumentParser()
    p.add_argument('--config', required=True)
    p.add_argument('--s3', required=True)
    p.add_argument('--stage', choices=('load', 'readback'), required=True)
    p.add_argument('--seconds', type=float, default=120)
    p.add_argument('--journal', required=True)
    a = p.parse_args()
    emit = lambda obj: print(json.dumps(obj, allow_nan=False), flush=True)
    conn = None
    try:
        c = json.loads(a.config)
        if c['transport'] != 'http' or not 0 < a.seconds <= 600:
            raise ValueError('chaos load is HTTP only, 0 < seconds <= 600')
        kv = kv_smoke()
        helper = kv.exact_helper('soak_native_worker')
        kv.configure(c)
        sys.path[:0] = [c['helper_dir'], c['native_dir']]
        from seaweedkv_native._native import SeaweedKVConnector
        options = dict(capacity_bytes=1, root=c['root'], collection='lmcache', replication=c.get('replication', '000'))
        conn = SeaweedKVConnector(c['filer_grpc'], 'loader-http', 4, json.dumps(options))
        helper.validate_api(type(conn))
        result = load(conn, c, a.s3, a.seconds, a.journal, helper) if a.stage == 'load' else readback(conn, c, a.s3, a.journal, helper)
        emit(result)
        return 0
    except Exception as error:
        emit(dict(status='FAIL', code='CHAOS_LOAD_FAILED', error='%s: %s' % (type(error).__name__, error)))
        return 1
    finally:
        if conn is not None:
            conn.close()


if __name__ == '__main__':
    raise SystemExit(main())
