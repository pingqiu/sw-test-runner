package seaweed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/pingqiu/sw-test-runner/inventory"
)

// Process roles are an execution view, never a physical/resolved inventory.
// Co-located copies prove process tolerance, not host tolerance.
func processPlacement(d inventory.Document) (inventory.Document, map[string]inventory.Document, map[string]string, error) {
	view := d
	view.Hosts = nil
	docs, physical := map[string]inventory.Document{}, map[string]string{}
	used := map[string]bool{}
	for _, h := range d.Hosts {
		k := fmt.Sprintf("%s:%d", h.Management.Address, h.Management.Port)
		if used[k] || h.RunRoot != cloudRunRoot(d, h) {
			return view, nil, nil, errors.New("duplicate physical host or changed root")
		}
		used[k] = true
	}
	for i, id := range append([]string{"meta", "vol1", "vol2", "vol3", "vol4", "vol5", "vol6"}, chaosClients...) {
		index := 0
		if len(d.Hosts) == 2 && (id == "vol4" || id == "vol5" || id == "vol6" || id == "client2") {
			index = 1
		}
		h := d.Hosts[index]
		physical[id] = h.ID
		h.ID = id
		h.Services = nil
		h.Roles = []string{"client"}
		add := func(name string, offset int) {
			p := servicePort(d.Hosts[index], name)
			if p == 0 {
				offset = 0
			}
			h.Services = append(h.Services, inventory.Service{Name: name, Port: p + offset})
		}
		if id == "meta" {
			h.Roles = []string{"master", "filer", "s3"}
			for _, n := range []string{"master_http", "master_grpc", "filer_http", "filer_grpc", "s3_http"} {
				add(n, 0)
			}
			h.Services = append(h.Services, inventory.Service{Name: "s3_grpc", Port: servicePort(h, "s3_http") + 10000})
		} else if i >= 1 && i <= 6 {
			h.Roles = []string{"volume"}
			add("volume_http", i-1)
			add("volume_grpc", i-1)
		}
		rd := d
		rd.RunID = d.RunID + "-" + id
		h.RunRoot = cloudRunRoot(rd, h)
		docs[id] = rd
		view.Hosts = append(view.Hosts, h)
	}
	ports := map[string]bool{}
	for _, h := range view.Hosts {
		for _, s := range h.Services {
			k := h.Management.Address + ":" + strconv.Itoa(s.Port)
			if s.Port < 1 || s.Port > 65535 || ports[k] {
				return view, nil, nil, errors.New("process listener range/collision")
			}
			ports[k] = true
		}
	}
	return view, docs, physical, nil
}

func runDirectProcess(ctx context.Context, d inventory.Document, c CloudConfig) (result string, failure error) {
	if d.Target != "onprem" || d.State != "requested" || d.Transport != "http" || len(d.Hosts) < 1 || len(d.Hosts) > 2 || len(c.ClusterChaosCases) != 1 || c.Replication != "001" || c.Chaos || c.ChaosSet != "" || c.ECStage1 || c.ECUnderQuorum || c.ECVolumeImpl != "" || c.MasterWrites || c.KVSoak || c.KVLoad || c.ReadCase != "" {
		return "", errors.New("cluster_chaos_cases requires one process case, replication 001, one/two onprem HTTP hosts and no other phase")
	}
	name := c.ClusterChaosCases[0]
	var selected *chaosCase
	for _, x := range chaosCases {
		if x.Name == name {
			v := x
			selected = &v
		}
	}
	var mixed *chaos2Case
	for _, set := range chaos2Sets {
		for _, x := range set {
			if x.Name == name && x.Kind != "partition" && x.Kind != "netem" {
				v := x
				mixed = &v
			}
		}
	}
	if selected == nil && mixed == nil {
		return "", fmt.Errorf("%s NOT_RUN on direct: only process cases implemented; C3/C6 remain cloud-only", name)
	}
	if e := c.CheckFiles(); e != nil {
		return "", e
	}
	view, docs, physical, e := processPlacement(d)
	if e != nil {
		return "", e
	}
	pins := processPins(view, c)
	if e = os.WriteFile(filepath.Join(c.Output, "process-placement.json"), processJSON(map[string]any{"physical_inventory": d, "roles": view.Hosts, "physical_host": physical, "host_tolerance": "NOT_CLAIMED"}), 0600); e != nil {
		return "", e
	}
	phase := func(label string, h inventory.Host) error {
		hc := c
		hc.HostKey = pins[h.ID]
		hc.Output = filepath.Join(c.Output, label, h.ID)
		hc.ChaosSet = "process" // upload the existing mixed-load leaf
		if e := os.MkdirAll(hc.Output, 0700); e != nil {
			return e
		}
		rd := docs[h.ID]
		rd.Hosts = view.Hosts
		_, e := runCloudNode(ctx, rd, h, hc, label)
		return e
	}
	hosts, nodes, release, e := chaosConnect(ctx, view, CloudConfig{SSHKey: c.SSHKey, HostKeys: pins}, append([]string{"meta", "vol1", "vol2", "vol3", "vol4", "vol5", "vol6"}, chaosClients...))
	if e != nil {
		return "", e
	}
	defer release()
	r := &chaosRun{dir: c.Output, rep: "001", nodes: nodes, docs: docs, gens: &chaosGens{gens: map[string]int{}, witness: map[string]string{}}}
	for _, h := range view.Hosts {
		if _, e = r.step(ctx, h.ID, "fresh-"+h.ID, 10, "test ! -e "+cloudQuote(h.RunRoot)); e != nil {
			return "", e
		}
	}
	// Register teardown for every role before the first possible service start.
	defer func() {
		for _, h := range view.Hosts {
			hc := c
			hc.HostKey = pins[h.ID]
			hc.Output = filepath.Join(c.Output, "cleanup", h.ID)
			failure = errors.Join(failure, os.MkdirAll(hc.Output, 0700), directCleanup(hc, docs[h.ID], h))
		}
	}()
	for _, h := range view.Hosts {
		if e = phase("prepare", h); e != nil {
			return "", e
		}
	}
	for _, h := range view.Hosts {
		if e = phase("deploy", h); e != nil {
			return "", e
		}
	}
	if e = phase("grow", hosts["meta"]); e != nil {
		return "", e
	}
	for _, h := range view.Hosts {
		if h.ID == "meta" || contains(h.Roles, "volume") {
			if contains(h.Roles, "volume") {
				if _, e = processOwner(ctx, r, hosts, h.ID); e != nil {
					return "", e
				}
			}
			if e = phase("before", h); e != nil {
				return "", e
			}
		}
	}
	if _, e = assessCloudCluster(ctx, view, c.Output, "--before", "001"); e != nil {
		return "", e
	}
	r.seedCheck = func(ctx context.Context, seeds map[string]string) error { return processCopies(ctx, r, hosts, seeds) }
	if selected != nil {
		e = runChaosCase(ctx, view, *selected, hosts, map[string]int{"volume": 1, "filer": 1}, r)
	} else {
		e = runChaos2Case(ctx, view, *mixed, hosts, r.gens, r)
		if e == nil {
			journals, err := r.each(ctx, "actual-journal", 30, func(id string) string { return "cat " + cloudQuote(hosts[id].RunRoot+"/chaos-"+name+".journal") })
			if err != nil {
				return "", err
			}
			e = processCopies(ctx, r, hosts, journals)
		}
	}
	if e != nil {
		return "", e
	}
	result = filepath.Join(c.Output, "process-result.json")
	row := map[string]any{"case": name, "status": "PASS", "C3": "NOT_RUN", "C6": "NOT_RUN", "host_tolerance": "NOT_CLAIMED"}
	if name == "C5-two-kill" {
		row["process_recovery"], row["unavailable_keys"] = "PASS", r.unavailableKeys
	}
	e = os.WriteFile(result, processJSON(row), 0600)
	return result, e // Overall direct PASS also requires the deferred clean postflight.
}

func processJSON(v any) []byte { b, _ := json.MarshalIndent(v, "", "  "); return b }
func processPins(d inventory.Document, c CloudConfig) map[string]string {
	pins := map[string]string{}
	for _, h := range d.Hosts {
		pins[h.ID] = c.HostKey
		if pins[h.ID] == "" && h.Management.HostKey != nil {
			pins[h.ID] = *h.Management.HostKey
		}
	}
	return pins
}

// Join MainPID/starttime to executable, argv/root and owned TCP listeners;
// recheck that unit generation after the status read.
func processOwner(ctx context.Context, r *chaosRun, hosts map[string]inventory.Host, id string) (string, error) {
	h := hosts[id]
	unit := r.gens.unit(r.docs[id], id, "volume")
	cmd := mainPID(unit) + `test "$(sudo -n readlink /proc/$pid/exe)" = ` + cloudQuote(h.RunRoot+"/weed-volume") + `; test "$(sudo -n readlink /proc/$pid/cwd)" = ` + cloudQuote(h.RunRoot) + `; ` +
		`args=$(sudo -n cat /proc/$pid/cmdline | tr '\000' '\n'); for arg in ` + cloudQuote("--dir="+h.RunRoot+"/volume") + ` ` + cloudQuote("--ip="+h.Data.Address) + ` ` + cloudQuote("--port="+strconv.Itoa(servicePort(h, "volume_http"))) + ` ` + cloudQuote("--port.grpc="+strconv.Itoa(servicePort(h, "volume_grpc"))) + `; do printf '%s\n' "$args" | grep -Fx -- "$arg" >/dev/null || exit 1; done; ` +
		`sockets=$(sudo -n ss -Hltnp) || exit 1; for p in ` + strconv.Itoa(servicePort(h, "volume_http")) + ` ` + strconv.Itoa(servicePort(h, "volume_grpc")) + `; do printf '%s\n' "$sockets" | awk -v p="$p" -v pid="$pid" '$4 ~ (":" p "$") && index($0,"pid=" pid ","){found=1} END{exit !found}' || exit 1; done; ` +
		`body=$(curl -fsS --max-time 10 ` + cloudQuote("http://"+endpoint(h, "volume_http")+"/status") + `) || exit 1; test "$(` + chaosStat + `)" = "$start"; test "$(sudo -n systemctl show -p MainPID --value ` + cloudQuote(unit) + `)" = "$pid"; printf 'OWNER %s %s %s %s\n%s\n' "$pid" "$start" ` + cloudQuote(endpoint(h, "volume_http")) + ` ` + cloudQuote(h.RunRoot) + ` "$body"`
	out, e := r.step(ctx, id, "copies-"+id, 30, cmd)
	receipt, body, ok := strings.Cut(out, "\n")
	f := strings.Fields(receipt)
	var st struct{ DiskStatuses []struct{ Dir string } }
	if e != nil || !ok || len(f) != 5 || f[0] != "OWNER" || f[3] != endpoint(h, "volume_http") || f[4] != h.RunRoot || json.Unmarshal([]byte(body), &st) != nil || len(st.DiskStatuses) != 1 || st.DiskStatuses[0].Dir != h.RunRoot+"/volume" {
		return "", errors.Join(e, errors.New("volume role identity/endpoint/root not bound: "+id))
	}
	return body, nil
}

// Join every actual ACK VID to two distinct observed volume-process owners
// and the master's actual lookup. Replication 001 alone is not this proof.
func processCopies(ctx context.Context, r *chaosRun, hosts map[string]inventory.Host, acks map[string]string) error {
	owners := map[string][]string{}
	for i := 1; i <= 6; i++ {
		id := "vol" + strconv.Itoa(i)
		out, e := processOwner(ctx, r, hosts, id)
		var st struct {
			Volumes []struct {
				Id       uint64
				ReadOnly bool
			}
		}
		if e != nil || json.Unmarshal([]byte(out), &st) != nil || len(st.Volumes) == 0 {
			return errors.Join(e, errors.New("missing volume owners on "+id))
		}
		writable := false
		for _, v := range st.Volumes {
			owners[strconv.FormatUint(v.Id, 10)] = append(owners[strconv.FormatUint(v.Id, 10)], endpoint(hosts[id], "volume_http"))
			writable = writable || !v.ReadOnly
		}
		if !writable {
			return errors.New("no writable owner on " + id)
		}
	}
	for _, urls := range owners {
		sort.Strings(urls)
		if len(urls) != 2 || urls[0] == urls[1] {
			return errors.New("observed volume copies differ from two")
		}
	}
	used := map[string]bool{}
	for _, out := range acks {
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			var ack struct {
				Event string
				FIDs  []string
			}
			if json.Unmarshal([]byte(line), &ack) != nil {
				return errors.New("ACK journal row malformed")
			}
			if ack.Event != "ACK" {
				continue
			}
			if len(ack.FIDs) == 0 {
				return errors.New("ACK missing FID")
			}
			for _, fid := range ack.FIDs {
				vid, _, ok := strings.Cut(fid, ",")
				if !ok || len(owners[vid]) != 2 {
					return errors.New("ACK has unknown copies")
				}
				used[vid] = true
			}
		}
	}
	exercised := map[string]bool{}
	for vid := range used {
		for _, url := range owners[vid] {
			exercised[url] = true
		}
	}
	if len(exercised) != 6 {
		return errors.New("ACKs do not exercise all six owners")
	}

	for vid := range used {
		out, e := r.step(ctx, "meta", "lookup-"+vid, 30, "curl -fsS --max-time 10 "+cloudQuote("http://"+endpoint(hosts["meta"], "master_http")+"/dir/lookup?volumeId="+vid))
		var lookup struct {
			Locations []struct {
				URL string `json:"url"`
			}
		}
		if e != nil || json.Unmarshal([]byte(out), &lookup) != nil {
			return errors.Join(e, errors.New("owner lookup unreadable"))
		}
		urls := []string{}
		for _, l := range lookup.Locations {
			urls = append(urls, l.URL)
		}
		sort.Strings(urls)
		if strings.Join(urls, ",") != strings.Join(owners[vid], ",") {
			return errors.New("master lookup differs from captured owners")
		}
	}
	return r.save("copies.json", map[string]any{"owners": owners, "acked_vids": used, "copies": 2})
}
