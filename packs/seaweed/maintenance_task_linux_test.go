//go:build linux

package seaweed

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMaintenanceLeafRealProcessAndImportOrigin(t *testing.T) {
	dir := t.TempDir()
	body, err := workerFiles.ReadFile("worker/maintenance.py")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "maintenance.py")
	if err = os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "cc", "-x", "c", "-o", filepath.Join(dir, "weed-fixture"), "-")
	cmd.Stdin = strings.NewReader(maintenanceProductFixture)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile process fixture: %v %s", err, out)
	}
	cmd = exec.CommandContext(ctx, "python3", "-I", "-B", "-c", maintenanceTaskControls, path, filepath.Join(dir, "weed-fixture"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("real task controls: %v\n%s", err, out)
	}
}

const maintenanceProductFixture = `
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
int main(int argc,char **argv){
 if(argc!=3)return 64;
 if(getenv("MAINT_LOCK_PROBE")){
  char input[4096];size_t n=fread(input,1,sizeof(input)-1,stdin);input[n]=0;
  if(!strstr(input,"lock\n")||!strstr(input,"volume.list -volumeId=7 -collectionPattern=owned -v=5\n")||!strstr(input,"unlock\n"))return 65;
  printf("          volume Id:7, Size:1, ReplicaPlacement:001, Collection:%s, Version:3,\n",getenv("MAINT_FOREIGN")?"foreign":"owned");return 0;
 }
 if(getenv("MAINT_FAST"))return 0;
 if(getenv("MAINT_REFUSE")){puts("error: rpc error: code = Unavailable desc = fixture");return 73;}
 puts("copying volume 7 from source");puts("source => target volume 7 processed 1.00 MiB");fflush(stdout);
 while(access(getenv("MAINT_ADVANCE"),F_OK))usleep(10000);
 puts("source => target volume 7 processed 2.00 MiB");fflush(stdout);
 while(access(getenv("MAINT_FINISH"),F_OK))usleep(10000);
 puts("moved volume 7 from source to target");return 0;
}
`

const maintenanceTaskControls = `
import importlib.util,sys,pathlib,tempfile,os,json,time,hashlib,subprocess,shutil,signal
spec=importlib.util.spec_from_file_location('maintenance',sys.argv[1]);m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
def wait(check):
    end=time.monotonic()+5
    while time.monotonic()<end:
        if check():return
        time.sleep(.01)
    raise AssertionError('process fixture deadline')
for mode in ('success','refusal','fast-noop'):
 with tempfile.TemporaryDirectory() as temporary:
    root=pathlib.Path(temporary);(root/'worker').mkdir();helper=root/'worker/maintenance.py';helper.write_bytes(pathlib.Path(sys.argv[1]).read_bytes())
    weed=root/'weed';shutil.copyfile(sys.argv[2],weed);weed.chmod(0o700)
    unit='sw-cloud-run-meta-maintenance-task.service';pidfile=root/'unit-pid';bin=root/'bin';bin.mkdir()
    shim=bin/'systemctl';shim.write_text('#!'+sys.executable+'\nimport os,sys,pathlib\nassert sys.argv[1:]==["show","-p","MainPID","--value",os.environ["MAINT_UNIT"]]\nprint(pathlib.Path(os.environ["MAINT_PID_FILE"]).read_text())\n');shim.chmod(0o700)
    c=dict(root=str(root),run_id='run',task='volume.balance',collection='owned',weed=str(weed),master='source',
           config_sha256='a'*64,weed_sha256=hashlib.sha256(weed.read_bytes()).hexdigest(),helper_sha256=hashlib.sha256(helper.read_bytes()).hexdigest(),task_unit=unit)
    oldenv=os.environ.copy();os.environ.update(PATH=str(bin)+':'+oldenv['PATH'],MAINT_UNIT=unit,MAINT_PID_FILE=str(pidfile),MAINT_ADVANCE=str(root/'advance'),MAINT_FINISH=str(root/'finish'))
    if mode=='refusal':os.environ['MAINT_REFUSE']='1'
    if mode=='fast-noop':os.environ['MAINT_FAST']='1'
    subprocess.run([sys.executable,'-I',str(helper),json.dumps(dict(c,stage='init'))],check=True,stdout=subprocess.PIPE,timeout=5)
    m.write(root/'maintenance/manifest.json',dict(run_id='run',task=c['task'],collection='owned',vid='7',objects=[]),True)
    process=subprocess.Popen([sys.executable,'-I',str(helper),json.dumps(dict(c,stage='task'))],stdout=subprocess.PIPE,stderr=subprocess.PIPE)
    pidfile.write_text(str(process.pid))
    try:
        if mode=='success':
            progress=root/'maintenance/progress.json';wait(lambda:progress.exists() and m.strict(progress.read_bytes())['state']=='in_flight')
            a=m.sample(dict(c,stage='sample'),root/'maintenance')
            assert a['alive'] and a['progress']['bytes']==1048576 and a['progress']['source']=='source',a
            pidfile.write_text(str(process.pid+1))
            try:m.sample(dict(c,stage='sample'),root/'maintenance')
            except ValueError as e:assert 'foreign task MainPID' in str(e)
            else:raise AssertionError('foreign unit MainPID was accepted')
            pidfile.write_text(str(process.pid));(root/'advance').touch();wait(lambda:m.strict(progress.read_bytes())['bytes']==2097152)
            b=m.sample(dict(c,stage='sample'),root/'maintenance');assert b['progress']['operation']==a['progress']['operation'] and b['progress']['bytes']>a['progress']['bytes']
            original=helper.read_bytes();helper.write_bytes(original+b'\n# changed\n')
            try:m.sample(dict(c,stage='sample'),root/'maintenance')
            except ValueError as e:assert 'helper changed' in str(e)
            else:raise AssertionError('foreign helper bytes accepted')
            helper.write_bytes(original)
            weed.rename(root/'old-weed');shutil.copyfile(sys.argv[2],weed);weed.chmod(0o700)
            rejected=m.sample(dict(c,stage='sample'),root/'maintenance');assert rejected['progress'] is None,'worker imported a different current executable'
            (root/'finish').touch()
        stdout,stderr=process.communicate(timeout=5);assert process.returncode==0,(stdout,stderr)
        result=m.strict(stdout)
        if mode=='refusal':assert result['terminal']=='refused' and result['refusal']=='RPC_UNAVAILABLE' and result['exit']==73,result
        else:assert result['terminal']=='succeeded' and result['exit']==0,result
        assert not m.sample(dict(c,stage='sample'),root/'maintenance')['alive'],'dead process remained a work witness'
        if mode=='success':
            os.environ['MAINT_LOCK_PROBE']='1';os.environ['MAINT_FOREIGN']='1'
            try:m.lock_check(c,root/'maintenance')
            except ValueError:pass
            else:raise AssertionError('exit zero with foreign namespace passed the lock proof')
            del os.environ['MAINT_FOREIGN']
            proof=m.lock_check(c,root/'maintenance');assert proof['lock_usable'] and proof['vid']=='7' and proof['collection']=='owned',proof
    finally:
        if process.poll() is None:process.kill();process.wait(timeout=2)
        child=root/'maintenance/child.json'
        if child.exists():
            identity=m.strict(child.read_bytes())
            try:
                observed=m.proc_identity(identity['pid'])
                if all(observed[k]==identity[k] for k in ('pid','start','boot')):os.killpg(identity['pid'],signal.SIGKILL)
            except (FileNotFoundError,ProcessLookupError):pass
        os.environ.clear();os.environ.update(oldenv)
print('real task: success/refusal/fast-noop + MainPID/helper/executable negatives PASS')
`
