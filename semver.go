package binkit

import (
	"cmp"
	"strconv"
	"strings"
)

// compareVersions orders two dotted numeric versions such as "0.15.1" or "v0.16.0",
// returning -1, 0, or +1 in the manner of [cmp.Compare]. A leading "v" is ignored,
// absent components count as zero, build metadata is disregarded, and a prerelease
// sorts before the release it precedes ("1.0.0-rc1" < "1.0.0").
//
// This is deliberately not a general semver implementation. binkit only ever needs to
// answer "is upstream newer than the pin", which does not justify depending on
// golang.org/x/mod for one comparison.
func compareVersions(a, b string) int {
	numsA, preA := parseVersion(a)
	numsB, preB := parseVersion(b)

	for i := range numsA {
		if c := cmp.Compare(numsA[i], numsB[i]); c != 0 {
			return c
		}
	}

	switch {
	case preA == preB:
		return 0
	case preA == "":
		return 1 // a is a release, b is a prerelease of it
	case preB == "":
		return -1
	default:
		return cmp.Compare(preA, preB)
	}
}

// parseVersion splits "v1.2.3-rc1+build" into its numeric components and prerelease
// suffix. A malformed component stops parsing rather than erroring, so an unreadable
// version simply sorts low — the safe outcome for an update check, which should stay
// quiet rather than nag about a version it failed to understand.
func parseVersion(s string) ([3]int, string) {
	var nums [3]int

	base, pre, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(s), "v"), "-")
	base, _, _ = strings.Cut(base, "+")

	i := 0
	for part := range strings.SplitSeq(base, ".") {
		if i >= len(nums) {
			break
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			break
		}
		nums[i] = n
		i++
	}

	return nums, pre
}
