package seaweed

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSWCtlDoctorUsesReadOnlyCLIArguments(t *testing.T) {
	for _, real := range []bool{false, true} {
		t.Run(fmt.Sprint(real), func(t *testing.T) {
			raw, c, _, calls := swctlAdapterFixture(t, "success")
			program, err := os.ReadFile(c.SWCtl.Binary.Path)
			if err != nil {
				t.Fatal(err)
			}
			gate := "if action=='doctor' and '--state-dir' in sys.argv:\n print(json.dumps({'schema':'swctl.result/v1','command':'doctor','status':'USAGE','items':[]}));sys.exit(64)\n"
			receipt := filepath.Join(t.TempDir(), "doctor.exit")
			if real {
				binary := os.Getenv("SWCTL_PLAN_BINARY")
				if binary == "" {
					t.Skip("requires real mono CLI; strict ordinary caller still runs")
				}
				gate = "if action=='doctor':\n import subprocess\n p=subprocess.run([" + fmt.Sprintf("%q", binary) + ",*sys.argv[1:]],capture_output=True)\n pathlib.Path(" + fmt.Sprintf("%q", receipt) + ").write_text(str(p.returncode)+'\\n'+p.stdout.decode())\n sys.stdout.buffer.write(p.stdout);sys.exit(p.returncode)\n"
				bin := t.TempDir()
				os.WriteFile(filepath.Join(bin, "ssh-keyscan"), []byte("#!/bin/sh\nexit 0\n"), 0700)
				os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nexit 99\n"), 0700)
				t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			}
			program = []byte(strings.Replace(string(program), "if action=='doctor':", gate+"if action=='doctor':", 1))
			if err := os.WriteFile(c.SWCtl.Binary.Path, program, 0700); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(program)
			c.SWCtl.Binary.SHA256 = fmt.Sprintf("%x", digest)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_, err = RunCloudHTTP(ctx, raw, c)
			if real {
				observed, e := os.ReadFile(receipt)
				t.Logf("real CLI doctor: %s", observed)
				if e != nil || !strings.HasPrefix(string(observed), "65\n") || !strings.Contains(string(observed), "HOST_KEY_MISMATCH") {
					t.Fatalf("real CLI parsing: %v %s", e, observed)
				}
				b, _ := os.ReadFile(calls)
				if !strings.Contains(string(b), `"Action": "uninstall"`) {
					t.Fatal("refusal skipped teardown")
				}
				if err == nil {
					t.Fatal("host-key refusal accepted")
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
