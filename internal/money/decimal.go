// Package money preserves JSON decimal numbers without floating-point arithmetic.
package money

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

var decimal = regexp.MustCompile(`^(-?)([0-9]+)(?:\.([0-9]+))?(?:[eE]([+-]?[0-9]+))?$`)

// Canonical expands exponents, strips insignificant zeroes and normalizes -0.
func Canonical(s string) (string, error) {
	m := decimal.FindStringSubmatch(s)
	if m == nil || len(s) > 4096 {
		return "", errors.New("invalid decimal amount")
	}
	exp := 0
	if m[4] != "" {
		var err error
		exp, err = strconv.Atoi(m[4])
		if err != nil || exp < -4096 || exp > 4096 {
			return "", errors.New("decimal exponent out of range")
		}
	}
	digits := m[2] + m[3]
	point := len(m[2]) + exp
	if point <= 0 {
		digits = strings.Repeat("0", 1-point) + digits
		point = 1
	}
	if point >= len(digits) {
		digits += strings.Repeat("0", point-len(digits))
		point = len(digits)
	}
	whole := strings.TrimLeft(digits[:point], "0")
	if whole == "" {
		whole = "0"
	}
	frac := strings.TrimRight(digits[point:], "0")
	result := whole
	if frac != "" {
		result += "." + frac
	}
	if result != "0" {
		result = m[1] + result
	}
	return result, nil
}

func Display(s string) string {
	if !strings.Contains(s, ".") {
		return s + ".00"
	}
	if len(s)-strings.IndexByte(s, '.') == 2 {
		return s + "0"
	}
	return s
}
