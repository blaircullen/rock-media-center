package automation

import "testing"

// A comparison used for sorting must be a strict weak ordering, or sort.Slice can
// misbehave. Equal values must not compare less than each other in either direction.
func TestNaturalLessIsConsistent(t *testing.T) {
	for _, s := range []string{"a", "Chapter 1.mp3", "01", "1", ""} {
		if naturalLess(s, s) {
			t.Errorf("naturalLess(%q, %q) must be false — a value is not less than itself", s, s)
		}
	}
	pairs := [][2]string{{"Chapter 2.mp3", "Chapter 10.mp3"}, {"a", "b"}, {"1", "2"}}
	for _, p := range pairs {
		if !naturalLess(p[0], p[1]) {
			t.Errorf("%q should sort before %q", p[0], p[1])
		}
		if naturalLess(p[1], p[0]) {
			t.Errorf("%q must not also sort before %q — the order has to be asymmetric", p[1], p[0])
		}
	}
}
