package infra

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DeploymentPolicy must come from independently approved operator policy, never
// the guest's own report or unverified inventory. The initial provider requires
// root-owned Linux installation files, distinct from the unprivileged job UID.
type DeploymentPolicy struct {
	NodeID, Host, User, HostKey, InstallRoot string
	ExpectedBootID                           string
	WorkloadUID                              uint32
	RequireNative, RequireInterpreter        bool
	Files                                    []DeploymentFile
}

// DeploymentFile pins bytes and their reviewed roles, not a guest-supplied name.
type DeploymentFile struct {
	Path, SHA256 string
	Size         int64
	Roles        []string // runner, adapter, config, product, native, interpreter
}

// DeploymentReceipt is qualified only when VerifyDeployment returns a nil error.
// Transcript is retained on refusal too; it contains metadata/hashes, not file
// contents. Base OS read tools are a trusted-image prerequisite.
type DeploymentReceipt struct {
	NodeID, Host, User, HostKey, PolicySHA256, BootID string
	WorkloadUID                                       uint32
	ReadStart, ReadEnd                                time.Time
	Files                                             map[string]string
	Transcript                                        string
}

func deploymentPaths(p DeploymentPolicy) ([]string, map[string]bool, error) {
	validPath := func(v string) bool {
		return path.IsAbs(v) && path.Clean(v) == v && len(v) <= 4096 && !strings.ContainsAny(v, "\x00\r\n|")
	}
	if p.NodeID == "" || p.Host == "" || p.User == "" || p.HostKey == "" || !validPath(p.InstallRoot) || p.InstallRoot == "/" || p.WorkloadUID == 0 || len(p.Files) == 0 || len(p.Files) > 256 {
		return nil, nil, errors.New("incomplete protected deployment policy")
	}
	roles := map[string]bool{}
	files := map[string]bool{}
	dirs := map[string]bool{p.InstallRoot: true}
	all := map[string]bool{}
	for _, f := range p.Files {
		digest, err := hex.DecodeString(f.SHA256)
		inside := strings.HasPrefix(f.Path, p.InstallRoot+"/")
		if !validPath(f.Path) || f.Path == "/" || len(digest) != 32 || err != nil || f.SHA256 != strings.ToLower(f.SHA256) || f.Size < 0 || f.Size > 512<<20 || files[f.Path] || len(f.Roles) == 0 || len(f.Roles) > 6 {
			return nil, nil, errors.New("invalid or duplicate deployed file")
		}
		for _, role := range f.Roles {
			switch role {
			case "runner", "adapter", "config", "product", "native", "interpreter":
			default:
				return nil, nil, errors.New("unknown deployed file role")
			}
			if !inside && role != "interpreter" {
				return nil, nil, errors.New("only a pinned interpreter may be outside the installation")
			}
			roles[role] = true
		}
		files[f.Path], all[f.Path] = true, true
		for dir := path.Dir(f.Path); ; dir = path.Dir(dir) {
			all[dir] = true
			if len(all) > 1024 {
				return nil, nil, errors.New("deployment path set exceeds bound")
			}
			if strings.HasPrefix(dir, p.InstallRoot+"/") || dir == p.InstallRoot {
				dirs[dir] = true
			}
			if dir == "/" {
				break
			}
		}
	}
	for _, role := range []string{"runner", "adapter", "config", "product"} {
		if !roles[role] {
			return nil, nil, fmt.Errorf("missing deployment role %s", role)
		}
	}
	if (p.RequireNative && !roles["native"]) || (p.RequireInterpreter && !roles["interpreter"]) {
		return nil, nil, errors.New("required native/interpreter set is empty")
	}
	if files[p.InstallRoot] {
		return nil, nil, errors.New("installation root is a file")
	}
	paths := make([]string, 0, len(all))
	for name := range all {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	return paths, dirs, nil
}

func deploymentCommand(p DeploymentPolicy, paths []string) (string, error) {
	var script strings.Builder
	script.WriteString("set -eu\nexport LC_ALL=C\nprintf 'S0B-READBACK-1\\nBOOT|'\n/bin/cat /proc/sys/kernel/random/boot_id\nprintf 'UID|'\n/usr/bin/id -u\n")
	command, err := ShellArgv("/usr/bin/find", "-P", p.InstallRoot, "-mindepth", "1", "-printf", "TREE|%y|%p\n")
	if err != nil {
		return "", err
	}
	script.WriteString(command + "\n")
	metadata := func(label string) error {
		for i, name := range paths {
			fmt.Fprintf(&script, "printf '%s|%d|'\n", label, i)
			command, err := ShellArgv("/usr/bin/stat", "-c", "%d|%i|%s|%f|%u|%g|%y|%z", "--", name)
			if err != nil {
				return err
			}
			script.WriteString(command + "\n")
		}
		return nil
	}
	if err := metadata("BEFORE"); err != nil {
		return "", err
	}
	for i, f := range p.Files {
		fmt.Fprintf(&script, "printf 'HASH|%d|'\n", i)
		command, err := ShellArgv("/usr/bin/sha256sum", "--", f.Path)
		if err != nil {
			return "", err
		}
		script.WriteString(command + "\n")
		command, err = ShellArgv("/usr/sbin/getcap", "-n", f.Path)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&script, "caps=$(%s)\nprintf 'CAPS|%d|%%s\\n' \"$caps\"\n", command, i)
	}
	if err := metadata("AFTER"); err != nil {
		return "", err
	}
	script.WriteString("printf 'BOOT-END|'\n/bin/cat /proc/sys/kernel/random/boot_id\nprintf 'S0B-DONE\\n'\n")
	// The probe executes only fixed base-OS read commands. Never execute the
	// deployed runner/adapter/product/interpreter to prove its own identity.
	return ShellArgv("/usr/bin/timeout", "-k", "1", "12", "/bin/sh", "-c", script.String())
}

// VerifyDeployment reads a private snapshot over fresh pinned SSH. It executes
// only trusted base-OS read tools, never the candidate deployment itself.
func (n *Node) VerifyDeployment(parent context.Context, policy DeploymentPolicy) (receipt DeploymentReceipt, err error) {
	files := make([]DeploymentFile, len(policy.Files))
	for i, file := range policy.Files {
		files[i] = file
		files[i].Roles = append([]string(nil), file.Roles...)
	}
	policy.Files = files
	receipt = DeploymentReceipt{NodeID: policy.NodeID, Host: policy.Host, User: policy.User, HostKey: policy.HostKey, ReadStart: time.Now(), Files: map[string]string{}}
	defer func() { receipt.ReadEnd = time.Now() }()
	if n.IsLocal || n.IsNative || n.Host != policy.Host || n.User != policy.User || n.HostKey != policy.HostKey {
		return receipt, errors.New("deployment must use its approved remote SSH identity")
	}
	paths, dirs, err := deploymentPaths(policy)
	if err != nil {
		return receipt, err
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		return receipt, err
	}
	digest := sha256.Sum256(encoded)
	receipt.PolicySHA256 = hex.EncodeToString(digest[:])
	command, err := deploymentCommand(policy, paths)
	if err != nil {
		return receipt, err
	}
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	// Snapshot connection inputs and use a fresh independently pinned transport,
	// not a possibly older Node.client or a reinterpreted guest manifest.
	probe := &Node{Host: policy.Host, User: policy.User, KeyFile: n.KeyFile, HostKey: policy.HostKey}
	pipe, err := probe.OpenCommand(ctx, "/bin/sh", "-c", command)
	if err != nil {
		return receipt, err
	}
	defer pipe.Close()
	stop := context.AfterFunc(ctx, func() { _ = pipe.Close() })
	defer stop()
	// No probe command consumes stdin. A fast peer may already have completed
	// before OpenCommand returns; do not turn its closed input into a refusal.
	const maxTranscript = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(pipe, maxTranscript+1))
	receipt.Transcript = string(raw)
	if ctx.Err() != nil {
		return receipt, ctx.Err()
	}
	if err != nil || len(raw) > maxTranscript {
		return receipt, errors.New("deployment readback incomplete or oversized")
	}
	err = verifyDeploymentTranscript(&receipt, policy, paths, dirs)
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	return receipt, err
}

func verifyDeploymentTranscript(r *DeploymentReceipt, p DeploymentPolicy, paths []string, dirs map[string]bool) error {
	lines := strings.Split(strings.TrimSuffix(r.Transcript, "\n"), "\n")
	if len(lines) < 5 || lines[0] != "S0B-READBACK-1" || lines[len(lines)-1] != "S0B-DONE" {
		return errors.New("deployment readback did not complete")
	}
	before, after, hashes, caps, tree := map[int]string{}, map[int]string{}, map[int]string{}, map[int]string{}, map[string]string{}
	boot, bootEnd, uid := "", "", ""
	seenIdentity := map[string]bool{}
	for _, line := range lines[1 : len(lines)-1] {
		fields := strings.SplitN(line, "|", 3)
		if len(fields) < 2 {
			return errors.New("malformed readback record")
		}
		switch fields[0] {
		case "BOOT", "BOOT-END", "UID":
			if len(fields) != 2 {
				return errors.New("invalid boot/UID record")
			}
			destination := &boot
			if fields[0] == "BOOT-END" {
				destination = &bootEnd
			} else if fields[0] == "UID" {
				destination = &uid
			}
			if seenIdentity[fields[0]] {
				return errors.New("duplicate boot/UID record")
			}
			seenIdentity[fields[0]] = true
			*destination = fields[1]
		case "TREE":
			if len(fields) != 3 || tree[fields[2]] != "" || (fields[1] != "f" && fields[1] != "d") {
				return errors.New("duplicate, symlink or unsupported installation entry")
			}
			tree[fields[2]] = fields[1]
		case "BEFORE", "AFTER", "HASH", "CAPS":
			if len(fields) != 3 {
				return errors.New("invalid file record")
			}
			index, err := strconv.Atoi(fields[1])
			destination := before
			if fields[0] == "AFTER" {
				destination = after
			} else if fields[0] == "HASH" {
				destination = hashes
			} else if fields[0] == "CAPS" {
				destination = caps
			}
			_, duplicate := destination[index]
			if err != nil || index < 0 || duplicate {
				return errors.New("invalid or duplicate file index")
			}
			destination[index] = fields[2]
		default:
			return errors.New("unknown readback record")
		}
	}
	bootBytes, bootErr := hex.DecodeString(strings.ReplaceAll(boot, "-", ""))
	if bootErr != nil || len(bootBytes) != 16 || boot != bootEnd || (p.ExpectedBootID != "" && boot != p.ExpectedBootID) || uid != strconv.FormatUint(uint64(p.WorkloadUID), 10) {
		return errors.New("boot or workload UID mismatch")
	}
	r.BootID, r.WorkloadUID = boot, p.WorkloadUID
	fileMap := map[string]DeploymentFile{}
	for _, f := range p.Files {
		fileMap[f.Path] = f
	}
	if len(before) != len(paths) || len(after) != len(paths) || len(hashes) != len(p.Files) || len(caps) != len(p.Files) {
		return errors.New("incomplete deployment metadata/hash set")
	}
	for i, name := range paths {
		if before[i] == "" || before[i] != after[i] {
			return errors.New("deployment changed during readback")
		}
		metadata := strings.Split(before[i], "|")
		if len(metadata) != 8 {
			return errors.New("invalid deployment stat")
		}
		mode, modeErr := strconv.ParseUint(metadata[3], 16, 32)
		wantedType := uint64(0040000)
		file, isFile := fileMap[name]
		if isFile {
			wantedType = 0100000
		}
		if modeErr != nil || mode&0170000 != wantedType || mode&06022 != 0 || metadata[4] != "0" {
			return fmt.Errorf("unprotected, foreign-owned or symlink path %s", name)
		}
		if isFile {
			if metadata[2] != strconv.FormatInt(file.Size, 10) {
				return fmt.Errorf("deployed size mismatch %s", name)
			}
			for _, role := range file.Roles {
				if (role == "runner" || role == "adapter" || role == "interpreter" || role == "product") && mode&0111 == 0 {
					return fmt.Errorf("nonexecutable deployed role %s", name)
				}
			}
		}
	}
	wantedTree := map[string]string{}
	for dir := range dirs {
		if dir != p.InstallRoot {
			wantedTree[dir] = "d"
		}
	}
	for _, f := range p.Files {
		if strings.HasPrefix(f.Path, p.InstallRoot+"/") {
			wantedTree[f.Path] = "f"
		}
	}
	if len(tree) != len(wantedTree) {
		return errors.New("installation contains missing or extra entries")
	}
	for name, kind := range wantedTree {
		if tree[name] != kind {
			return errors.New("installation tree does not match approved set")
		}
	}
	for i, f := range p.Files {
		if value, exists := caps[i]; !exists || value != "" {
			return fmt.Errorf("deployed capability is not permitted %s", f.Path)
		}
		if hashes[i] != f.SHA256+"  "+f.Path {
			return fmt.Errorf("deployed digest mismatch %s", f.Path)
		}
		r.Files[f.Path] = f.SHA256
	}
	return nil
}
