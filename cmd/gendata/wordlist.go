package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// WordlistURL is a stable, widely-mirrored English word list.
const WordlistURL = "https://raw.githubusercontent.com/dwyl/english-words/master/words_alpha.txt"

// Words loads the word list, caching it under dir so repeated runs and offline work
// do not depend on the network.
func Words(dir string) ([]string, error) {
	cache := filepath.Join(dir, "words_alpha.txt")
	if f, err := os.Open(cache); err == nil {
		defer f.Close()
		return readWords(f)
	}

	req, err := http.NewRequest(http.MethodGet, WordlistURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "folding-stats-gen/0.1")
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gen: fetching wordlist: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gen: wordlist returned %s", resp.Status)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	tmp := cache + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return nil, err
	}
	f.Close()
	if err := os.Rename(tmp, cache); err != nil {
		return nil, err
	}

	g, err := os.Open(cache)
	if err != nil {
		return nil, err
	}
	defer g.Close()
	return readWords(g)
}

func readWords(f *os.File) ([]string, error) {
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		w := strings.TrimSpace(sc.Text())
		// Very short words make indistinct names; very long ones dominate the
		// arena without adding variety.
		if len(w) >= 3 && len(w) <= 12 {
			out = append(out, w)
		}
	}
	return out, sc.Err()
}
