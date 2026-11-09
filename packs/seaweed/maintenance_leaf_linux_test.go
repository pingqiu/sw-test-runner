//go:build linux

package seaweed

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestMaintenanceEmbeddedLeafActualProgressAndHTTPBounds(t *testing.T) {
	leaf, err := workerFiles.ReadFile("worker/maintenance.py")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "maintenance.py")
	if err = os.WriteFile(path, leaf, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", "-I", "-B", "-c", maintenanceLeafControls, path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("embedded caller controls: %v\n%s", err, out)
	}
}

const maintenanceLeafControls = `
import importlib.util,sys,pathlib,tempfile,threading,time,http.server,signal
spec=importlib.util.spec_from_file_location('maintenance',sys.argv[1]);m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
s=dict(phase=None,operation=0)
for line in ['copying volume 7 from source','replicating volume 7 001 from source','  moving hdd volume maint-2026_7 source => target','volume 8 processed 1.00 MiB','volume 7 processed NaN MiB','volume 7 processed 0 B']:
    assert m.transfer_progress(line,'7',s,10)['state']=='idle',line
p=m.transfer_progress('source => target volume 7 processed 1.00 MiB','7',s,11)
assert p==dict(state='in_flight',vid='7',operation='1',bytes=1048576,begin=11,source='source',target='target'),p
p=m.transfer_progress('copying volume 7 from source','7',s,20)
assert p['state']=='idle','same VID started a second copy but kept the old operation'
p=m.transfer_progress('source => target volume 7 processed 2.00 MiB','7',s,30)
assert p['operation']=='2' and p['begin']==30,p
assert m.transfer_progress('copying volume 8 from source','7',s,31)['state']=='idle'
with tempfile.TemporaryDirectory() as root:
    p=pathlib.Path(root)/'atomic.json';m.write(p,dict(before=1),True)
    try:m.write(p,dict(before=2),True)
    except FileExistsError:pass
    else:raise AssertionError('exclusive reply overwritten')
    assert m.strict(p.read_bytes())==dict(before=1)
    target=pathlib.Path(root)/'zero';target.mkdir()
    m.write(target/'manifest.json',dict(run_id='own',collection='maint-own',vid='7'),True)
    # A real leaf operation with a zero-candidate product reply must retain
    # that reply and return a distinct noncoverage result, never execute work.
    original=m.api
    m.api=lambda *args,**kw:dict(count=0,proposals=[],completed=dict(success=True),detector_worker_id='worker')
    try:
        result=m.detect(dict(run_id='own',collection='maint-own',task='ec_repair',admin='http://127.0.0.1:1',worker_id='worker'),target)
        assert result==dict(count=0,status='NO_EC_CANDIDATES'),result
        assert not (target/'proposal.json').exists()
        assert m.strict((target/'detection.json').read_bytes())['count']==0
    finally:m.api=original
class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self,*args):pass
    def do_GET(self):
        if self.path=='/missing':
            self.send_response(404);self.end_headers();self.wfile.write(b'absent');return
        self.send_response(200);self.send_header('Content-Length','100');self.end_headers()
        try:
            for _ in range(100):self.wfile.write(b'x');self.wfile.flush();time.sleep(.03)
        except (BrokenPipeError,ConnectionResetError):pass
server=http.server.ThreadingHTTPServer(('127.0.0.1',0),Handler);server.daemon_threads=True
thread=threading.Thread(target=server.serve_forever,daemon=True);thread.start();url='http://127.0.0.1:'+str(server.server_port)
assert m.request(url+'/missing',timeout=1)==(404,b'absent')
started=time.monotonic()
try:m.request(url+'/trickle',timeout=.15)
except TimeoutError:pass
else:raise AssertionError('trickle escaped whole-operation deadline')
assert time.monotonic()-started<1,'deadline was applied only per read'
assert signal.getitimer(signal.ITIMER_REAL)[0]==0
server.shutdown();server.server_close();thread.join()
print('embedded leaf controls PASS')
`
