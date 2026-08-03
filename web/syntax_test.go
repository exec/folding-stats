package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestEmbeddedScriptsParseAsModules is the gate that was missing.
//
// The frontend is a graph of ES modules imported by absolute path, so a syntax error
// anywhere in it does not break one page — it stops the whole graph loading and the
// site serves a blank shell. That is a total outage from a stray bracket, and the Go
// build cannot see it: these files are embedded bytes as far as the compiler is
// concerned.
//
// `node --check` on a .js file is *not* this check. It does not parse the file as a
// module, and it accepted an unbalanced parenthesis that took the live site down.
// Copying to .mjs first is what forces module parsing, so that is what this does.
//
// Skipped when node is absent, following the corpus tests: a machine without it can
// still run the suite, and CI or a developer machine with node catches the fault
// before it is deployed.
func TestEmbeddedScriptsParseAsModules(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping frontend syntax check")
	}

	names, err := scriptNames()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) < 5 {
		t.Fatalf("found %d embedded scripts, expected the whole frontend — has the "+
			"embed pattern changed?", len(names))
	}

	dir := t.TempDir()
	for _, name := range names {
		src, err := assets.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		// The .mjs extension is the whole point: it is what makes node parse this as
		// a module rather than as a script.
		tmp := filepath.Join(dir, strings.TrimSuffix(filepath.Base(name), ".js")+".mjs")
		if err := os.WriteFile(tmp, src, 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(node, "--check", tmp).CombinedOutput(); err != nil {
			t.Errorf("%s does not parse as an ES module:\n%s", name, out)
		}
	}
}

// scriptNames lists every embedded .js file, vendor included — a broken dependency
// stops the graph just as dead as a broken one of ours.
func scriptNames() ([]string, error) {
	var out []string
	for _, dir := range []string{".", "vendor"} {
		entries, err := assets.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".js") {
				continue
			}
			if dir == "." {
				out = append(out, e.Name())
			} else {
				out = append(out, dir+"/"+e.Name())
			}
		}
	}
	return out, nil
}
