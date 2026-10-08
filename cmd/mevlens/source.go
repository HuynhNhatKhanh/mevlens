package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/huynhnhatkhanh/mevlens/internal/eth"
	"github.com/huynhnhatkhanh/mevlens/internal/observe"
	"github.com/huynhnhatkhanh/mevlens/internal/rpc"
)

// rpcSource adapts *rpc.Client to observe.BlockSource, translating transport
// errors into the pipeline's vocabulary.
//
// *rpc.Client deliberately does NOT satisfy observe.BlockSource (its method is
// BlockWithReceipts, not Block): passing the raw client to observe.New is a
// compile error, so the error translation cannot be bypassed by accident.
type rpcSource struct{ c *rpc.Client }

var _ observe.BlockSource = rpcSource{}

func (s rpcSource) BlockNumber(ctx context.Context) (uint64, error) { return s.c.BlockNumber(ctx) }

func (s rpcSource) Block(ctx context.Context, n uint64) (*eth.Block, error) {
	b, err := s.c.BlockWithReceipts(ctx, n)
	if err != nil && (errors.Is(err, rpc.ErrNotFound) || rpc.IsRetryable(err)) {
		return nil, fmt.Errorf("%w: %w", observe.ErrUnavailable, err)
	}
	return b, err
}
