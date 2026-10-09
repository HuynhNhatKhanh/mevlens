package dex

import (
	"github.com/huynhnhatkhanh/mevlens/internal/eth"
)

// PoolID identifies a pool. v2/v3 pools are contracts, so their ID is their
// address left-padded to 32 bytes. Singleton-based AMMs (Uniswap v4, and later
// Balancer-style vaults) identify pools by a bytes32 id emitted by the singleton.
type PoolID eth.Hash

// PoolIDFromAddress returns the ID of a pool that is its own contract.
func PoolIDFromAddress(a eth.Address) PoolID { return PoolID(a.Word()) }

// Address returns the contract address when the ID is an address-form ID.
func (id PoolID) Address() (eth.Address, bool) {
	for _, b := range id[:eth.HashLength-eth.AddressLength] {
		if b != 0 {
			return eth.Address{}, false
		}
	}
	return eth.Hash(id).Address(), true
}

// String renders address-form IDs as 20-byte addresses (so they can be pasted
// into an explorer) and other IDs as 32-byte hex.
func (id PoolID) String() string {
	if a, ok := id.Address(); ok {
		return a.Hex()
	}
	return eth.Hash(id).Hex()
}

// MarshalText implements encoding.TextMarshaler using the String form.
func (id PoolID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }

// UnmarshalText accepts either a 20-byte address or a 32-byte id.
func (id *PoolID) UnmarshalText(b []byte) error {
	if len(b) == 2+2*eth.AddressLength {
		var a eth.Address
		if err := a.UnmarshalText(b); err != nil {
			return err
		}
		*id = PoolIDFromAddress(a)
		return nil
	}
	return (*eth.Hash)(id).UnmarshalText(b)
}

// Pool is the resolved metadata of a pool. It is plain domain data: resolving it
// from the chain is the registry's job.
type Pool struct {
	ID         PoolID      `json:"id"`
	Contract   eth.Address `json:"contract"` // emitting contract: the pool itself, or the v4 PoolManager
	Kind       Kind        `json:"kind"`
	Canonical  bool        `json:"canonical"`
	Venue      string      `json:"venue,omitzero"`       // factory name; empty when not canonical
	Factory    eth.Address `json:"factory"`              // factory, or the PoolManager for v4
	Token0     eth.Address `json:"token0"`               // native ETH is aliased to WETH (see Native)
	Token1     eth.Address `json:"token1"`               //
	FeePips    uint32      `json:"fee_pips,omitzero"`    // fee tier in hundredths of a bip (500 = 0.05%); 0 if DynamicFee
	DynamicFee bool        `json:"dynamic_fee,omitzero"` // v4 hooks or Algebra set the fee per swap; no tier
	Hooks      eth.Address `json:"hooks,omitzero"`       // v4 hooks contract (zero if none)
	Native     bool        `json:"native,omitzero"`      // v4: token0 is native ETH, aliased to WETH
	FirstSeen  uint64      `json:"first_seen"`           // first block seen (v4: initialization block)
}

// Candidate is a pool referenced by a log that still needs to be resolved. For
// v4, an Initialize log carries the whole pool in Init and needs no lookup.
type Candidate struct {
	ID       PoolID
	Contract eth.Address
	Kind     Kind
	Init     *Pool
}
