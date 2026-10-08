package eth

import (
	"golang.org/x/crypto/sha3"
)

// Keccak256 returns the legacy Keccak-256 digest used by the EVM (not NIST SHA3-256).
func Keccak256(data ...[]byte) Hash {
	d := sha3.NewLegacyKeccak256()
	for _, b := range data {
		d.Write(b)
	}
	var h Hash
	d.Sum(h[:0])
	return h
}

// EventTopic returns topic0 for an event signature, e.g. "Sync(uint112,uint112)".
func EventTopic(signature string) Hash { return Keccak256([]byte(signature)) }

// Selector is a 4-byte function selector.
type Selector [4]byte

// NewSelector returns the selector for a function signature, e.g. "token0()".
func NewSelector(signature string) Selector {
	h := Keccak256([]byte(signature))
	return Selector{h[0], h[1], h[2], h[3]}
}

// Calldata builds ABI calldata for a function whose arguments are all static
// 32-byte words (addresses, unsigned integers). This covers every call the
// observatory makes and avoids pulling in a general-purpose ABI encoder.
func (s Selector) Calldata(args ...Hash) Data {
	out := make([]byte, 4, 4+HashLength*len(args))
	copy(out, s[:])
	for _, a := range args {
		out = append(out, a[:]...)
	}
	return out
}

// Uint64Word returns v as a big-endian, left-padded ABI word.
func Uint64Word(v uint64) Hash {
	var w Hash
	for i := 0; i < 8; i++ {
		w[HashLength-1-i] = byte(v >> (8 * i))
	}
	return w
}

// WordAt returns the i-th 32-byte word of ABI-encoded data.
func WordAt(data []byte, i int) (Hash, bool) {
	var w Hash
	off := i * HashLength
	if i < 0 || off+HashLength > len(data) {
		return w, false
	}
	copy(w[:], data[off:off+HashLength])
	return w, true
}
