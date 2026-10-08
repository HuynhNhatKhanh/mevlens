package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/holiman/uint256"

	"github.com/huynhnhatkhanh/mevlens/internal/classify"
	"github.com/huynhnhatkhanh/mevlens/internal/config"
	"github.com/huynhnhatkhanh/mevlens/internal/dex"
	"github.com/huynhnhatkhanh/mevlens/internal/eth"
	"github.com/huynhnhatkhanh/mevlens/internal/fixture"
	"github.com/huynhnhatkhanh/mevlens/internal/registry"
	"github.com/huynhnhatkhanh/mevlens/internal/rpc"
	"github.com/huynhnhatkhanh/mevlens/internal/telemetry"
)

// inspect classifies a single block without any database, which makes it the
// quickest way to sanity-check the classifier against the live chain.
func inspect(ctx context.Context, configPath string, n uint64, dumpDir string, stdout, stderr io.Writer) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	log, err := telemetry.NewLogger(stderr, "warn", "text")
	if err != nil {
		return err
	}
	client, err := rpc.New(cfg.RPCConfig())
	if err != nil {
		return err
	}
	if err := checkChainID(ctx, client, cfg.Chain.ChainID); err != nil {
		return err
	}
	if n == 0 {
		if n, err = client.BlockNumber(ctx); err != nil {
			return err
		}
	}
	b, err := client.BlockWithReceipts(ctx, n)
	if err != nil {
		return err
	}
	reg, err := registry.New(client, cfg.Factories())
	if err != nil {
		return err
	}
	oracle, seed, err := newOracle(ctx, cfg, client, log)
	if err != nil {
		return err
	}
	cl := classify.New(reg, oracle, classify.WithRegime(cfg.RegimeFunc()))
	if err := reg.Resolve(ctx, n, cl.Candidates(b)); err != nil {
		return err
	}
	res := cl.Classify(b)

	if dumpDir != "" {
		if err := dumpFixture(dumpDir, cfg.Chain.Name, b, reg.DrainNew(), oracle.RefPool(), seed, stderr); err != nil {
			return err
		}
	}
	return json.MarshalWrite(stdout, newReport(&res), jsontext.WithIndent("  "))
}

func dumpFixture(dir, chain string, b *eth.Block, pools []dex.Pool, ref eth.Address, seed *uint256.Int, stderr io.Writer) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	fx := fixture.Fixture{Chain: chain, Block: *b, Pools: pools, RefPool: ref}
	if seed != nil {
		fx.SqrtPrice = seed.Dec()
	}
	path := filepath.Join(dir, fmt.Sprintf("block-%d.json", uint64(b.Header.Number)))
	if err := fixture.Save(path, &fx); err != nil {
		return err
	}
	fmt.Fprintln(stderr, "fixture written to", path)
	return nil
}

type report struct {
	Block       uint64        `json:"block"`
	Hash        string        `json:"hash"`
	Regime      string        `json:"regime"`
	Txs         uint32        `json:"txs"`
	Timeboosted uint32        `json:"timeboosted_txs"`
	Swaps       uint32        `json:"swaps_on_canonical_pools"`
	Arbs        []reportedArb `json:"arbitrages"`
}

type reportedArb struct {
	Tx           string   `json:"tx"`
	Status       string   `json:"status"`
	Sender       string   `json:"sender"`
	Contract     string   `json:"contract"`
	Hops         uint8    `json:"hops"`
	Pools        []string `json:"pools,omitempty"`
	ProfitToken  string   `json:"profit_token,omitempty"`
	ProfitTokens uint8    `json:"profit_tokens,omitempty"`
	ProfitRaw    string   `json:"profit_raw,omitempty"`
	ProfitETH    *float64 `json:"profit_eth,omitempty"`
	CostETH      float64  `json:"cost_eth"`
	PriorityGwei float64  `json:"priority_fee_gwei"`
	GasUsed      uint64   `json:"gas_used"`
	Timeboosted  bool     `json:"timeboosted"`
}

func newReport(res *classify.Result) report {
	r := report{
		Block: res.Block.Number, Hash: res.Block.Hash.Hex(), Regime: res.Block.Regime.String(),
		Txs: res.Block.TxCount, Timeboosted: res.Block.TimeboostedTxs, Swaps: res.Block.Swaps,
		Arbs: []reportedArb{},
	}
	for i := range res.Arbs {
		a := &res.Arbs[i]
		ra := reportedArb{
			Tx: a.TxHash.Hex(), Status: a.Status.String(), Sender: a.From.Hex(), Contract: a.To.Hex(), Hops: a.Hops,
			CostETH: a.CostETH, PriorityGwei: float64(a.PriorityFeePerGas) / 1e9, GasUsed: a.GasUsed, Timeboosted: a.Timeboosted,
		}
		for _, p := range a.Pools {
			ra.Pools = append(ra.Pools, p.Hex())
		}
		if a.Status == classify.StatusSuccess {
			ra.ProfitToken, ra.ProfitRaw, ra.ProfitTokens = a.ProfitToken.Hex(), a.Profit.Dec(), a.ProfitTokens
			if a.Valued {
				v := a.ProfitETH
				ra.ProfitETH = &v
			}
		}
		r.Arbs = append(r.Arbs, ra)
	}
	return r
}
