// Package eth defines the minimal, allocation-conscious Ethereum primitives used
// across MEVLens: addresses, hashes and JSON-RPC hex encodings.
//
// The package deliberately does not depend on go-ethereum. Arbitrum blocks contain
// Arbitrum-specific transaction types that upstream go-ethereum cannot decode, and
// the observatory only needs a small, well-tested subset of the wire format.
package eth

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
)

// AddressLength and HashLength are the byte sizes of an address and a 32-byte word.
const (
	AddressLength = 20
	HashLength    = 32
)

var (
	// ErrMissingPrefix is returned when a hex string does not start with "0x".
	ErrMissingPrefix = errors.New("eth: hex string without 0x prefix")
	// ErrLength is returned when decoded bytes do not match the expected length.
	ErrLength = errors.New("eth: hex string has wrong length")
)

// Address is a 20-byte account or contract address. The zero value is the zero address,
// which is also what a JSON null decodes to (e.g. the "to" field of a contract creation).
type Address [AddressLength]byte

// Hash is a 32-byte Keccak-256 hash or EVM word.
type Hash [HashLength]byte

// ParseAddress parses a 0x-prefixed, 40-hex-digit address (any letter case).
func ParseAddress(s string) (Address, error) {
	var a Address
	err := decodeFixed(a[:], []byte(s))
	return a, err
}

// MustAddress is ParseAddress that panics on error. Use only for constants.
func MustAddress(s string) Address {
	a, err := ParseAddress(s)
	if err != nil {
		panic(fmt.Sprintf("eth.MustAddress(%q): %v", s, err))
	}
	return a
}

// ParseHash parses a 0x-prefixed, 64-hex-digit hash.
func ParseHash(s string) (Hash, error) {
	var h Hash
	err := decodeFixed(h[:], []byte(s))
	return h, err
}

// MustHash is ParseHash that panics on error. Use only for constants.
func MustHash(s string) Hash {
	h, err := ParseHash(s)
	if err != nil {
		panic(fmt.Sprintf("eth.MustHash(%q): %v", s, err))
	}
	return h
}

// Hex returns the lowercase 0x-prefixed hex form.
func (a Address) Hex() string { return encodeHex(a[:]) }

// String implements fmt.Stringer.
func (a Address) String() string { return a.Hex() }

// IsZero reports whether a is the zero address.
func (a Address) IsZero() bool { return a == Address{} }

// Compare orders addresses by their big-endian byte value (the order Uniswap uses
// to assign token0/token1).
func (a Address) Compare(b Address) int { return bytes.Compare(a[:], b[:]) }

// MarshalText implements encoding.TextMarshaler.
func (a Address) MarshalText() ([]byte, error) { return appendHex(nil, a[:]), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (a *Address) UnmarshalText(b []byte) error { return decodeFixed(a[:], b) }

// Word returns the address left-padded to a 32-byte ABI word.
func (a Address) Word() Hash {
	var w Hash
	copy(w[HashLength-AddressLength:], a[:])
	return w
}

// Hex returns the lowercase 0x-prefixed hex form.
func (h Hash) Hex() string { return encodeHex(h[:]) }

// String implements fmt.Stringer.
func (h Hash) String() string { return h.Hex() }

// IsZero reports whether h is all zeroes.
func (h Hash) IsZero() bool { return h == Hash{} }

// Address interprets the word as a left-padded ABI address (the low 20 bytes).
func (h Hash) Address() Address {
	var a Address
	copy(a[:], h[HashLength-AddressLength:])
	return a
}

// MarshalText implements encoding.TextMarshaler.
func (h Hash) MarshalText() ([]byte, error) { return appendHex(nil, h[:]), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (h *Hash) UnmarshalText(b []byte) error { return decodeFixed(h[:], b) }

// Quantity is a JSON-RPC hex quantity that fits in 64 bits ("0x1a").
type Quantity uint64

// MarshalText implements encoding.TextMarshaler.
func (q Quantity) MarshalText() ([]byte, error) {
	return strconv.AppendUint([]byte("0x"), uint64(q), 16), nil
}

// UnmarshalText implements encoding.TextUnmarshaler. Leading zeroes are tolerated:
// some providers emit them even though the JSON-RPC spec forbids it.
func (q *Quantity) UnmarshalText(b []byte) error {
	if !has0x(b) {
		return ErrMissingPrefix
	}
	if len(b) == 2 {
		return fmt.Errorf("eth: empty quantity")
	}
	v, err := strconv.ParseUint(string(b[2:]), 16, 64)
	if err != nil {
		return fmt.Errorf("eth: invalid quantity %q: %w", b, err)
	}
	*q = Quantity(v)
	return nil
}

// Data is a 0x-prefixed hex byte string ("0x" decodes to an empty slice).
type Data []byte

// MarshalText implements encoding.TextMarshaler.
func (d Data) MarshalText() ([]byte, error) { return appendHex(nil, d), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Data) UnmarshalText(b []byte) error {
	if !has0x(b) {
		return ErrMissingPrefix
	}
	src := b[2:]
	if len(src)%2 != 0 {
		return ErrLength
	}
	out := make([]byte, len(src)/2)
	if _, err := hex.Decode(out, src); err != nil {
		return fmt.Errorf("eth: invalid hex data: %w", err)
	}
	*d = out
	return nil
}

// FormatBlock renders a block number as a JSON-RPC block parameter.
func FormatBlock(n uint64) string { return "0x" + strconv.FormatUint(n, 16) }

func decodeFixed(dst, b []byte) error {
	if !has0x(b) {
		return ErrMissingPrefix
	}
	src := b[2:]
	if len(src) != 2*len(dst) {
		return fmt.Errorf("%w: want %d hex digits, got %d", ErrLength, 2*len(dst), len(src))
	}
	if _, err := hex.Decode(dst, src); err != nil {
		return fmt.Errorf("eth: invalid hex: %w", err)
	}
	return nil
}

func has0x(b []byte) bool { return len(b) >= 2 && b[0] == '0' && (b[1] == 'x' || b[1] == 'X') }

func appendHex(dst, src []byte) []byte {
	dst = append(dst, '0', 'x')
	return hex.AppendEncode(dst, src)
}

func encodeHex(src []byte) string { return string(appendHex(make([]byte, 0, 2+2*len(src)), src)) }
