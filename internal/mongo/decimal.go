package mongo

import (
	"encoding/binary"
	"errors"
	"math/big"
	"strconv"
	"strings"
)

// Decimal128 (IEEE 754-2008, binary integer encoding) as MongoDB stores it: 16 bytes,
// little-endian. The text form follows the specification's to-scientific-string, as
// the shell prints it; parsing never rounds (a value that does not fit is refused).

const (
	decBias   = 6176
	decMinExp = -6176
	decMaxExp = 6111
	decDigits = 34
)

var (
	decMaxCoef = new(big.Int).Sub(new(big.Int).Exp(big.NewInt(10), big.NewInt(decDigits), nil), big.NewInt(1))
	mask64     = new(big.Int).SetUint64(^uint64(0))
)

// decimalText returns the text of a Decimal128 and whether it is canonical (a NaN or
// infinity with payload bits, or a coefficient out of range, does not read back the
// same).
func decimalText(b []byte) (string, bool) {
	lo := binary.LittleEndian.Uint64(b)
	hi := binary.LittleEndian.Uint64(b[8:])
	neg := hi>>63 == 1
	sign := ""
	if neg {
		sign = "-"
	}
	var exp int
	coef := new(big.Int)
	canonical := true
	switch {
	case hi>>58&0x1f == 0x1f:
		return "NaN", hi == 0x7c00000000000000 && lo == 0
	case hi>>58&0x1f == 0x1e:
		return sign + "Infinity", hi&^(1<<63) == 0x7800000000000000 && lo == 0
	case hi>>61&3 == 3:
		// The large-coefficient form: always more than 34 digits, so the value is zero.
		exp = int(hi>>47&0x3fff) - decBias
		canonical = false
	default:
		exp = int(hi>>49&0x3fff) - decBias
		coef.SetUint64(hi & (1<<49 - 1))
		coef.Lsh(coef, 64)
		coef.Or(coef, new(big.Int).SetUint64(lo))
		if coef.Cmp(decMaxCoef) > 0 {
			coef.SetInt64(0)
			canonical = false
		}
	}
	digits := coef.String()
	adjusted := exp + len(digits) - 1
	var s string
	switch {
	case exp <= 0 && adjusted >= -6:
		if exp == 0 {
			s = digits
		} else if pad := -exp; len(digits) > pad {
			s = digits[:len(digits)-pad] + "." + digits[len(digits)-pad:]
		} else {
			s = "0." + strings.Repeat("0", pad-len(digits)) + digits
		}
	default:
		s = digits[:1]
		if len(digits) > 1 {
			s += "." + digits[1:]
		}
		s += "E"
		if adjusted >= 0 {
			s += "+"
		}
		s += strconv.Itoa(adjusted)
	}
	return sign + s, canonical
}

var errDecimal = errors.New("geçersiz ondalık sayı")

// parseDecimal reads the text of a Decimal128.
func parseDecimal(s string) ([16]byte, error) {
	var out [16]byte
	put := func(hi, lo uint64) [16]byte {
		binary.LittleEndian.PutUint64(out[:], lo)
		binary.LittleEndian.PutUint64(out[8:], hi)
		return out
	}
	neg := false
	rest := s
	if rest != "" && (rest[0] == '+' || rest[0] == '-') {
		neg = rest[0] == '-'
		rest = rest[1:]
	}
	var signBit uint64
	if neg {
		signBit = 1 << 63
	}
	switch strings.ToLower(rest) {
	case "inf", "infinity":
		return put(signBit|0x7800000000000000, 0), nil
	case "nan":
		if neg {
			return out, errDecimal
		}
		return put(0x7c00000000000000, 0), nil
	}
	mant, expPart, hasExp := strings.Cut(strings.ToLower(rest), "e")
	intPart, frac, _ := strings.Cut(mant, ".")
	digits := intPart + frac
	if digits == "" || len(digits) > 6200 || strings.Count(mant, ".") > 1 {
		return out, errDecimal
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return out, errDecimal
		}
	}
	exp := 0
	if hasExp {
		e := expPart
		eneg := false
		if e != "" && (e[0] == '+' || e[0] == '-') {
			eneg = e[0] == '-'
			e = e[1:]
		}
		if e == "" || len(e) > 6 {
			return out, errDecimal
		}
		for i := 0; i < len(e); i++ {
			if e[i] < '0' || e[i] > '9' {
				return out, errDecimal
			}
			exp = exp*10 + int(e[i]-'0')
		}
		if eneg {
			exp = -exp
		}
	}
	exp -= len(frac)
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		digits = "0"
	}
	if len(digits) > decDigits {
		return out, errors.New("ondalık sayı en çok 34 basamak olabilir (yuvarlama yapılmaz)")
	}
	coef, _ := new(big.Int).SetString(digits, 10)
	zero := coef.Sign() == 0
	switch {
	case exp > decMaxExp && zero:
		exp = decMaxExp
	case exp > decMaxExp:
		// Exact: shift the coefficient left while it has room ("clamping").
		for exp > decMaxExp && len(coef.String()) < decDigits {
			coef.Mul(coef, big.NewInt(10))
			exp--
		}
		if exp > decMaxExp {
			return out, errors.New("ondalık sayı çok büyük")
		}
	case exp < decMinExp && zero:
		exp = decMinExp
	case exp < decMinExp:
		return out, errors.New("ondalık sayı çok küçük (yuvarlama yapılmaz)")
	}
	hi := new(big.Int).Rsh(coef, 64).Uint64()
	lo := new(big.Int).And(coef, mask64).Uint64()
	return put(signBit|uint64(exp+decBias)<<49|hi, lo), nil
}
