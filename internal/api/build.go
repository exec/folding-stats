package api

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"strconv"
	"sync"
	"time"
)

// buildID identifies the running binary, so a cache validator changes when the code
// that computed the response changes.
//
// It hashes the executable rather than reading a version stamped at link time,
// because that keeps the documented build command working as-is —
// `go build -trimpath -ldflags="-s -w"` — and cannot be forgotten by whoever next
// builds a release. A wrong answer here is silent and lasts an hour, which is exactly
// the kind of thing not to make somebody remember.
//
// The frontend solves the same problem the same way: one hash over the asset set,
// stamped onto every internal URL, so a deploy invalidates the whole graph at once.
//
// Computed once per process. The read costs a few tens of milliseconds on a ~13 MB
// binary, at startup, once.
var buildID = sync.OnceValue(func() string {
	if h, ok := hashExecutable(); ok {
		return h
	}
	// Falling back to the start time is deliberately conservative: it changes on
	// every restart, so a deploy is never missed. It only costs a redundant cache
	// miss for restarts that did not change the binary, which is the safe direction
	// to be wrong in.
	return "t" + strconv.FormatInt(time.Now().UnixNano(), 36)
})

func hashExecutable() (string, bool) {
	path, err := os.Executable()
	if err != nil {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", false
	}
	// Twelve hex characters is 48 bits. These are compared for equality against a
	// value we issued ourselves, never guessed at, so the only thing that matters is
	// that two different builds do not collide.
	return hex.EncodeToString(h.Sum(nil))[:12], true
}
