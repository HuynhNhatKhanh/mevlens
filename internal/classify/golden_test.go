package classify_test

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/holiman/uint256"

	"github.com/huynhnhatkhanh/mevlens/internal/classify"
	"github.com/huynhnhatkhanh/mevlens/internal/eth"
	"github.com/huynhnhatkhanh/mevlens/internal/fixture"
	"github.com/huynhnhatkhanh/mevlens/internal/pricing"
)

var update = flag.Bool("update", false, "rewrite golden files")

// Fixtures are real Arbitrum One blocks captured with
//
//	mevlens inspect -block N -dump internal/classify/testdata
//
// The expected arbitrages were verified independently from the receipts' ERC-20
// Transfer logs (net balance change of the executing contract).
func TestGoldenBlocks(t *testing.T) {
	files, err := filepath.Glob("testdata/block-*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, ".golden.json") {
			continue
		}
		t.Run(filepath.Base(path), func(t *testing.T) {
			fx, err := fixture.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			res := classifyFixture(t, fx)
			got := summarize(&res)

			golden := strings.TrimSuffix(path, ".json") + ".golden.json"
			if *update {
				if err := fixture.Save(golden, got); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("missing golden file (run with -update): %v", err)
			}
			var buf bytes.Buffer
			if err := json.MarshalWrite(&buf, got, jsontext.WithIndent("  "), json.Deterministic(true)); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(bytes.TrimSpace(buf.Bytes()), bytes.TrimSpace(want)) {
				t.Errorf("classification changed for %s:\n got: %s\nwant: %s", path, buf.String(), want)
			}
		})
	}
}

func classifyFixture(t *testing.T, fx *fixture.Fixture) classify.Result {
	t.Helper()
	o, err := pricing.New(pricing.Config{
		WETH:      eth.MustAddress("0x82af49447d8a07e3bd95bd0d56f35241523fbab1"),
		Stables:   []pricing.Stable{{Address: eth.MustAddress("0xaf88d065e77c8cc2239327c5edb3a432268e5831"), Decimals: 6}},
		RefPool:   fx.RefPool,
		RefStable: eth.MustAddress("0xaf88d065e77c8cc2239327c5edb3a432268e5831"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if fx.SqrtPrice != "" {
		seed, err := uint256.FromDecimal(fx.SqrtPrice)
		if err != nil {
			t.Fatal(err)
		}
		// inspect seeds from the parent's state, so the price is as of Number-1.
		o.SetSqrtPrice(seed, uint64(fx.Block.Header.Number)-1)
	}
	return classify.New(fixture.NewLookup(fx.Pools), o).Classify(&fx.Block)
}

type goldenArb struct {
	Tx          string   `json:"tx"`
	Status      string   `json:"status"`
	Contract    string   `json:"contract"`
	Hops        uint8    `json:"hops"`
	Pools       []string `json:"pools"`
	ProfitToken string   `json:"profit_token"`
	ProfitRaw   string   `json:"profit_raw"`
	PriorityWei uint64   `json:"priority_fee_per_gas_wei"`
	GasUsed     uint64   `json:"gas_used"`
	Timeboosted bool     `json:"timeboosted"`
}

type goldenBlock struct {
	Block      uint64      `json:"block"`
	Txs        uint32      `json:"txs"`
	Swaps      uint32      `json:"swaps"`
	Arbitrages []goldenArb `json:"arbitrages"`
}

func summarize(res *classify.Result) goldenBlock {
	g := goldenBlock{Block: res.Block.Number, Txs: res.Block.TxCount, Swaps: res.Block.Swaps, Arbitrages: []goldenArb{}}
	for _, a := range res.Arbs {
		ga := goldenArb{
			Tx: a.TxHash.Hex(), Status: a.Status.String(), Contract: a.To.Hex(), Hops: a.Hops,
			ProfitToken: a.ProfitToken.Hex(), ProfitRaw: a.Profit.Dec(),
			PriorityWei: a.PriorityFeePerGas, GasUsed: a.GasUsed, Timeboosted: a.Timeboosted, Pools: []string{},
		}
		for _, p := range a.Pools {
			ga.Pools = append(ga.Pools, p.String())
		}
		g.Arbitrages = append(g.Arbitrages, ga)
	}
	return g
}
