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

func TestMaintenanceLifecycleRealDirectoriesAndRefusalBeforeDeletion(t *testing.T) {
	body, err := workerFiles.ReadFile("worker/maintenance_lifecycle.py")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "maintenance_lifecycle.py")
	if err = os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", "-I", "-B", "-c", maintenanceLifecycleControls, path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("real lifecycle caller: %v\n%s", err, out)
	}
}

const maintenanceLifecycleControls = `
import importlib.util,sys,pathlib,tempfile,os,json
spec=importlib.util.spec_from_file_location('life',sys.argv[1]);m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
for case in ('clean','symlink','hardlink','replacement','open-race','evidence-symlink','lost-receipt','foreign-unit','foreign-owner','unknown-root','nonexclusive-receipt'):
    with tempfile.TemporaryDirectory() as parent:
        parent=pathlib.Path(parent);canary=parent/'adjacent-canary';canary.write_bytes(b'KEEP')
        roots=[];records=[];units=[];states={};calls=[]
        for role in ('vol1','vol2'):
            root=parent/role;root.mkdir(mode=0o700);(root/'volume').mkdir();(root/'volume/data.dat').write_bytes(b'ACKed live bytes')
            record=m.prepare(dict(root=str(root),run_id='run',role_run_id='run-'+role));roots.append(dict(path=str(root)));records.append(record)
            unit='sw-cloud-run-'+role+'-volume.service';units.append(dict(unit=unit,root=str(root)))
            states[unit]=dict(LoadState='loaded',MainPID='0',WorkingDirectory=str(root),FragmentPath='/run/systemd/transient/'+unit,ActiveState='inactive')
        def command(argv,timeout=10):
            calls.append(argv)
            if argv[:2]==['systemctl','show']:
                return ''.join(k+'='+v+'\n' for k,v in states[argv[2]].items())
            if argv[:2]==['systemctl','stop']:
                for name in argv[2:]:states[name].update(LoadState='not-found',MainPID='0',ActiveState='inactive')
                return ''
            if argv in (['ss','-Hltn'],['ss','-Hlxnp']):return ''
            raise AssertionError('unexpected platform command '+repr(argv))
        m.command=command
        root=pathlib.Path(roots[1]['path']);data=root/'volume/data.dat'
        original_open=m.os.open
        if case=='open-race':
            def raced_open(path,flags,*args,**kwargs):
                if str(path)==str(root) and not (parent/'replaced-root').exists():
                    root.rename(parent/'replaced-root');root.mkdir();(root/'.maintenance-owner.json').write_text(json.dumps(records[1]));os.chmod(root/'.maintenance-owner.json',0o600)
                return original_open(path,flags,*args,**kwargs)
            m.os.open=raced_open
        if case=='evidence-symlink':
            outside=parent/'outside';outside.mkdir();(outside/'config.json').write_bytes(b'FOREIGN CANARY')
            (root/'maintenance').symlink_to(outside,target_is_directory=True)
            try:m.preserve(dict(units=units,roots=roots,receipts=records,collection='owned'))
            except OSError:pass
            else:raise AssertionError('evidence followed a foreign parent symlink')
            assert (outside/'config.json').read_bytes()==b'FOREIGN CANARY'
        if case=='symlink':(root/'volume/link').symlink_to(canary)
        if case=='hardlink':os.link(canary,root/'volume/link')
        if case=='replacement':
            old=parent/'old-root';root.rename(old);root.mkdir();(root/'.maintenance-owner.json').write_text(json.dumps(records[1]));os.chmod(root/'.maintenance-owner.json',0o600)
        if case=='lost-receipt':records=[]
        if case=='foreign-unit':states[units[1]['unit']]['WorkingDirectory']='/unrelated/foreign'
        if case=='foreign-owner':
            if os.getuid()!=0:raise AssertionError('this control requires the ordinary WSL root test process')
            os.chown(data,65534,65534)
        if case=='unknown-root':records=records[:1]
        if case=='nonexclusive-receipt':os.link(root/'.maintenance-owner.json',parent/'receipt-link')
        c=dict(units=units,ports=[],roots=roots,receipts=records,remove=True)
        try:result=m.cleanup(c)
        except (ValueError,PermissionError):
            assert case!='clean',case
            assert pathlib.Path(roots[0]['path'],'volume/data.dat').read_bytes()==b'ACKed live bytes','partial deletion before all roots admitted'
            assert all(pathlib.Path(x['path']).exists() for x in roots),'unknown state deleted a root'
        else:
            assert case=='clean',('false cleanup',case,result)
            assert result['status']=='CLEAN' and len(result['removed'])==2
            assert all(not pathlib.Path(x['path']).exists() for x in roots),'independent root check failed'
        assert canary.read_bytes()==b'KEEP','adjacent data touched'
        if case=='foreign-unit':assert not any(x[:2]==['systemctl','stop'] for x in calls),'foreign unit was stopped'
        m.os.open=original_open
print('real directory cleanup: 1 positive + 10 fail-closed controls PASS')
`
