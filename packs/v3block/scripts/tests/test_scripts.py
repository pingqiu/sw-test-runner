"""Unit tests of the block pack's declared scripts (packs/v3block/scripts).

The declarations run each script's text as `python3 -c <text> ARGS` on the
node; these tests run the same files as `python3 <file> ARGS` (sys.argv[1:]
is the same) against a temporary directory and a fake filer.

Run: python -m unittest discover -s packs/v3block/scripts/tests
"""

import hashlib
import http.server
import json
import os
import subprocess
import sys
import tempfile
import threading
import unittest
import urllib.parse

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPTS = os.path.dirname(HERE)


def rd(path):
    with open(path, "rb") as f:
        return f.read()


def wr(path, data):
    with open(path, "wb") as f:
        f.write(data)


def run(script, *args):
    p = subprocess.run([sys.executable, "-I", os.path.join(SCRIPTS, script), *args],
                       capture_output=True, text=True, timeout=30)
    return p.returncode, p.stdout, p.stderr


class BlockFill(unittest.TestCase):
    """block_fill.py TAG PATH: the tag repeated to one 4096-byte block."""

    def test_repeats_the_tag_to_one_block(self):
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "blk")
            code, _, err = run("block_fill.py", "v1|r1|7|", path)
            self.assertEqual(code, 0, err)
            data = rd(path)
            self.assertEqual(len(data), 4096)
            self.assertEqual(data, (b"v1|r1|7|" * 600)[:4096])

    def test_the_hash_the_runner_records(self):
        # The runner computes the same sha256 before the invoke
        # (declared.RepeatSHA); a block of another tag differs.
        with tempfile.TemporaryDirectory() as d:
            a, b = os.path.join(d, "a"), os.path.join(d, "b")
            run("block_fill.py", "v1|r1|0|", a)
            run("block_fill.py", "v1|r1|1|", b)
            want = hashlib.sha256((b"v1|r1|0|" * 600)[:4096]).hexdigest()
            self.assertEqual(hashlib.sha256(rd(a)).hexdigest(), want)
            self.assertNotEqual(rd(a), rd(b))

    def test_overwrites_a_longer_file(self):
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "blk")
            wr(path, b"x" * 9000)
            run("block_fill.py", "t", path)
            self.assertEqual(rd(path), b"t" * 4096)

    def test_red_control_an_unwritable_path_fails(self):
        with tempfile.TemporaryDirectory() as d:
            code, _, _ = run("block_fill.py", "t", os.path.join(d, "no", "such", "dir", "blk"))
            self.assertNotEqual(code, 0)


class DeviceFsync(unittest.TestCase):
    """device_fsync.py DEV: fsync(2) of the path; exit 0 only after it returns."""

    def test_fsync_of_a_writable_file(self):
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "dev")
            wr(path, b"x")
            code, _, err = run("device_fsync.py", path)
            self.assertEqual(code, 0, err)
            self.assertEqual(rd(path), b"x")  # opened without truncation

    def test_red_control_a_missing_device_fails(self):
        with tempfile.TemporaryDirectory() as d:
            code, _, err = run("device_fsync.py", os.path.join(d, "missing"))
            self.assertNotEqual(code, 0)
            self.assertIn("Error", err)


class FakeFiler:
    """A filer HTTP listing: tree maps a directory path to its entries
    [(name, is_dir)]; page is the listing's page size."""

    def __init__(self, tree, page=2, fail=None):
        self.tree, self.page, self.fail, self.queries = tree, page, fail, []
        filer = self

        class H(http.server.BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_GET(self):
                u = urllib.parse.urlparse(self.path)
                q = urllib.parse.parse_qs(u.query)
                filer.queries.append((u.path, q))
                d = urllib.parse.unquote(u.path).rstrip("/") or "/"
                if filer.fail and d == filer.fail:
                    self.send_response(500)
                    self.end_headers()
                    return
                if d not in filer.tree:
                    self.send_response(404)
                    self.end_headers()
                    return
                ents = sorted(filer.tree[d])
                pat = q.get("namePattern", [""])[0]
                if pat.endswith("*"):
                    ents = [e for e in ents if e[0].startswith(pat[:-1])]
                last = q.get("lastFileName", [""])[0]
                if last:
                    ents = [e for e in ents if e[0] > last]
                more = len(ents) > filer.page
                ents = ents[:filer.page]
                body = {"Entries": [{"FullPath": d.rstrip("/") + "/" + n, "Mode": (0o20000000000 | 0o755) if isdir else 0o644} for n, isdir in ents] or None,
                        "ShouldDisplayLoadMore": more}
                b = json.dumps(body).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(b)

        self.srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
        self.url = "http://127.0.0.1:%d" % self.srv.server_address[1]
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()

    def close(self):
        self.srv.shutdown()
        self.srv.server_close()


class FilerDirs(unittest.TestCase):
    """filer_dirs.py DIR PREFIX: the sorted names under DIR starting with
    PREFIX, every page read; a missing DIR is an empty list."""

    def setUp(self):
        self.f = FakeFiler({"/chunks": [("v1-aaaa", True), ("v1-bbbb", True), ("v1-cccc", True), ("v10-dddd", True), ("v2-eeee", True)]})
        self.addCleanup(self.f.close)

    def test_every_page_and_only_the_prefix(self):
        code, out, err = run("filer_dirs.py", self.f.url + "/chunks", "v1-")
        self.assertEqual(code, 0, err)
        self.assertEqual(json.loads(out), ["v1-aaaa", "v1-bbbb", "v1-cccc"])
        self.assertGreater(len(self.f.queries), 1, "a listing longer than a page is read page by page")

    def test_a_missing_directory_is_empty(self):
        code, out, err = run("filer_dirs.py", self.f.url + "/nowhere", "v1-")
        self.assertEqual(code, 0, err)
        self.assertEqual(json.loads(out), [])

    def test_red_control_a_failing_filer_is_not_an_empty_list(self):
        f = FakeFiler({"/chunks": []}, fail="/chunks")
        self.addCleanup(f.close)
        code, out, _ = run("filer_dirs.py", f.url + "/chunks", "v1-")
        self.assertNotEqual(code, 0)
        self.assertEqual(out.strip(), "")


class FilerFiles(unittest.TestCase):
    """filer_files.py DIR: every file below DIR, as sorted paths relative to
    it, through every page and subdirectory; a missing DIR is empty."""

    def setUp(self):
        self.f = FakeFiler({
            "/chunks/v1-aaaa": [("HEAD", False), ("snap", True), ("wal 1", True), ("z", False)],
            "/chunks/v1-aaaa/snap": [("s1", False), ("s2", False), ("s3", False)],
            "/chunks/v1-aaaa/wal 1": [("seg", False)],
        })
        self.addCleanup(self.f.close)

    def test_recursive_and_paged(self):
        code, out, err = run("filer_files.py", self.f.url + "/chunks/v1-aaaa")
        self.assertEqual(code, 0, err)
        self.assertEqual(json.loads(out), ["HEAD", "snap/s1", "snap/s2", "snap/s3", "wal 1/seg", "z"])

    def test_a_missing_directory_is_empty(self):
        code, out, err = run("filer_files.py", self.f.url + "/chunks/v9-0000")
        self.assertEqual(code, 0, err)
        self.assertEqual(json.loads(out), [])

    def test_red_control_a_failing_subdirectory_fails_the_listing(self):
        f = FakeFiler({"/r": [("a", True)], "/r/a": [("x", False)]}, fail="/r/a")
        self.addCleanup(f.close)
        code, _, _ = run("filer_files.py", f.url + "/r")
        self.assertNotEqual(code, 0)


if __name__ == "__main__":
    unittest.main()
