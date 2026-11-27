package declared

import (
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// productWords are words of a protocol or a product. The executor knows a
// description, not a product: none of them may stand in its code (names,
// strings), comments aside. A product's words belong in its description.
var productWords = []string{
	"nvme", "subsysnqn", "/sys/class/nvme", "guest connect", "blockvolume", "blockmaster", "seaweed", "weed",
	"kernel", "sysfs", "snapshot",
}

// codeWords lists the product words in Go source src outside its comments.
func codeWords(name string, src []byte) []string {
	var s scanner.Scanner
	fs := token.NewFileSet()
	s.Init(fs.AddFile(name, fs.Base(), len(src)), src, nil, 0) // comments are skipped
	var code strings.Builder
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if lit == "" {
			lit = tok.String()
		}
		code.WriteString(strings.ToLower(lit))
		code.WriteByte(' ')
	}
	var out []string
	for _, w := range productWords {
		if strings.Contains(code.String(), w) {
			out = append(out, w)
		}
	}
	return out
}

func TestNoProductWordsInTheExecutor(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	n := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		n++
		if ws := codeWords(f, b); len(ws) != 0 {
			t.Errorf("%s names %s outside its comments", f, strings.Join(ws, ", "))
		}
	}
	if n == 0 {
		t.Fatal("no source files read")
	}
	// Red controls: a word in a string, and in a name (any case), is caught;
	// the same word in a comment is not.
	for src, want := range map[string]bool{
		"package p\nvar x = \"guest connect: \" // ok\n":               true,
		"package p\nfunc scanNVMe() {}\n":                              true,
		"package p\nvar p = \"/sys/class/nvme/x\"\n":                   true,
		"package p\n// the kernel's sysfs, nvme and weed\nvar x = 1\n": false,
		"package p\n/* blockvolume */\nvar y = 2\n":                    false,
	} {
		if got := len(codeWords("seed.go", []byte(src))) != 0; got != want {
			t.Errorf("red control %q: caught %v, want %v", src, got, want)
		}
	}
}
