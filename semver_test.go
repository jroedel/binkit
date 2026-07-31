package binkit

import "testing"

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want int
	}{
		{"equal", "1.2.3", "1.2.3", 0},
		{"equal despite v prefix", "v1.2.3", "1.2.3", 0},
		{"patch newer", "0.15.1", "0.15.0", 1},
		{"patch older", "0.15.0", "0.15.1", -1},
		{"minor dominates patch", "0.16.0", "0.15.99", 1},
		{"major dominates minor", "1.0.0", "0.99.99", 1},
		{"missing components are zero", "1", "1.0.0", 0},
		{"missing components still compare", "1.1", "1.0.9", 1},
		{"prerelease precedes release", "1.0.0-rc1", "1.0.0", -1},
		{"release follows prerelease", "1.0.0", "1.0.0-rc1", 1},
		{"prereleases compare lexically", "1.0.0-rc1", "1.0.0-rc2", -1},
		{"build metadata ignored", "1.2.3+build9", "1.2.3+build1", 0},
		{"typst style tags", "v0.15.1", "v0.16.0", -1},
		{"garbage sorts low", "not-a-version", "0.0.1", -1},
		{"two garbage values are equal", "xyz", "abc", 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := compareVersions(tc.a, tc.b); got != tc.want {
				t.Errorf("compareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestCompareVersionsIsAntisymmetric guards the ordering contract: swapping the
// arguments must negate the result, or a caller comparing in either direction gets
// inconsistent answers.
func TestCompareVersionsIsAntisymmetric(t *testing.T) {
	versions := []string{"0.0.1", "0.15.0", "0.15.1", "0.16.0", "1.0.0-rc1", "1.0.0", "2.3.4"}

	for _, a := range versions {
		for _, b := range versions {
			forward := compareVersions(a, b)
			reverse := compareVersions(b, a)
			if forward != -reverse {
				t.Errorf("compareVersions(%q,%q)=%d but compareVersions(%q,%q)=%d; want negation",
					a, b, forward, b, a, reverse)
			}
		}
	}
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		in       string
		wantNums [3]int
		wantPre  string
	}{
		{"1.2.3", [3]int{1, 2, 3}, ""},
		{"v0.15.1", [3]int{0, 15, 1}, ""},
		{"  v2.0  ", [3]int{2, 0, 0}, ""},
		{"1.0.0-rc1", [3]int{1, 0, 0}, "rc1"},
		{"1.0.0+meta", [3]int{1, 0, 0}, ""},
		{"1.2.3.4", [3]int{1, 2, 3}, ""}, // fourth component ignored
		{"", [3]int{0, 0, 0}, ""},
		{"junk", [3]int{0, 0, 0}, ""},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			nums, pre := parseVersion(tc.in)
			if nums != tc.wantNums || pre != tc.wantPre {
				t.Errorf("parseVersion(%q) = %v,%q; want %v,%q", tc.in, nums, pre, tc.wantNums, tc.wantPre)
			}
		})
	}
}
