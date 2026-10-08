package rpc

import (
	"context"
	"fmt"

	"github.com/huynhnhatkhanh/mevlens/internal/eth"
)

// Latest is the "latest" block tag.
const Latest = "latest"

// ChainID returns eth_chainId.
func (c *Client) ChainID(ctx context.Context) (uint64, error) {
	var q eth.Quantity
	if err := c.Call(ctx, &q, "eth_chainId"); err != nil {
		return 0, err
	}
	return uint64(q), nil
}

// BlockNumber returns the latest block number known to the serving endpoint.
func (c *Client) BlockNumber(ctx context.Context) (uint64, error) {
	var q eth.Quantity
	if err := c.Call(ctx, &q, "eth_blockNumber"); err != nil {
		return 0, err
	}
	return uint64(q), nil
}

// BlockWithReceipts fetches a header and all its receipts in a single HTTP batch and checks
// that both halves describe the same block. Inconsistent answers (common behind
// load balancers near the chain head) return ErrInconsistent, which is retryable.
func (c *Client) BlockWithReceipts(ctx context.Context, n uint64) (*eth.Block, error) {
	b := new(eth.Block)
	tag := eth.FormatBlock(n)
	reqs := []Request{
		{Method: "eth_getBlockByNumber", Params: []any{tag, false}, Result: &b.Header},
		{Method: "eth_getBlockReceipts", Params: []any{tag}, Result: &b.Receipts},
	}
	if err := c.Batch(ctx, reqs); err != nil {
		return nil, err
	}
	for _, r := range reqs {
		if r.Err != nil {
			return nil, fmt.Errorf("block %d: %s: %w", n, r.Method, r.Err)
		}
	}
	if err := validateBlock(n, b); err != nil {
		return nil, err
	}
	return b, nil
}

func validateBlock(n uint64, b *eth.Block) error {
	h := &b.Header
	if uint64(h.Number) != n {
		return fmt.Errorf("block %d: header number %d: %w", n, h.Number, ErrInconsistent)
	}
	if len(b.Receipts) != len(h.Transactions) {
		return fmt.Errorf("block %d: %d receipts for %d transactions: %w", n, len(b.Receipts), len(h.Transactions), ErrInconsistent)
	}
	for i := range b.Receipts {
		r := &b.Receipts[i]
		if r.BlockHash != h.Hash || r.TxHash != h.Transactions[i] || uint64(r.TxIndex) != uint64(i) {
			return fmt.Errorf("block %d: receipt %d does not match header: %w", n, i, ErrInconsistent)
		}
	}
	return nil
}

// CallMsg is the transaction object of eth_call.
type CallMsg struct {
	To   eth.Address `json:"to"`
	Data eth.Data    `json:"data"`
}

// NewCall builds a batched eth_call request whose raw return data is decoded into out.
func NewCall(to eth.Address, data eth.Data, block string, out *eth.Data) Request {
	return Request{Method: "eth_call", Params: []any{CallMsg{To: to, Data: data}, block}, Result: out}
}

// CallContract performs a single eth_call.
func (c *Client) CallContract(ctx context.Context, to eth.Address, data eth.Data, block string) (eth.Data, error) {
	var out eth.Data
	req := []Request{NewCall(to, data, block, &out)}
	if err := c.Batch(ctx, req); err != nil {
		return nil, err
	}
	return out, req[0].Err
}
