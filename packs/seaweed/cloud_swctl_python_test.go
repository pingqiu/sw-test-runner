package seaweed

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pingqiu/sw-test-runner/inventory"
)

// Real interpreter/import boundary; the extension deliberately terminates before
// KV I/O. Existing caller/oracle tests cover the workload, not this fixture.
type swctlPythonHost struct {
	*swctlAdapterHost
	logical, physical, foreign string
	poison                     bool
}

func (h *swctlPythonHost) Upload(local, remote string) error {
	if err := h.swctlAdapterHost.Upload(local, remote); err != nil {
		return err
	}
	b, err := os.ReadFile(local)
	if err != nil {
		return err
	}
	return os.WriteFile(remote, b, 0600)
}
func (h *swctlPythonHost) Run(ctx context.Context, command string) (string, string, int, error) {
	if strings.Contains(command, "systemctl list-units") {
		return h.swctlAdapterHost.Run(ctx, command)
	}
	h.commands = append(h.commands, command)
	if strings.Contains(command, "kv_smoke.py") && strings.Contains(command, "--config") {
		if _, err := os.Stat(h.traffic + ".window"); err == nil {
			os.WriteFile(h.traffic, []byte("traffic"), 0600)
		}
	}
	actual := strings.ReplaceAll(command, h.logical, h.physical)
	q := exec.CommandContext(ctx, "sh", "-c", actual)
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "PYTHON") {
			q.Env = append(q.Env, v)
		}
	}
	if h.poison {
		q.Env = append(q.Env, "PYTHONPATH="+h.foreign, "PYTHONHOME="+h.foreign+"/not-a-python-home")
	}
	var out, stderr strings.Builder
	q.Stdout = &out
	q.Stderr = &stderr
	err := q.Run()
	code := 0
	if err != nil {
		code = -1
		if e, ok := err.(*exec.ExitError); ok {
			code = e.ExitCode()
		}
	}
	text := out.String()
	if strings.Contains(command, "sysconfig.get_path") {
		text = strings.ReplaceAll(text, h.physical, h.logical)
	}
	return text, stderr.String(), code, err
}
func TestSWCtlSmokeUsesIsolatedOverlayPython(t *testing.T) {
	for _, mode := range []string{"valid", "foreign-pythonpath", "missing-overlay", "outside-overlay"} {
		t.Run(mode, func(t *testing.T) {
			raw, c, host, calls := swctlAdapterFixture(t, "success")
			root := t.TempDir()
			venv := filepath.Join(root, "venv")
			if b, err := exec.Command("python3", "-I", "-B", "-m", "venv", "--without-pip", venv).CombinedOutput(); err != nil {
				t.Fatalf("venv: %v %s", err, b)
			}
			python := filepath.Join(venv, "bin/python")
			b, err := exec.Command(python, "-I", "-B", "-c", `import sysconfig,json;print(json.dumps([sysconfig.get_path('platlib'),sysconfig.get_path('include'),sysconfig.get_config_var('EXT_SUFFIX')]))`).Output()
			if err != nil {
				t.Fatal(err)
			}
			var paths []string
			if json.Unmarshal(b, &paths) != nil || len(paths) != 3 {
				t.Fatalf("python layout: %s", b)
			}
			pkg := filepath.Join(paths[0], "seaweedkv_native")
			os.MkdirAll(pkg, 0700)
			os.WriteFile(filepath.Join(pkg, "__init__.py"), nil, 0600)
			marker := filepath.Join(root, "import.json")
			fixture := fmt.Sprintf(`import json,os,sys,pathlib
from . import _native
def launch():
 frame=sys._getframe(1)
 record=dict(origin=_native.__file__,isolated=sys.flags.isolated,bytecode=sys.dont_write_bytecode,pythonpath=os.environ.get('PYTHONPATH'),pythonhome=os.environ.get('PYTHONHOME'),native_dir=frame.f_locals['c']['native_dir'])
 pathlib.Path(%q).write_text(json.dumps(record))
 assert record['isolated']==1 and record['bytecode'] and record['pythonpath'] is None and record['pythonhome'] is None
 assert record['native_dir']==str(pathlib.Path(__file__).parent.parent)
 print(json.dumps(dict(status='PASS',ack=16,exact=16,wrong=0)))
 raise SystemExit(0)
`, marker)
			os.WriteFile(filepath.Join(pkg, "fixture.py"), []byte(fixture), 0600)
			source := filepath.Join(root, "native.c")
			os.WriteFile(source, []byte(`#include <Python.h>
static PyObject* make(PyObject*s,PyObject*a){PyObject*m=PyImport_ImportModule("seaweedkv_native.fixture");if(!m)return NULL;PyObject*r=PyObject_CallMethod(m,"launch",NULL);Py_DECREF(m);return r;}
static PyMethodDef methods[]={{"SeaweedKVConnector",make,METH_VARARGS,NULL},{NULL,NULL,0,NULL}};
static struct PyModuleDef module={PyModuleDef_HEAD_INIT,"_native",NULL,-1,methods};
PyMODINIT_FUNC PyInit__native(void){return PyModule_Create(&module);}
`), 0600)
			extension := filepath.Join(pkg, "_native"+paths[2])
			if b, err := exec.Command("cc", "-shared", "-fPIC", "-I"+paths[1], source, "-o", extension).CombinedOutput(); err != nil {
				t.Fatalf("extension: %v %s", err, b)
			}
			foreign := filepath.Join(root, "foreign")
			os.MkdirAll(filepath.Join(foreign, "seaweedkv_native"), 0700)
			trap := filepath.Join(root, "FOREIGN_IMPORTED")
			os.WriteFile(filepath.Join(foreign, "seaweedkv_native/__init__.py"), []byte(fmt.Sprintf("import pathlib\npathlib.Path(%q).write_text('foreign')\n", trap)), 0600)
			os.WriteFile(filepath.Join(foreign, "seaweedkv_native/_native.py"), []byte("raise RuntimeError('FOREIGN_NATIVE')\n"), 0600)
			if mode == "missing-overlay" {
				os.Remove(extension)
			}
			if mode == "outside-overlay" {
				moved := filepath.Join(foreign, filepath.Base(extension))
				if err := os.Rename(extension, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(moved, extension); err != nil {
					t.Fatal(err)
				}
			}
			profile, _, err := bindSWCtl(mustDecodeSWCtlInventory(t, raw), c)
			if err != nil {
				t.Fatal(err)
			}
			h := &swctlPythonHost{swctlAdapterHost: host, logical: "/opt/swctl/" + profile.Name + "/releases/" + profile.Release.SHA + "/venv", physical: venv, foreign: foreign, poison: mode != "valid"}
			c.swctlNodes = func(inventory.Host) swctlSmokeNode { return h }
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_, err = RunCloudHTTP(ctx, raw, c)
			observed, _ := os.ReadFile(marker)
			t.Logf("real Python import: %s", observed)
			if (err == nil) != (mode == "valid" || mode == "foreign-pythonpath") {
				t.Fatalf("%s: %v", mode, err)
			}
			if _, err := os.Stat(trap); !os.IsNotExist(err) {
				t.Fatal("foreign PYTHONPATH module imported")
			}
			if mode == "valid" || mode == "foreign-pythonpath" {
				b, err := os.ReadFile(marker)
				if err != nil {
					t.Fatal(err)
				}
				var got struct{ Origin string }
				json.Unmarshal(b, &got)
				if got.Origin != extension {
					t.Fatalf("foreign origin: %s", b)
				}
			}
			b, _ = os.ReadFile(calls)
			if !strings.Contains(string(b), `"Action": "uninstall"`) {
				t.Fatal("teardown missing")
			}
		})
	}
}
func mustDecodeSWCtlInventory(t *testing.T, raw []byte) inventory.Document {
	t.Helper()
	d, err := inventory.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	return *d
}
