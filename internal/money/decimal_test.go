package money

import "testing"

func TestCanonical(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"positive", "42.50", "42.5"}, {"negative", "-123.45000", "-123.45"}, {"fractional", "0.000001234567890123456789", "0.000001234567890123456789"},
		{"large", "9007199254740993.01", "9007199254740993.01"}, {"exponent", "-1.234e-7", "-0.0000001234"}, {"positive exponent", "1.002e5", "100200"}, {"zero", "-0.000e12", "0"}, {"leading zeroes", "00042.000", "42"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Canonical(tc.in)
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	for _, in := range []string{"NaN", "Infinity", "", "1.2.3", "+2", "1e99999", "--1"} {
		if _, err := Canonical(in); err == nil {
			t.Errorf("accepted %q", in)
		}
	}
}
func FuzzCanonicalIdempotence(f *testing.F) {
	for _, s := range []string{"42.50", "-0.1", "1e-3", "9007199254740993.01"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out, err := Canonical(s)
		if err != nil {
			return
		}
		again, err := Canonical(out)
		if err != nil || again != out {
			t.Fatalf("noncanonical %q -> %q -> %q", s, out, again)
		}
	})
}
