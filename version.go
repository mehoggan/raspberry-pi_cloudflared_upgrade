package main

import (
	"fmt"
	"regexp"
	"strconv"
)

// cloudflaredVersion is a cloudflared release number such as 2026.10.7
// (year.month.patch).
type cloudflaredVersion struct {
	year, month, patch int
}

var versionPattern = regexp.MustCompile(`(\d{4})\.(\d{1,2})\.(\d+)`)

// parseVersion finds the first year.month.patch number in text, so it accepts
// both "2026.10.7" and "cloudflared version 2026.10.7 (built ...)".
func parseVersion(text string) (cloudflaredVersion, error) {
	match := versionPattern.FindStringSubmatch(text)
	if match == nil {
		return cloudflaredVersion{}, fmt.Errorf("no cloudflared version found in %q", text)
	}
	year, _ := strconv.Atoi(match[1])
	month, _ := strconv.Atoi(match[2])
	patch, _ := strconv.Atoi(match[3])
	return cloudflaredVersion{year: year, month: month, patch: patch}, nil
}

func (v cloudflaredVersion) isZero() bool {
	return v == cloudflaredVersion{}
}

// olderThan reports whether v is an earlier release than other.
func (v cloudflaredVersion) olderThan(other cloudflaredVersion) bool {
	if v.year != other.year {
		return v.year < other.year
	}
	if v.month != other.month {
		return v.month < other.month
	}
	return v.patch < other.patch
}

func (v cloudflaredVersion) String() string {
	if v.isZero() {
		return "unknown"
	}
	return fmt.Sprintf("%d.%d.%d", v.year, v.month, v.patch)
}
