package v3block

// v3_run_plan: one run's ports, work directory and NVMe NQN prefix, derived
// from port_base and the run's tag, so that concurrent runs on one host share
// nothing the product can see. The engine resolves {{ var }} one level and
// has no arithmetic, so a scenario cannot compute these itself; this action
// computes them and sets them as vars before anything uses them.
//
// Params:
//   port_base    the first port of this run's block (required)
//   volumes      the name pool the cluster spec declares (sizes the block)
//   run_tag      the run's tag, [a-z0-9]{1,32}; empty: the run id with its
//                dashes removed
//   work_prefix  the work directory is work_prefix + tag (default /root/m1-run-)
//   nqn_base     the NQN prefix is nqn_base + tag + "-"
//                (default nqn.2026-11.io.seaweedfs:op-)
//
// Ports, P = port_base, n = len(volumes):
//   P+0 weed master    P+1 weed volume    P+2 weed filer    P+3 blockmaster
//   P+10 .. P+10n+7    the holders (v3_op_spec with base_port P)
//   P+10000 .. P+10002 the weed master, volume and filer gRPC ports
//                      (SeaweedFS's port+10000; the typed ops find the filer's
//                      HTTP port as its gRPC port - 10000)
// Two runs are disjoint when their port_base values differ by at least the
// block (10n+8) and all lie within 10000 - (10n+8) of each other.
//
// A tag has no dash, so no run's NQN prefix is a prefix of another run's
// (the teardown deletes the kernel controllers by prefix).
//
// It refuses a var it would set that is already set to another value: those
// come from port_base and the tag only, never from a fixed value.

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	tr "github.com/pingqiu/sw-test-runner"
)

var runTagRe = regexp.MustCompile(`^[a-z0-9]{1,32}$`)

// runPlan computes the vars of one run.
func runPlan(portBase, volumes int, tag, workPrefix, nqnBase string) (map[string]string, error) {
	if !runTagRe.MatchString(tag) {
		return nil, fmt.Errorf("run tag %q is not [a-z0-9]{1,32}", tag)
	}
	if volumes < 1 {
		return nil, fmt.Errorf("no volumes")
	}
	block := 10*volumes + 8
	if portBase < 1024 || block > 10000 || portBase+10002 > 65535 {
		return nil, fmt.Errorf("port_base %d with %d volumes: the run needs %d..%d and %d..%d", portBase, volumes,
			portBase, portBase+block-1, portBase+10000, portBase+10002)
	}
	if !strings.HasPrefix(workPrefix, "/") || !strings.HasPrefix(nqnBase, "nqn.") {
		return nil, fmt.Errorf("work_prefix %q must be absolute and nqn_base %q an NQN", workPrefix, nqnBase)
	}
	p := func(k int) string { return strconv.Itoa(portBase + k) }
	return map[string]string{
		"run_tag":          tag,
		"work":             workPrefix + tag,
		"nqn_prefix":       nqnBase + tag + "-",
		"weed_master_port": p(0), "weed_volume_port": p(1), "weed_filer_port": p(2), "master_port": p(3),
		"holder_base_port":      p(0),
		"weed_master_grpc_port": p(10000), "weed_volume_grpc_port": p(10001), "weed_filer_grpc_port": p(10002),
	}, nil
}

func v3RunPlan(_ context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	pb, err := strconv.Atoi(strings.TrimSpace(act.Params["port_base"]))
	if err != nil {
		return nil, fmt.Errorf("v3_run_plan: port_base: %w", err)
	}
	tag := act.Params["run_tag"]
	if tag == "" {
		tag = strings.ReplaceAll(actx.Vars["run_id"], "-", "")
	}
	wp, nb := act.Params["work_prefix"], act.Params["nqn_base"]
	if wp == "" {
		wp = "/root/m1-run-"
	}
	if nb == "" {
		nb = "nqn.2026-11.io.seaweedfs:op-"
	}
	vars, err := runPlan(pb, len(strings.Fields(act.Params["volumes"])), tag, wp, nb)
	if err != nil {
		return nil, fmt.Errorf("v3_run_plan: %w", err)
	}
	for k := range vars {
		if actx.Vars[k] != "" && actx.Vars[k] != vars[k] {
			return nil, fmt.Errorf("v3_run_plan: %s is already set (%q): it comes from port_base and the run tag only", k, actx.Vars[k])
		}
	}
	for k, v := range vars {
		actx.Vars[k] = v
	}
	actx.Log("  v3_run_plan: tag %s, work %s, ports %s..%d and %s..%s", tag, vars["work"], vars["weed_master_port"],
		pb+10*len(strings.Fields(act.Params["volumes"]))+7, vars["weed_master_grpc_port"], vars["weed_filer_grpc_port"])
	return map[string]string{"value": tag}, nil
}
