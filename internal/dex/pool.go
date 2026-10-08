package dex

import "github.com/huynhnhatkhanh/mevlens/internal/eth"

// Pool is the resolved metadata of a contract that emitted a swap-shaped log.
// It is plain domain data: resolving it from the chain is the registry's job.
type Pool struct {
	Address   eth.Address `json:"address"`
	Kind      Kind        `json:"kind"`
	Canonical bool        `json:"canonical"`
	Venue     string      `json:"venue,omitzero"` // factory name; empty when not canonical
	Factory   eth.Address `json:"factory"`
	Token0    eth.Address `json:"token0"`
	Token1    eth.Address `json:"token1"`
	FeePips   uint32      `json:"fee_pips,omitzero"` // v3 fee tier in hundredths of a bip (500 = 0.05%)
	FirstSeen uint64      `json:"first_seen"`        // block in which the address was first seen
}

// Candidate is an address that emitted a swap-shaped log of the given kind and
// still needs to be resolved into a Pool.
type Candidate struct {
	Address eth.Address
	Kind    Kind
}
