package seaweed

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pingqiu/sw-test-runner/inventory"
)

// EC is one local HTTP phase; its additional listeners are checked by the
// existing direct postflight, without changing the shared inventory schema.
func ecDirectPorts(h inventory.Host) ([]string, error) {
	ports, seen := []string{}, map[int]bool{}
	add := func(p int) error {
		if p < 1 || p > 65535 || seen[p] {
			return errors.New("EC listener range/collision")
		}
		seen[p] = true
		ports = append(ports, strconv.Itoa(p))
		return nil
	}
	for _, role := range []string{"master_http", "master_grpc", "filer_http", "filer_grpc", "s3_http"} {
		if e := add(servicePort(h, role)); e != nil {
			return nil, e
		}
	}
	if e := add(servicePort(h, "s3_http") + 10000); e != nil {
		return nil, e
	}
	for i := 0; i < 14; i++ {
		for _, role := range []string{"volume_http", "volume_grpc"} {
			if e := add(servicePort(h, role) + i); e != nil {
				return nil, e
			}
		}
	}
	ip := net.ParseIP(h.Data.Address)
	if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
		return nil, errors.New("EC holders need a non-loopback host address")
	}
	return ports, nil
}

func runDirectEC(d inventory.Document, h inventory.Host, c CloudConfig, run func(string, string) (string, error)) (output string, err error) {
	root, q := h.RunRoot, cloudQuote
	js := func(v any) string { b, _ := json.Marshal(v); return q(string(b)) }
	leaf := func(name string, v any) string { return "python3 -I " + q(root+"/worker/"+name) + " " + js(v) }
	master := fmt.Sprintf("127.0.0.1:%d", servicePort(h, "master_http"))
	url := func(role string) string { return fmt.Sprintf("http://127.0.0.1:%d", servicePort(h, role)) }
	manifest := root + "/manifest.json"
	output = filepath.Join(c.Output, "ec-stage1.json")
	report := map[string]any{"s1": "NOT_RUN", "s2": "NOT_COVERED", "exposure": "NOT_OBSERVED", "implementation": "go", "rust": "NOT_RUN", "rdma": "NOT_RUN"}
	defer func() {
		b, e := json.MarshalIndent(report, "", "  ")
		if e == nil {
			e = os.WriteFile(output, b, 0600)
		}
		err = errors.Join(err, e)
	}()
	if _, err = run("ec-configure", "chmod 0700 "+q(root)+"; chmod 0755 "+q(root+"/weed")+"; printf '%s\\n' '[master.volume_growth]' 'copy_1=1' 'copy_2=1' 'copy_3=1' 'copy_other=1' 'threshold=0.9' > "+q(root+"/master.toml")); err != nil {
		return
	}
	holders := map[string]string{}
	services := []string{"master", "filer", "s3"}
	args := map[string][]string{
		"master": {"-config_dir=" + root, "master", "-ip=127.0.0.1", "-ip.bind=127.0.0.1", fmt.Sprint("-port=", servicePort(h, "master_http")), fmt.Sprint("-port.grpc=", servicePort(h, "master_grpc")), "-mdir=" + root + "/master", "-peers=none", "-volumeSizeLimitMB=64"},
		"filer":  {"-config_dir=" + root, "filer", "-ip=127.0.0.1", "-ip.bind=127.0.0.1", fmt.Sprint("-port=", servicePort(h, "filer_http")), fmt.Sprint("-port.grpc=", servicePort(h, "filer_grpc")), "-master=" + master, "-localSocket=" + root + "/filer.sock"},
		"s3":     {"-config_dir=" + root, "s3", "-ip.bind=127.0.0.1", fmt.Sprint("-port=", servicePort(h, "s3_http")), fmt.Sprint("-port.grpc=", servicePort(h, "s3_http")+10000), "-filer=" + strings.TrimPrefix(url("filer_http"), "http://"), "-localSocket=" + root + "/s3.sock"},
	}
	urls := []string{url("master_http") + "/dir/status", url("filer_http") + "/", url("s3_http") + "/"}
	for i := 0; i < 14; i++ {
		role := fmt.Sprint("h", i)
		services = append(services, role)
		holders[role] = root + "/" + role
		args[role] = []string{"-config_dir=" + root, "volume", "-ip=" + h.Data.Address, "-ip.bind=0.0.0.0", fmt.Sprint("-port=", servicePort(h, "volume_http")+i), fmt.Sprint("-port.grpc=", servicePort(h, "volume_grpc")+i), "-master=" + master, "-dir=" + root + "/" + role, "-max=3", "-preStopSeconds=0"}
		urls = append(urls, fmt.Sprintf("http://127.0.0.1:%d/status", servicePort(h, "volume_http")+i))
	}
	for _, role := range services {
		cmd := leaf("ec_service.py", map[string]any{"root": root, "role": role, "exe": root + "/weed", "sha256": c.Files["weed"].SHA256, "args": args[role]})
		// The helper execs weed in-place, so systemd's MainPID is the captured holder.
		if _, err = run("start-"+role, cloudStart(d, root, role, "--property=User="+q(h.Management.User)+" --property=UMask=0077 "+cmd)); err != nil {
			return
		}
	}
	raw, e := run("ec-ready", leaf("ec_setup.py", map[string]any{"root": root, "stage": "ready", "urls": urls}))
	if e != nil {
		err = e
		return
	}
	var ready struct{ Ready bool }
	if json.Unmarshal([]byte(raw), &ready) != nil || !ready.Ready {
		err = errors.New("EC readiness terminal missing")
		return
	}
	config := map[string]any{"root": root, "run_id": d.RunID, "keys": 64, "s3": url("s3_http"), "filer_http": url("filer_http"), "volume": url("volume_http"), "observe_errors": c.ECUnderQuorum}
	worker := func(stage, route string, plant bool) string {
		cmd := "python3 -I " + q(root+"/worker/ec_chaos.py") + " --config " + js(config) + " --stage " + stage + " --manifest " + q(manifest) + " --journal " + q(root+"/"+stage+"-"+route+".jsonl")
		if route != "" {
			cmd += " --route " + route
		}
		if plant {
			cmd += " --plant-wrong-byte"
		}
		return cmd
	}
	raw, err = run("ec-seed", worker("seed", "", false))
	if err != nil {
		return
	}
	var seed EC1aManifest
	if json.Unmarshal([]byte(raw), &seed) != nil || seed.RunID != d.RunID || len(seed.Objects) != 64 {
		err = errors.New("EC seed terminal invalid")
		return
	}
	for _, o := range seed.Objects {
		if o.Length != 262144 || hash(ecPayload(d.RunID, o.Key)) != o.SHA256 {
			err = errors.New("EC seed payload differs")
			return
		}
	}
	if _, err = run("ec-encode", leaf("ec_setup.py", map[string]any{"root": root, "stage": "encode", "weed": root + "/weed", "master": master, "manifest": manifest})); err != nil {
		return
	}
	raw, err = run("ec-capture", leaf("ec_capture.py", map[string]any{"root": root, "run_id": d.RunID, "holders": holders, "volume_exe": root + "/weed", "volume_sha256": c.Files["weed"].SHA256, "under_quorum": c.ECUnderQuorum})+" "+q(manifest))
	if err != nil {
		return
	}
	var snap ecSnapshot
	if err = json.Unmarshal([]byte(raw), &snap); err != nil {
		return
	}
	boot, e := run("ec-boot", "cat /proc/sys/kernel/random/boot_id")
	if e != nil {
		err = e
		return
	}
	if err = ecValidateSnapshot(seed, snap, services, strings.TrimSpace(boot)); err != nil {
		return
	}
	report["capture"], report["ack"] = snap, 64
	read := func(name, route string, plant bool) error { // Every raw terminal is retained by cloudStep, including failures.
		cmd := strings.Replace(worker("read", route, plant), q(root+"/read-"+route+".jsonl"), q(root+"/"+name+".jsonl"), 1)
		raw, e := run(name, cmd)
		var body ecReadBody
		if decodeErr := json.Unmarshal([]byte(raw), &body); decodeErr != nil {
			return errors.Join(e, decodeErr)
		}
		if c.ECUnderQuorum && !plant {
			return errors.Join(e, ecAssessUnderQuorum(body, route, snap, report["holders_killed"] == 5))
		}
		return errors.Join(e, ecAssessRead(body, route, snap.Manifest, plant))
	}
	if err = read("ec-plant", "s3", true); err != nil {
		return
	}
	report["wrong_byte_control"] = "WRONG_BYTE_REJECTED"
	for _, route := range []string{"s3", "volume"} {
		if err = read("ec-s1-"+route, route, false); err != nil {
			return
		}
	}
	report["s1"] = "PASS"
	victims := []string{"h0", "h1", "h2", "h3"}
	if c.ECUnderQuorum {
		victims = nil
		for _, id := range []int{10, 11, 12, 13, 0} {
			for _, shard := range snap.Shards {
				if shard.Shard == id {
					victims = append(victims, shard.Job)
				}
			}
		}
	}
	dead := map[string]bool{}
	for i, victim := range victims {
		unit := q("sw-cloud-" + d.RunID + "-" + victim)
		var pid int
		var start uint64
		for _, v := range snap.Holders {
			if v.Job == victim {
				pid, start = v.PID, v.Start
			}
		}
		// Recheck the frozen generation, then signal only this unit's main process.
		check := fmt.Sprintf("test \"$(sudo -n systemctl show -p MainPID --value %s)\" = %d; test \"$(cat /proc/sys/kernel/random/boot_id)\" = %s; test \"$(awk '{print $22}' /proc/%d/stat)\" = %d; sudo -n systemctl kill --kill-whom=main --signal=KILL %s; until [ \"$(sudo -n systemctl show -p MainPID --value %s)\" = 0 ]; do sleep .1; done; test \"$(sudo -n systemctl show -p ExecMainCode --value %s)\" = 2; test \"$(sudo -n systemctl show -p ExecMainStatus --value %s)\" = 9", unit, pid, q(strings.TrimSpace(boot)), pid, start, unit, unit, unit, unit)
		if _, err = run("ec-kill-"+victim, check); err != nil {
			return
		}
		report["holders_killed"] = i + 1
		dead[victim] = true
		port := servicePort(h, "volume_http") + i + 1
		if c.ECUnderQuorum {
			for _, holder := range snap.Holders {
				if !dead[holder.Job] {
					n, _ := strconv.Atoi(strings.TrimPrefix(holder.Job, "h"))
					port = servicePort(h, "volume_http") + n
					break
				}
			}
		}
		config["volume"] = fmt.Sprintf("http://127.0.0.1:%d", port)
		for _, route := range []string{"s3", "volume"} {
			if err = read(fmt.Sprintf("ec-s2-%d-%s", i+1, route), route, false); err != nil {
				return
			}
		}
	}
	report["mandatory_bytes_deadlines"], report["exact"], report["wrong"] = "PASS", 640, 0
	if c.ECUnderQuorum {
		report["s2a"], report["survivors"] = "PASS", 9
		report["stage1_exact"] = 640
		delete(report, "exact")
	}
	return
}
