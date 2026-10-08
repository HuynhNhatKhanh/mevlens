package eth

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"testing"
)

func TestAddressRoundTrip(t *testing.T) {
	const in = "0x82aF49447D8a07e3bd95BD0d56f35241523fBab1"
	a, err := ParseAddress(in)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := a.Hex(), "0x82af49447d8a07e3bd95bd0d56f35241523fbab1"; got != want {
		t.Fatalf("Hex() = %s, want %s", got, want)
	}
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var back Address
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back != a {
		t.Fatalf("round trip mismatch: %s != %s", back, a)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		in   string
		want error
	}{
		{"82af49447d8a07e3bd95bd0d56f35241523fbab1", ErrMissingPrefix},
		{"0x82af", ErrLength},
		{"0x82af49447d8a07e3bd95bd0d56f35241523fbab1aa", ErrLength},
	}
	for _, tt := range tests {
		if _, err := ParseAddress(tt.in); !errors.Is(err, tt.want) {
			t.Errorf("ParseAddress(%q) err = %v, want %v", tt.in, err, tt.want)
		}
	}
	if _, err := ParseAddress("0xzz" + "00000000000000000000000000000000000000"); err == nil {
		t.Error("expected error for non-hex digits")
	}
}

func TestJSONNullDecodesToZero(t *testing.T) {
	var r struct {
		To Address `json:"to"`
	}
	r.To = MustAddress("0x82af49447d8a07e3bd95bd0d56f35241523fbab1")
	if err := json.Unmarshal([]byte(`{"to":null}`), &r); err != nil {
		t.Fatal(err)
	}
	if !r.To.IsZero() {
		t.Fatalf("null should decode to zero address, got %s", r.To)
	}
}

func TestQuantity(t *testing.T) {
	tests := []struct {
		in      string
		want    uint64
		wantErr bool
	}{
		{`"0x0"`, 0, false},
		{`"0x1a"`, 26, false},
		{`"0x01"`, 1, false}, // tolerated leading zero
		{`"0xffffffffffffffff"`, 1<<64 - 1, false},
		{`"0x"`, 0, true},
		{`"1a"`, 0, true},
		{`"0x10000000000000000"`, 0, true}, // overflow
	}
	for _, tt := range tests {
		var q Quantity
		err := json.Unmarshal([]byte(tt.in), &q)
		if (err != nil) != tt.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if err == nil && uint64(q) != tt.want {
			t.Errorf("%s: got %d, want %d", tt.in, q, tt.want)
		}
	}
	b, _ := json.Marshal(Quantity(255))
	if string(b) != `"0xff"` {
		t.Errorf("marshal = %s", b)
	}
}

func TestData(t *testing.T) {
	var d Data
	if err := json.Unmarshal([]byte(`"0x"`), &d); err != nil || len(d) != 0 {
		t.Fatalf("empty data: %v %x", err, d)
	}
	if err := json.Unmarshal([]byte(`"0x0a0b"`), &d); err != nil || !bytes.Equal(d, []byte{0x0a, 0x0b}) {
		t.Fatalf("data: %v %x", err, d)
	}
	if err := json.Unmarshal([]byte(`"0x0a0"`), &d); err == nil {
		t.Fatal("odd-length data must fail")
	}
}

func TestKeccakKnownVectors(t *testing.T) {
	// Well-known topics/selectors, independently published on Etherscan and in the
	// Uniswap/OpenZeppelin sources.
	tests := []struct {
		sig  string
		want string
	}{
		{"Transfer(address,address,uint256)", "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"},
		{"Sync(uint112,uint112)", "0x1c411e9a96e071241c2f21f7726b17ae89e3cab4c78be50e062b03a9fffbbad1"},
	}
	for _, tt := range tests {
		if got := EventTopic(tt.sig).Hex(); got != tt.want {
			t.Errorf("EventTopic(%s) = %s, want %s", tt.sig, got, tt.want)
		}
	}
	if got := NewSelector("transfer(address,uint256)"); got != (Selector{0xa9, 0x05, 0x9c, 0xbb}) {
		t.Errorf("selector = %x", got)
	}
}

func TestCalldataAndWords(t *testing.T) {
	a := MustAddress("0x82af49447d8a07e3bd95bd0d56f35241523fbab1")
	cd := NewSelector("getPair(address,address)").Calldata(a.Word(), Uint64Word(500))
	if len(cd) != 4+64 {
		t.Fatalf("calldata len = %d", len(cd))
	}
	w0, ok := WordAt(cd[4:], 0)
	if !ok || w0.Address() != a {
		t.Fatalf("word 0 = %s", w0)
	}
	w1, _ := WordAt(cd[4:], 1)
	if w1[31] != 0xf4 || w1[30] != 0x01 {
		t.Fatalf("uint word = %x", w1)
	}
	if _, ok := WordAt(cd[4:], 2); ok {
		t.Fatal("out-of-range word must fail")
	}
}

func FuzzHashRoundTrip(f *testing.F) {
	f.Add(make([]byte, 32))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) != HashLength {
			return
		}
		var h Hash
		copy(h[:], b)
		back, err := ParseHash(h.Hex())
		if err != nil || back != h {
			t.Fatalf("round trip failed: %v", err)
		}
	})
}

func BenchmarkAddressUnmarshalText(b *testing.B) {
	in := []byte("0x82af49447d8a07e3bd95bd0d56f35241523fbab1")
	var a Address
	b.ReportAllocs()
	for b.Loop() {
		if err := a.UnmarshalText(in); err != nil {
			b.Fatal(err)
		}
	}
}
