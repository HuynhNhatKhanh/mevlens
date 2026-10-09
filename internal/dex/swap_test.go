package dex

import (
	"testing"

	"github.com/holiman/uint256"

	"github.com/huynhnhatkhanh/mevlens/internal/eth"
)

func TestTopicsMatchPublishedValues(t *testing.T) {
	// Values as indexed by Etherscan for UniswapV2Pair / UniswapV3Pool.
	tests := map[string]struct {
		got  eth.Hash
		want string
	}{
		"v2 Swap": {TopicV2Swap, "0xd78ad95fa46c994b6551d0da85fc275fe613ce37657fb8d5e3d130840159d822"},
		"v2 Sync": {TopicV2Sync, "0x1c411e9a96e071241c2f21f7726b17ae89e3cab4c78be50e062b03a9fffbbad1"},
		"v3 Swap": {TopicV3Swap, "0xc42079f94a6350d7e6235f29174924f928cc2ac818eb64fed8004e115fbcca67"},
		// Observed on Arbitrum One from PancakeSwap v3 pools (factory 0x0BFbCF9f…1865).
		"PancakeSwap v3 Swap": {TopicPancakeV3Swap, "0x19b47279256b2a23a1665c810c8d55a1758940ee09377d4f8d26497a3577dc83"},
	}
	for name, tt := range tests {
		if tt.got.Hex() != tt.want {
			t.Errorf("%s topic = %s, want %s", name, tt.got, tt.want)
		}
	}
}

func word(v int64) []byte {
	var z uint256.Int
	if v < 0 {
		z.SetUint64(uint64(-v))
		z.Neg(&z)
	} else {
		z.SetUint64(uint64(v))
	}
	b := z.Bytes32()
	return b[:]
}

func concat(words ...[]byte) []byte {
	var out []byte
	for _, w := range words {
		out = append(out, w...)
	}
	return out
}

var pool = eth.MustAddress("0x0000000000000000000000000000000000000abc")

func TestDecodeV2Swap(t *testing.T) {
	l := &eth.Log{
		Address:  pool,
		Topics:   []eth.Hash{TopicV2Swap, {}, {}},
		Data:     concat(word(1000), word(0), word(0), word(2990)),
		LogIndex: 7,
	}
	s, ok := DecodeSwap(l)
	if !ok || s.Kind != KindV2 || s.LogIndex != 7 || s.Pool != PoolIDFromAddress(pool) {
		t.Fatalf("decode failed: %+v", s)
	}
	zeroForOne, in, out, ok := s.Direction()
	if !ok || !zeroForOne || in.Uint64() != 1000 || out.Uint64() != 2990 {
		t.Fatalf("direction = %v %d %d %v", zeroForOne, in.Uint64(), out.Uint64(), ok)
	}
}

func TestDecodeV3SwapNegativeAmounts(t *testing.T) {
	// Trader sends 5e17 of token1 into the pool and receives 1234 of token0.
	l := &eth.Log{
		Address: pool,
		Topics:  []eth.Hash{TopicV3Swap, {}, {}},
		Data:    concat(word(-1234), word(500_000_000_000_000_000), word(79228162514264337), word(1), word(-5)),
	}
	s, ok := DecodeSwap(l)
	if !ok || s.Kind != KindV3 {
		t.Fatal("decode failed")
	}
	if s.Out0.Uint64() != 1234 || !s.In0.IsZero() || s.In1.Uint64() != 500_000_000_000_000_000 || !s.Out1.IsZero() {
		t.Fatalf("amounts: in0=%s out0=%s in1=%s out1=%s", s.In0.Dec(), s.Out0.Dec(), s.In1.Dec(), s.Out1.Dec())
	}
	zeroForOne, in, out, ok := s.Direction()
	if !ok || zeroForOne || in.Uint64() != 500_000_000_000_000_000 || out.Uint64() != 1234 {
		t.Fatalf("direction = %v %s %s", zeroForOne, in.Dec(), out.Dec())
	}
	if s.SqrtPriceX96.Uint64() != 79228162514264337 {
		t.Fatalf("sqrtPrice = %s", s.SqrtPriceX96.Dec())
	}
}

func TestDecodePancakeV3Swap(t *testing.T) {
	// Same amounts and signs as Uniswap v3; the two trailing protocol-fee words are
	// not part of the pool-side flow.
	l := &eth.Log{
		Address: pool,
		Topics:  []eth.Hash{TopicPancakeV3Swap, {}, {}},
		Data:    concat(word(-1234), word(500_000_000_000_000_000), word(79228162514264337), word(1), word(-5), word(7), word(9)),
	}
	s, ok := DecodeSwap(l)
	if !ok || s.Kind != KindV3 || KindOfTopic(l.Topics[0]) != KindV3 || s.Pool != PoolIDFromAddress(pool) {
		t.Fatalf("decode = %+v, %v", s, ok)
	}
	if s.Out0.Uint64() != 1234 || !s.In0.IsZero() || s.In1.Uint64() != 500_000_000_000_000_000 || !s.Out1.IsZero() {
		t.Fatalf("amounts: in0=%s out0=%s in1=%s out1=%s", s.In0.Dec(), s.Out0.Dec(), s.In1.Dec(), s.Out1.Dec())
	}
	if s.SqrtPriceX96.Uint64() != 79228162514264337 {
		t.Fatalf("sqrtPrice = %s", s.SqrtPriceX96.Dec())
	}
}

func TestDecodeRejectsMalformed(t *testing.T) {
	cases := map[string]*eth.Log{
		"wrong topic":     {Topics: []eth.Hash{TopicV2Sync, {}, {}}, Data: make([]byte, 128)},
		"short v2 data":   {Topics: []eth.Hash{TopicV2Swap, {}, {}}, Data: make([]byte, 96)},
		"v3 topic v2 len": {Topics: []eth.Hash{TopicV3Swap, {}, {}}, Data: make([]byte, 128)},
		// Each v3 topic only with its own data length.
		"v3 topic pancake len": {Topics: []eth.Hash{TopicV3Swap, {}, {}}, Data: make([]byte, 224)},
		"pancake topic v3 len": {Topics: []eth.Hash{TopicPancakeV3Swap, {}, {}}, Data: make([]byte, 160)},
		"missing topics":       {Topics: []eth.Hash{TopicV2Swap}, Data: make([]byte, 128)},
		"removed":              {Topics: []eth.Hash{TopicV2Swap, {}, {}}, Data: make([]byte, 128), Removed: true},
	}
	for name, l := range cases {
		if _, ok := DecodeSwap(l); ok {
			t.Errorf("%s: decoded a malformed log", name)
		}
	}
}

func TestDirectionDegenerate(t *testing.T) {
	var s Swap
	s.In0.SetUint64(10)
	s.In1.SetUint64(10)
	if _, _, _, ok := s.Direction(); ok {
		t.Fatal("both sides in must be degenerate")
	}
}

func BenchmarkDecodeV3Swap(b *testing.B) {
	l := &eth.Log{
		Address: pool,
		Topics:  []eth.Hash{TopicV3Swap, {}, {}},
		Data:    concat(word(-1234), word(5e17), word(79228162514264337), word(1), word(-5)),
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := DecodeSwap(l); !ok {
			b.Fatal("decode")
		}
	}
}

func TestV4TopicsMatchPublishedValues(t *testing.T) {
	// Observed on Arbitrum One from the PoolManager 0x360e68fa…fb32.
	if got := TopicV4Swap.Hex(); got != "0x40e9cecb9f5f1f1c5b9c97dec2917b7ee92e57ba5563708daca94dd84ad7112f" {
		t.Errorf("v4 Swap topic = %s", got)
	}
	if got := TopicV4Initialize.Hex(); got != "0xdd466e674ea557f56295e2d0218a125ea4b4f0f6f3307b95f85e6110838d6438" {
		t.Errorf("v4 Initialize topic = %s", got)
	}
}

var (
	manager = eth.MustAddress("0x360e68faccca8ca495c1b759fd9eee466db9fb32")
	v4ID    = eth.MustHash("0x3e0dfbaee0c581f9b5c53e44461e21a51e9e06b67e8a0defd5d068a2fd423dab")
)

func TestDecodeV4SwapUsesSwapperSign(t *testing.T) {
	// Real swap (tx 0xeac9…): the swapper received 4419590917081865 wei of native
	// ETH (currency0, positive) and paid 5611771093812306903 of currency1 (negative).
	l := &eth.Log{
		Address: manager,
		Topics:  []eth.Hash{TopicV4Swap, v4ID, {}},
		Data:    concat(word(4419590917081865), negWord("5611771093812306903"), word(1), word(1), word(1), word(3000)),
	}
	s, ok := DecodeSwap(l)
	if !ok || s.Kind != KindV4 || s.Pool != PoolID(v4ID) || s.Contract != manager {
		t.Fatalf("decode = %+v %v", s, ok)
	}
	// Pool-side view: currency1 entered the pool, currency0 left it.
	if s.Out0.Uint64() != 4419590917081865 || !s.In0.IsZero() || s.In1.Dec() != "5611771093812306903" || !s.Out1.IsZero() {
		t.Fatalf("amounts: in0=%s out0=%s in1=%s out1=%s", s.In0.Dec(), s.Out0.Dec(), s.In1.Dec(), s.Out1.Dec())
	}
	if zeroForOne, _, _, _ := s.Direction(); zeroForOne {
		t.Fatal("swapper paid currency1: direction must be oneForZero")
	}
}

func negWord(dec string) []byte {
	z, _ := uint256.FromDecimal(dec)
	z.Neg(z)
	b := z.Bytes32()
	return b[:]
}

func TestDecodeV4Initialize(t *testing.T) {
	token := eth.MustAddress("0x88a269df8fe7f53e590c561954c52fccc8ec0cfb")
	hooks := eth.MustAddress("0x0000000000000000000000000000000000000c0c")
	l := &eth.Log{
		Address: manager,
		Topics:  []eth.Hash{TopicV4Initialize, v4ID, {}, token.Word()}, // currency0 = native ETH
		Data:    concat(word(3000), word(60), func() []byte { w := hooks.Word(); return w[:] }(), word(1), word(-5)),
	}
	p, ok := DecodeV4Initialize(l)
	if !ok || p.ID != PoolID(v4ID) || p.Contract != manager || !p.Token0.IsZero() || p.Token1 != token || p.FeePips != 3000 || p.DynamicFee || p.Hooks != hooks {
		t.Fatalf("pool = %+v %v", p, ok)
	}
	// Dynamic-fee pool, like mainnet ETH/USDC 0x0155… in the golden fixtures: the
	// PoolKey fee is the flag 0x800000, which must not surface as 8,388,608 pips.
	copy(l.Data[0:32], word(V4DynamicFeeFlag))
	if p, ok := DecodeV4Initialize(l); !ok || !p.DynamicFee || p.FeePips != 0 {
		t.Fatalf("dynamic-fee pool = %+v %v", p, ok)
	}
	l.Data = l.Data[:64]
	if _, ok := DecodeV4Initialize(l); ok {
		t.Fatal("short data accepted")
	}
}

func TestPoolIDText(t *testing.T) {
	a := eth.MustAddress("0x82af49447d8a07e3bd95bd0d56f35241523fbab1")
	for _, id := range []PoolID{PoolIDFromAddress(a), PoolID(v4ID)} {
		b, _ := id.MarshalText()
		var back PoolID
		if err := back.UnmarshalText(b); err != nil || back != id {
			t.Fatalf("round trip %s: %v", b, err)
		}
	}
	if s := PoolIDFromAddress(a).String(); s != a.Hex() {
		t.Fatalf("address-form id renders as %s", s)
	}
	if _, ok := PoolID(v4ID).Address(); ok {
		t.Fatal("a v4 id is not an address")
	}
}
