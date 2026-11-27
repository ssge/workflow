package feel

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// decimal represents sign * digits * 10^exp. Nonzero digits have neither
// leading nor trailing zeros, so precision and adjusted exponent are explicit.
type decimal struct {
	digits string
	exp    int
	neg    bool
}

func parseDecimal(s string) (decimal, error) {
	bad := func() (decimal, error) { return decimal{}, fmt.Errorf("invalid or unrepresentable decimal %q", s) }
	if len(s) == 0 {
		return bad()
	}
	i, neg := 0, false
	if s[i] == '-' {
		neg = true
		i++
	}
	if i == len(s) || s[i] < '0' || s[i] > '9' {
		return bad()
	}
	start := i
	if s[i] == '0' {
		i++
		if i < len(s) && s[i] >= '0' && s[i] <= '9' {
			return bad()
		}
	} else {
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	}
	digits := s[start:i]
	fraction := 0
	if i < len(s) && s[i] == '.' {
		i++
		start = i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if start == i {
			return bad()
		}
		fraction = i - start
		digits += s[start:i]
	}
	exponent := 0
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		expNeg := false
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			expNeg = s[i] == '-'
			i++
		}
		start = i
		// Anything beyond this cap cannot be brought into range by the
		// decimal point or trailing zeros present in this input.
		limit := len(s) + 20000
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			d := int(s[i] - '0')
			if exponent > (limit-d)/10 {
				exponent = limit
			} else {
				exponent = exponent*10 + d
			}
			i++
		}
		if start == i {
			return bad()
		}
		if expNeg {
			exponent = -exponent
		}
	}
	if i != len(s) {
		return bad()
	}
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return decimal{digits: "0"}, nil
	}
	trimmed := strings.TrimRight(digits, "0")
	exponent += len(digits) - len(trimmed) - fraction
	digits = trimmed
	adjusted := exponent + len(digits) - 1
	if len(digits) > 34 || adjusted < -6143 || adjusted > 6144 {
		return bad()
	}
	return decimal{digits: digits, exp: exponent, neg: neg}, nil
}

func (d decimal) String() string {
	if d.digits == "0" {
		return "0"
	}
	sign := ""
	if d.neg {
		sign = "-"
	}
	if d.exp >= 0 {
		return sign + d.digits + strings.Repeat("0", d.exp)
	}
	pos := len(d.digits) + d.exp
	if pos > 0 {
		return sign + d.digits[:pos] + "." + d.digits[pos:]
	}
	return sign + "0." + strings.Repeat("0", -pos) + d.digits
}

func (d decimal) compare(other decimal) int {
	if d.digits == "0" && other.digits == "0" {
		return 0
	}
	if d.neg != other.neg {
		if d.neg {
			return -1
		}
		return 1
	}
	// Zero has no meaningful adjusted exponent.
	if d.digits == "0" {
		return -1
	}
	if other.digits == "0" {
		return 1
	}
	a, b := len(d.digits)+d.exp, len(other.digits)+other.exp
	cmp := 0
	switch {
	case a < b:
		cmp = -1
	case a > b:
		cmp = 1
	default:
		for i := 0; i < len(d.digits) || i < len(other.digits); i++ {
			x, y := byte('0'), byte('0')
			if i < len(d.digits) {
				x = d.digits[i]
			}
			if i < len(other.digits) {
				y = other.digits[i]
			}
			if x < y {
				cmp = -1
				break
			}
			if x > y {
				cmp = 1
				break
			}
		}
	}
	if d.neg {
		return -cmp
	}
	return cmp
}

func (d decimal) add(other decimal) (decimal, error) {
	exp := min(d.exp, other.exp)
	integer := func(n decimal) *big.Int {
		v, _ := new(big.Int).SetString(n.digits, 10)
		if n.exp > exp {
			power := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n.exp-exp)), nil)
			v.Mul(v, power)
		}
		if n.neg {
			v.Neg(v)
		}
		return v
	}
	sum := integer(d)
	sum.Add(sum, integer(other))
	return parseDecimal(sum.String() + "e" + strconv.Itoa(exp))
}
