package v3block

// Frozen copies of the device Go handlers that product/actions.json's
// device actions replaced (v3_op_write, v3_op_read, v3_op_flush), as they
// were before the move (feat/v3block-m2 at 4fb5f3d), with frozen copies of
// every helper whose body the move changed or removed (the device lookup,
// the sysfs scan, the detached device run), so the old behaviour cannot
// drift with the new code. The equivalence tests (device_equiv_test.go) run
// each against its declared action. Test code only: nothing registers them.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/declared"
)

// The guest's device I/O runner deadline and poll as they were declared
// (guest.device_io_runner_deadline_seconds) at the frozen commit.
var deviceDeadlineFrozen, devicePollFrozen = 120 * time.Second, 200 * time.Millisecond

func nvmeScanFrozen(ctx context.Context, node runner, nqn string) (state, dev, why string) {
	cmd := fmt.Sprintf(`for d in /sys/class/nvme/nvme*; do [ -e "$d" ] || continue; q=$(cat "$d/subsysnqn") || exit 3; [ "$q" = %s ] || continue; echo "ctrl $(basename "$d")"; for n in "$d"/nvme*n*; do [ -e "$n" ] && echo "ns $(basename "$n")"; done; done; echo end`, shellQuote(nqn))
	stdout, stderr, code, err := node.Run(ctx, cmd)
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if err != nil || code != 0 || len(lines) == 0 || lines[len(lines)-1] != "end" {
		return "unreadable", "", fmt.Sprintf("sysfs scan exit %d: %s", code, firstLine(stderr))
	}
	var ctrls, nss []string
	for _, l := range lines[:len(lines)-1] {
		switch {
		case strings.HasPrefix(l, "ctrl "):
			ctrls = append(ctrls, l[5:])
		case strings.HasPrefix(l, "ns "):
			nss = append(nss, regexp.MustCompile(`c[0-9]+n`).ReplaceAllString(l[3:], "n"))
		default:
			return "unreadable", "", "sysfs scan printed " + firstLine(l)
		}
	}
	switch {
	case len(ctrls) == 0:
		return "absent", "", ""
	case len(ctrls) == 1 && len(nss) == 1:
		return "one", "/dev/" + nss[0], ""
	}
	return "incomplete", "", fmt.Sprintf("subsystem %s: %d controllers, %d namespaces", nqn, len(ctrls), len(nss))
}

func deviceOfFrozen(ctx context.Context, node runner, nqn string, bound time.Duration) (string, string) {
	end := time.Now().Add(bound)
	for {
		state, dev, why := nvmeScanFrozen(ctx, node, nqn)
		if state == "one" {
			return dev, ""
		}
		if state == "unreadable" {
			return "", why
		}
		if time.Now().After(end) {
			if state == "absent" {
				return "", "no controller of " + nqn
			}
			return "", why
		}
		if !pause(ctx, 250*time.Millisecond) {
			return "", "no single device of " + nqn + " by the runner deadline"
		}
	}
}

func (e opEnv) runDeviceFrozen(ctx context.Context, node runner, cmd string) (string, string, int, map[string]any) {
	stdout, stderr, code, err := declared.RunDetached(ctx, node, true, e.root+"/.runner-io", cmd, deviceDeadlineFrozen, devicePollFrozen)
	if t, ok := declared.AsDetached(err); ok {
		return "", "", -1, declared.Abandon(ctx, t)
	}
	if err != nil {
		return "", "", -1, map[string]any{"detail": err.Error()}
	}
	return stdout, stderr, code, nil
}

func blockTagFrozen(vol, round string, lba uint64) string {
	return fmt.Sprintf("%s|%s|%d|", vol, round, lba)
}

func blockSHAFrozen(tag string) string {
	b := []byte(strings.Repeat(tag, 4096/len(tag)+1))[:4096]
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func parseLBAsFrozen(v *opVolume, s string) ([]uint64, error) {
	if s == "written" {
		var out []uint64
		for k := range v.Written {
			n, _ := strconv.ParseUint(k, 10, 64)
			out = append(out, n)
		}
		sort.Slice(out, func(a, b int) bool { return out[a] < out[b] })
		return out, nil
	}
	var out []uint64
	for _, f := range strings.Fields(s) {
		n, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("lba %q", f)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, errors.New("lbas are required")
	}
	return out, nil
}

func writeCmdFrozen(blk, dev string, lba uint64, flush bool) string {
	conv := "notrunc"
	if flush {
		conv = "fsync,notrunc"
	}
	return fmt.Sprintf("dd if=%s of=%s bs=4096 seek=%d count=1 oflag=direct conv=%s status=none", shellQuote(blk), dev, lba, conv)
}

func writeArgsFrozen(lba uint64, sha, round string, flush bool) map[string]any {
	a := map[string]any{"lba": lba, "sha256": sha, "round": round}
	if !flush {
		a["flush"] = false
	}
	return a
}

func (e opEnv) writeBlockFrozen(ctx context.Context, node runner, v *opVolume, blk string, lba uint64, flush bool) (string, map[string]any) {
	dev, why := deviceOfFrozen(ctx, node, v.NQN, 5*time.Second)
	if why != "" {
		return fail(why)
	}
	_, stderr, code, res := e.runDeviceFrozen(ctx, node, writeCmdFrozen(blk, dev, lba, flush))
	if res != nil {
		return tr.OutcomeUnknown, res
	}
	if code != 0 {
		return fail(fmt.Sprintf("dd exit %d: %s", code, firstLine(stderr)))
	}
	return tr.OutcomeOK, nil
}

func v3OpWriteFrozen(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, e, v, err := opBegin(actx, act, "v3_op_write")
	if err != nil {
		return nil, err
	}
	round := requireParam(act, actx, "round")
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{1,16}$`).MatchString(round) {
		return nil, errors.New("v3_op_write: round (1-16 of [A-Za-z0-9_-]) is required")
	}
	flush := true
	switch f := optionalParam(act, "flush"); f {
	case "", "true":
	case "false":
		flush = false
	default:
		return nil, fmt.Errorf("v3_op_write: flush %q; want true or false", f)
	}
	lbas, err := parseLBAsFrozen(v, requireParam(act, actx, "lbas"))
	if err != nil {
		return nil, fmt.Errorf("v3_op_write: %w", err)
	}
	if v.Written == nil {
		v.Written = map[string]bool{}
	}
	defer saveVolume(actx, v)
	var last map[string]string
	for _, lba := range lbas {
		tag := blockTagFrozen(v.Name, round, lba)
		want := blockSHAFrozen(tag)
		blk := fmt.Sprintf("%s/.block-%s-%d", e.root, v.Name, lba)
		gen := fmt.Sprintf(`mkdir -p %s && python3 -c 'import sys;t=sys.argv[1].encode();open(sys.argv[2],"wb").write((t*(4096//len(t)+1))[:4096])' %s %s && sha256sum < %s | cut -c1-64`,
			shellQuote(e.root), shellQuote(tag), shellQuote(blk), shellQuote(blk))
		stdout, stderr, code, err := node.Run(ctx, gen)
		if err != nil || code != 0 || strings.TrimSpace(stdout) != want {
			return nil, fmt.Errorf("v3_op_write: the block generator made %q (exit %d, %s), not %s: runner failure, nothing written", firstLine(stdout), code, firstLine(stderr), want)
		}
		v.Written[strconv.FormatUint(lba, 10)] = true
		last, err = recordBounded(ctx, actx, e, "write", v.Name, writeArgsFrozen(lba, want, round, flush), func(ctx context.Context) (string, map[string]any) {
			return e.writeBlockFrozen(ctx, node, v, blk, lba, flush)
		})
		if err != nil {
			return nil, err
		}
	}
	return last, nil
}

func flushCmdFrozen(dev string) string {
	return fmt.Sprintf(`python3 -I -c 'import os,sys;fd=os.open(sys.argv[1],os.O_WRONLY);os.fsync(fd);os.close(fd)' %s`, dev)
}

func (e opEnv) flushDeviceFrozen(ctx context.Context, node runner, v *opVolume) (string, map[string]any) {
	dev, why := deviceOfFrozen(ctx, node, v.NQN, 5*time.Second)
	if why != "" {
		return fail(why)
	}
	_, stderr, code, res := e.runDeviceFrozen(ctx, node, flushCmdFrozen(dev))
	if res != nil {
		res["device"] = dev
		return tr.OutcomeUnknown, res
	}
	if code != 0 {
		return fail(fmt.Sprintf("fsync exit %d: %s", code, firstLine(stderr)))
	}
	return tr.OutcomeOK, map[string]any{"device": dev}
}

func v3OpFlushFrozen(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, e, v, err := opBegin(actx, act, "v3_op_flush")
	if err != nil {
		return nil, err
	}
	return recordBounded(ctx, actx, e, "flush", v.Name, map[string]any{}, func(ctx context.Context) (string, map[string]any) {
		return e.flushDeviceFrozen(ctx, node, v)
	})
}

func readCmdFrozen(dev string, lba uint64) string {
	return fmt.Sprintf("bash -c %s", shellQuote(fmt.Sprintf("set -o pipefail; dd if=%s bs=4096 skip=%d count=1 iflag=direct status=none | sha256sum | cut -c1-64", dev, lba)))
}

func v3OpReadFrozen(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, e, v, err := opBegin(actx, act, "v3_op_read")
	if err != nil {
		return nil, err
	}
	lbas, err := parseLBAsFrozen(v, requireParam(act, actx, "lbas"))
	if err != nil {
		return nil, fmt.Errorf("v3_op_read: %w", err)
	}
	if len(lbas) == 0 {
		return nil, fmt.Errorf("v3_op_read: no lba to read on %s (lbas %q)", v.Name, requireParam(act, actx, "lbas"))
	}
	var last map[string]string
	for _, lba := range lbas {
		last, err = recordBounded(ctx, actx, e, "read", v.Name, map[string]any{"lba": lba}, func(ctx context.Context) (string, map[string]any) {
			dev, why := deviceOfFrozen(ctx, node, v.NQN, 5*time.Second)
			if why != "" {
				return fail(why)
			}
			stdout, stderr, code, res := e.runDeviceFrozen(ctx, node, readCmdFrozen(dev, lba))
			if res != nil {
				return tr.OutcomeUnknown, res
			}
			sum := strings.TrimSpace(stdout)
			if code != 0 || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(sum) {
				return fail(fmt.Sprintf("read exit %d: %s", code, firstLine(stderr)))
			}
			return tr.OutcomeOK, map[string]any{"sha256": sum}
		})
		if err != nil {
			return nil, err
		}
	}
	return last, nil
}
