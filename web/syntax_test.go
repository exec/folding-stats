package web

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLanguageResolution(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping locale check")
	}
	dir := t.TempDir()
	src, err := assets.ReadFile("i18n.js")
	if err != nil {
		t.Fatal(err)
	}
	src = bytes.Replace(src, []byte("const translations ="), []byte("export const translations ="), 1)
	src = bytes.Replace(src, []byte("const routeTranslations ="), []byte("export const routeTranslations ="), 1)
	if err := os.WriteFile(filepath.Join(dir, "i18n.mjs"), src, 0o600); err != nil {
		t.Fatal(err)
	}
	check := `let locale = 'de';
globalThis.window = {localStorage:{getItem:()=> locale}};
const {resolveLanguage,translations,routeTranslations} = await import('./i18n.mjs');
if (resolveLanguage('', ['de-DE']) !== 'de') throw new Error('German region fallback');
if (resolveLanguage('', ['pt-PT']) !== 'pt-BR') throw new Error('Portuguese fallback');
if (resolveLanguage('', ['ru-RU']) !== 'ru') throw new Error('Russian region fallback');
if (resolveLanguage('fr', ['de-DE']) !== 'fr') throw new Error('stored preference');
if (resolveLanguage('', ['xx']) !== 'en') throw new Error('English fallback');
const expected = new Set([...Object.keys(translations.de), ...Object.keys(routeTranslations)]);
for (const key of expected) {
  const value = translations.ru[key];
  if (!value) throw new Error('missing Russian translation: ' + key);
  const placeholders = (text) => [...text.matchAll(/\{\d+\}/g)].map((m) => m[0]).sort().join('|');
  if (placeholders(key) !== placeholders(value)) throw new Error('Russian placeholder mismatch: ' + key);
}
for (const [code, overview, dynamic, topic, description] of [
	  ['de', 'Übersicht', 'Gesamtrang #12', 'Hardware & Übertaktung', 'Foren, Magazine und Communitys rund um Bau, Tuning und Kühlung von PCs.'],
	  ['fr', 'Vue d’ensemble', 'Rang cumulé n° 12', 'Matériel et overclocking', 'Forums, magazines et communautés consacrés au montage, au réglage et au refroidissement des PC.'],
	  ['pt-BR', 'Visão geral', 'Posição total nº 12', 'Hardware e overclocking', 'Fóruns, revistas e comunidades dedicados a montar, ajustar e resfriar PCs.'],
	  ['ru', 'Обзор', 'Рейтинг #12', 'Железо и разгон', 'Форумы, журналы и сообщества, посвящённые сборке, настройке и охлаждению ПК.'],
]) {
  locale = code;
  const {t,tr} = await import('./i18n.mjs?' + code);
  if (t('Overview') !== overview) throw new Error(code + ' label translation');
  if (tr('Lifetime rank #{0}', 12) !== dynamic) throw new Error(code + ' interpolated translation');
  if (t('Hardware & overclocking') !== topic) throw new Error(code + ' topic name');
  if (t('Forums, magazines and communities built around building, tuning and cooling PCs.') !== description)
    throw new Error(code + ' topic description');
}`
	cmd := exec.Command(node, "--input-type=module", "--eval", check)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("locale check: %v\n%s", err, out)
	}
}

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
