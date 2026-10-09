-- Camelot v3 (Algebra) pools emit the Uniswap v3 Swap event but have no fee():
-- before Algebra factories were supported, that revert cached every one of them
-- as "not a pool", and a cached pool is never resolved again. Forget the
-- non-canonical v3 pools so each is resolved once more: genuine Algebra pools
-- become canonical, the rest (impostors, unconfigured forks) are cached again.
DELETE FROM pools WHERE kind = 'v3' AND NOT canonical;
