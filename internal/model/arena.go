package model

import "unsafe"

// NameArena interns donor and team names into one contiguous buffer.
//
// The corpus holds ~2.1M distinct names averaging 9 bytes. Held as individual Go
// strings in a map[string]int32 that costs roughly 130 MB — 16 bytes of string
// header per name, plus map overhead, plus 2.1M separate allocations for the GC to
// scan every cycle. Packing the bytes into one slice and indexing them with an
// open-addressed table of int32 slots brings that under 50 MB and gives the garbage
// collector three pointers to look at instead of millions.
type NameArena struct {
	buf     []byte
	offsets []uint32 // offsets[i]..offsets[i+1] bounds name i
	table   []int32  // slot -> id+1; 0 means empty
	mask    uint64
}

// NewNameArena returns an arena sized for about n names. Oversizing is cheap
// relative to rehashing 2M entries mid-ingest.
func NewNameArena(n int) *NameArena {
	if n < 16 {
		n = 16
	}
	// Load factor 0.5: probe chains stay short, and the table is only 4 bytes a
	// slot so the memory cost of the slack is minor.
	size := uint64(16)
	for size < uint64(n)*2 {
		size <<= 1
	}
	a := &NameArena{
		buf:     make([]byte, 0, n*12),
		offsets: make([]uint32, 1, n+1),
		table:   make([]int32, size),
		mask:    size - 1,
	}
	return a
}

// Len reports how many distinct names are interned.
func (a *NameArena) Len() int { return len(a.offsets) - 1 }

// Intern returns the id for name, adding it if unseen.
func (a *NameArena) Intern(name string) int32 {
	h := hashStr(name)
	for i := h & a.mask; ; i = (i + 1) & a.mask {
		slot := a.table[i]
		if slot == 0 {
			id := int32(a.Len())
			a.buf = append(a.buf, name...)
			a.offsets = append(a.offsets, uint32(len(a.buf)))
			a.table[i] = id + 1
			if uint64(a.Len())*2 >= uint64(len(a.table)) {
				a.grow()
			}
			return id
		}
		if a.equal(slot-1, name) {
			return slot - 1
		}
	}
}

// Lookup returns the id for name without interning it.
func (a *NameArena) Lookup(name string) (int32, bool) {
	h := hashStr(name)
	for i := h & a.mask; ; i = (i + 1) & a.mask {
		slot := a.table[i]
		if slot == 0 {
			return 0, false
		}
		if a.equal(slot-1, name) {
			return slot - 1, true
		}
	}
}

// Bytes returns a view of the name. It aliases arena storage and must not be
// retained across further Intern calls, which may reallocate the buffer.
func (a *NameArena) Bytes(id int32) []byte {
	if id < 0 || int(id) >= a.Len() {
		return nil
	}
	return a.buf[a.offsets[id]:a.offsets[id+1]]
}

// Name returns a copy of the name, safe to retain.
func (a *NameArena) Name(id int32) string { return string(a.Bytes(id)) }

func (a *NameArena) equal(id int32, s string) bool {
	b := a.buf[a.offsets[id]:a.offsets[id+1]]
	if len(b) != len(s) {
		return false
	}
	// unsafe conversion avoids allocating a string per probe; b is not retained.
	return unsafe.String(unsafe.SliceData(b), len(b)) == s
}

func (a *NameArena) grow() {
	size := uint64(len(a.table)) * 2
	tab := make([]int32, size)
	mask := size - 1
	for id := 0; id < a.Len(); id++ {
		b := a.buf[a.offsets[id]:a.offsets[id+1]]
		h := hashBytes(b)
		for i := h & mask; ; i = (i + 1) & mask {
			if tab[i] == 0 {
				tab[i] = int32(id) + 1
				break
			}
		}
	}
	a.table, a.mask = tab, mask
}

// FNV-1a. Names are short and adversarial input is not a concern here, so a fast
// non-cryptographic hash with good avalanche on short strings is the right trade.
const (
	fnvOffset = 14695981039346656037
	fnvPrime  = 1099511628211
)

func hashStr(s string) uint64 {
	h := uint64(fnvOffset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= fnvPrime
	}
	return h
}

func hashBytes(b []byte) uint64 {
	h := uint64(fnvOffset)
	for _, c := range b {
		h ^= uint64(c)
		h *= fnvPrime
	}
	return h
}
