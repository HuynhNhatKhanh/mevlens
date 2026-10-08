package registry

import (
	"context"
	"fmt"
	"slices"

	"github.com/huynhnhatkhanh/mevlens/internal/dex"
	"github.com/huynhnhatkhanh/mevlens/internal/eth"
	"github.com/huynhnhatkhanh/mevlens/internal/rpc"
)

// maxLogSpan is the widest eth_getLogs block range tried first. Arbitrum's public
// endpoint caps a query at 10,000,000 blocks; narrower limits are discovered at
// runtime by halving the range.
const maxLogSpan = 10_000_000

// minLogSpan bounds the halving so a provider that rejects everything fails fast.
const minLogSpan = 1_000

// Managers returns the configured v4 PoolManagers.
func (r *Registry) Managers() []Factory {
	out := make([]Factory, 0, len(r.managers))
	for _, m := range r.managers {
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b Factory) int { return a.Address.Compare(b.Address) })
	return out
}

// V4Synced returns the highest block whose Initialize logs are indexed for manager.
func (r *Registry) V4Synced(manager eth.Address) uint64 { return r.v4Synced[manager] }

// SetV4Synced restores the index height, e.g. from a checkpoint at startup.
func (r *Registry) SetV4Synced(manager eth.Address, block uint64) { r.v4Synced[manager] = block }

// SyncV4 indexes the Initialize logs of every configured PoolManager up to block
// `to`, so swaps on pools created before the pipeline started can be resolved.
// It resumes from V4Synced and can be interrupted; progress is reported after
// each scanned range. New pools are returned by DrainNew.
func (r *Registry) SyncV4(ctx context.Context, to uint64, progress func(manager Factory, through uint64, found int) error) error {
	for _, m := range r.Managers() {
		from := m.StartBlock
		if synced := r.v4Synced[m.Address]; synced >= from {
			from = synced + 1
		}
		span := uint64(maxLogSpan)
		for from <= to {
			end := min(from+span-1, to)
			logs, err := r.caller.Logs(ctx, rpc.LogQuery{
				Address: m.Address, From: from, To: end,
				Topics: [][]eth.Hash{{dex.TopicV4Initialize}},
			})
			if err != nil {
				if rpc.IsLogRangeError(err) && span > minLogSpan {
					span /= 2 // the provider wants a narrower query
					continue
				}
				return fmt.Errorf("registry: sync %s Initialize logs [%d, %d]: %w", m.Name, from, end, err)
			}
			for i := range logs {
				if p, ok := dex.DecodeV4Initialize(&logs[i]); ok {
					p.FirstSeen = uint64(logs[i].BlockNumber)
					r.addV4(p)
				}
			}
			r.v4Synced[m.Address] = end
			if progress != nil {
				if err := progress(m, end, len(logs)); err != nil {
					return err
				}
			}
			from = end + 1
		}
	}
	return nil
}
