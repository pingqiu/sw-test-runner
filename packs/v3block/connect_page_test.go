package v3block

// No drift between the description's documented connect and the page. The
// default's cite names the page lines of the documented procedure; at the
// pin (PRODUCT_SRC, like the citation test) every "nvme connect" command in
// them, and every row of the Host connect settings table in them, must give
// exactly the options the description declares.

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var (
	connectFlag  = regexp.MustCompile(`--([a-z_-]+)=(-?\d+)`)
	connectTable = regexp.MustCompile("^\\|\\s*`--([a-z_-]+)`\\s*\\|[^|]*\\|\\s*([^|]+?)\\s*\\|")
)

// pageConnect reads the options a page's lines set: from each nvme connect
// command, and from each settings table row ("leave off" sets nothing).
// It returns one option set per source found.
func pageConnect(lines []string) []map[string]int {
	var out []map[string]int
	table := map[string]int{}
	inTable := false
	for _, l := range lines {
		if strings.Contains(l, "nvme connect") && strings.Contains(l, "--") {
			set := map[string]int{}
			for _, m := range connectFlag.FindAllStringSubmatch(l, -1) {
				n, _ := strconv.Atoi(m[2])
				set[strings.ReplaceAll(m[1], "-", "_")] = n
			}
			out = append(out, set)
		}
		if m := connectTable.FindStringSubmatch(l); m != nil {
			inTable = true
			if n, err := strconv.Atoi(strings.Fields(m[2])[0]); err == nil {
				table[strings.ReplaceAll(m[1], "-", "_")] = n
			}
		}
	}
	if inTable {
		out = append(out, table)
	}
	return out
}

// connectDrift compares each option set a page gives with the declared one.
func connectDrift(sets []map[string]int, declared map[string]int) []string {
	if len(sets) == 0 {
		return []string{"the cited lines hold no nvme connect command or settings table"}
	}
	var bad []string
	for i, set := range sets {
		keys := map[string]bool{}
		for k := range set {
			keys[k] = true
		}
		for k := range declared {
			keys[k] = true
		}
		var ks []string
		for k := range keys {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			p, onPage := set[k]
			d, isDeclared := declared[k]
			if onPage != isDeclared || p != d {
				bad = append(bad, fmt.Sprintf("source %d: %s: page %v (%v), description %v (%v)", i, k, p, onPage, d, isDeclared))
			}
		}
	}
	return bad
}

func TestConnectPageDriftDetector(t *testing.T) {
	page := []string{
		`  sudo nvme connect -t tcp -a 127.0.0.1 -s 4420 -n "$NQN" --ctrl-loss-tmo=180 --reconnect-delay=2 ||`,
		"| `--ctrl-loss-tmo` | 600 s | 180 | why |",
		"| `--reconnect-delay` | 10 s | 2 | why |",
		"| `--fast_io_fail_tmo` | off | leave off | why |",
	}
	good := map[string]int{"ctrl_loss_tmo": 180, "reconnect_delay": 2}
	if bad := connectDrift(pageConnect(page), good); len(bad) != 0 {
		t.Fatalf("green twin: %v", bad)
	}
	// Red controls: a description option changed, dropped or added.
	for name, d := range map[string]map[string]int{
		"a changed value":  {"ctrl_loss_tmo": 181, "reconnect_delay": 2},
		"a dropped option": {"ctrl_loss_tmo": 180},
		"an added option":  {"ctrl_loss_tmo": 180, "reconnect_delay": 2, "fast_io_fail_tmo": 5},
	} {
		if len(connectDrift(pageConnect(page), d)) == 0 {
			t.Errorf("%s: no drift found", name)
		}
	}
	if len(connectDrift(pageConnect([]string{"no command here"}), good)) == 0 {
		t.Error("lines without a command or table passed")
	}
}

// At the pin: the default's cited page lines give exactly its options.
func TestConnectMatchesThePageAtThePin(t *testing.T) {
	src := os.Getenv("PRODUCT_SRC")
	if src == "" {
		t.Skip("PRODUCT_SRC is NOT SET: the documented connect is NOT compared with the product's page. " +
			"Set PRODUCT_SRC to a seaweed-block checkout that has the read_at commit (CI does).")
	}
	_, readAt := descriptionCites(t)
	pin := productPin(t, readAt)
	refs, err := parseCite(guest.Connect.Default.Cite)
	if err != nil || len(refs) == 0 {
		t.Fatalf("the default's cite: %v", err)
	}
	pages := 0
	for _, r := range refs {
		if !strings.HasPrefix(r.path, "docs/") {
			continue
		}
		pages++
		b, err := exec.Command("git", "-C", src, "show", pin+":"+r.path).Output()
		if err != nil {
			t.Fatalf("%s at %s: %v", r.path, pin, err)
		}
		lines := strings.Split(string(b), "\n")
		if r.hi > len(lines) {
			t.Fatalf("%s:%d-%d past its end", r.path, r.lo, r.hi)
		}
		if bad := connectDrift(pageConnect(lines[r.lo-1:r.hi]), guest.defaultSet); len(bad) != 0 {
			t.Errorf("%s:%d-%d at %s and the description's documented connect differ:\n  %s", r.path, r.lo, r.hi, pin[:12], strings.Join(bad, "\n  "))
		}
	}
	if pages == 0 {
		t.Fatal("the default cites no page")
	}
}
