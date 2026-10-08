// Package fixture stores real blocks, together with the pool metadata and price
// seed needed to classify them offline, for deterministic golden tests.
package fixture

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"

	"github.com/huynhnhatkhanh/mevlens/internal/dex"
	"github.com/huynhnhatkhanh/mevlens/internal/eth"
)

// Fixture is a self-contained classification input.
type Fixture struct {
	Chain     string      `json:"chain"`
	Block     eth.Block   `json:"block"`
	Pools     []dex.Pool  `json:"pools"`
	RefPool   eth.Address `json:"ref_pool"`
	SqrtPrice string      `json:"ref_sqrt_price_x96"` // decimal; seeds the ETH/USD oracle
}

// Load reads a fixture file.
func Load(path string) (*Fixture, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var fx Fixture
	if err := json.UnmarshalRead(f, &fx); err != nil {
		return nil, fmt.Errorf("fixture %s: %w", path, err)
	}
	return &fx, nil
}

// Save writes v as indented, deterministic JSON.
func Save(path string, v any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := json.MarshalWrite(f, v, jsontext.WithIndent("  "), json.Deterministic(true)); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Lookup is a static registry for offline classification.
type Lookup map[dex.PoolID]dex.Pool

// NewLookup indexes pools by address.
func NewLookup(pools []dex.Pool) Lookup {
	l := make(Lookup, len(pools))
	for _, p := range pools {
		l[p.ID] = p
	}
	return l
}

// Lookup implements classify.PoolLookup.
func (l Lookup) Lookup(id dex.PoolID) (dex.Pool, bool) {
	p, ok := l[id]
	return p, ok
}
