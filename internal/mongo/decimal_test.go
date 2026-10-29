package mongo

import (
	"encoding/binary"
	"math/big"
	"math/rand/v2"
	"testing"
)

func decBytes(hi, lo uint64) []byte {
	b := binary.LittleEndian.AppendUint64(nil, lo)
	return binary.LittleEndian.AppendUint64(b, hi)
}

func TestDecimalText(t *testing.T) {
	cases := []struct {
		hi, lo uint64
		want   string
	}{
		{0x3040000000000000, 0, "0"},
		{0xb040000000000000, 0, "-0"},
		{0x3040000000000000, 1, "1"},
		{0xb040000000000000, 1, "-1"},
		{0x303e000000000000, 1, "0.1"},
		{0x303c000000000000, 1250, "12.50"},
		{0x3034000000000000, 1, "0.000001"},
		{0x3032000000000000, 1, "1E-7"},
		{0x3046000000000000, 1, "1E+3"},
		{0x3044000000000000, 10, "1.0E+3"},
		{0x303c000000000000, 0, "0.00"},
		{0x3046000000000000, 0, "0E+3"},
		{0x7800000000000000, 0, "Infinity"},
		{0xf800000000000000, 0, "-Infinity"},
		{0x7c00000000000000, 0, "NaN"},
		// 10^34-1 at the largest exponent.
		{0x5fffed09bead87c0, 0x378d8e63ffffffff, "9.999999999999999999999999999999999E+6144"},
	}
	for _, c := range cases {
		got, ok := decimalText(decBytes(c.hi, c.lo))
		if got != c.want || !ok {
			t.Errorf("%x %x: %q %v, want %q", c.hi, c.lo, got, ok, c.want)
		}
		b, err := parseDecimal(c.want)
		if err != nil || binary.LittleEndian.Uint64(b[8:]) != c.hi || binary.LittleEndian.Uint64(b[:8]) != c.lo {
			t.Errorf("parse %q: %x %v", c.want, b, err)
		}
	}
}

func TestDecimalNonCanonical(t *testing.T) {
	for _, b := range [][]byte{
		decBytes(0x7c00000000000000, 1),                  // NaN with payload
		decBytes(0xfc00000000000000, 0),                  // negative NaN
		decBytes(0x6c10000000000000, 0),                  // large-coefficient form
		decBytes(0x3041ed09bead87c0, 0x378d8e6400000000), // coefficient 10^34
	} {
		if _, ok := decimalText(b); ok {
			t.Errorf("%x reported canonical", b)
		}
	}
}

func TestParseDecimalForms(t *testing.T) {
	same := map[string]string{
		"+1.5": "1.5", ".5": "0.5", "5.": "5", "1e3": "1E+3", "1.23E-2": "0.0123",
		"-0.0": "-0.0", "inf": "Infinity", "-INFINITY": "-Infinity", "nan": "NaN", "00012": "12",
	}
	for in, want := range same {
		b, err := parseDecimal(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got, _ := decimalText(b[:]); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "-", ".", "1.2.3", "1e", "1e+", "abc", " 1", "1 ", "0x10", "-NaN",
		"12345678901234567890123456789012345", "1E+7000", "1E-7000", "1e1234567"} {
		if _, err := parseDecimal(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	// Zero is clamped instead of refused, and a big exponent is folded into the
	// coefficient when that is exact.
	if b, err := parseDecimal("0E+7000"); err != nil {
		t.Error(err)
	} else if s, _ := decimalText(b[:]); s != "0E+6111" {
		t.Errorf("0E+7000 → %s", s)
	}
	if b, err := parseDecimal("1E+6144"); err != nil {
		t.Error(err)
	} else if s, _ := decimalText(b[:]); s != "1.000000000000000000000000000000000E+6144" {
		t.Errorf("1E+6144 → %s", s)
	}
}

// Every canonical value reads back as the same bytes.
func TestDecimalRoundTripRandom(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	limit := new(big.Int).Add(decMaxCoef, big.NewInt(1))
	for range 20000 {
		coef := new(big.Int).SetUint64(r.Uint64())
		coef.Lsh(coef, 64).Or(coef, new(big.Int).SetUint64(r.Uint64()))
		coef.Mod(coef, limit)
		if r.IntN(4) == 0 {
			coef.SetInt64(r.Int64N(1000))
		}
		exp := r.IntN(decMaxExp-decMinExp+1) + decMinExp
		if r.IntN(2) == 0 {
			exp = r.IntN(40) - 20
		}
		hi := new(big.Int).Rsh(coef, 64).Uint64() | uint64(exp+decBias)<<49
		if r.IntN(2) == 0 {
			hi |= 1 << 63
		}
		lo := new(big.Int).And(coef, mask64).Uint64()
		s, ok := decimalText(decBytes(hi, lo))
		if !ok {
			t.Fatalf("%x %x not canonical", hi, lo)
		}
		b, err := parseDecimal(s)
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if binary.LittleEndian.Uint64(b[8:]) != hi || binary.LittleEndian.Uint64(b[:8]) != lo {
			t.Fatalf("%q read back as %x, want %x %x", s, b, hi, lo)
		}
	}
}
