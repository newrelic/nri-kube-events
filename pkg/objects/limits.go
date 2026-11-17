package objects

import (
	"strings"
	"unicode/utf8"
)

const (
	// PartChars is the most runes one part attribute holds, the mark included.
	PartChars = 4094
	// Mark prefixes every part. The Log API parses an attribute whose value is valid JSON
	// into flattened attributes and drops the string; the mark keeps every part invalid JSON.
	Mark = "~"
	// Overlap is how many runes consecutive parts share, so a search term up to Overlap
	// runes long sits whole inside at least one part.
	Overlap = 256
	// Step is how many new runes each part adds.
	Step = PartChars - len(Mark) - Overlap
	// MaxAttrBytes is the most bytes a string attribute holds. The forwarder truncates a
	// longer string by byte count.
	MaxAttrBytes = 4095
)

// capList joins items as ",a,b,", dropping whole items from the end so the result holds at
// most PartChars runes and MaxAttrBytes bytes. It reports whether any item was dropped. When
// the first item alone is too long, the result is "".
func capList(items []string) (string, bool) {
	if len(items) == 0 {
		return "", false
	}
	var b strings.Builder
	b.WriteString(",")
	runes, size := 1, 1
	for _, item := range items {
		n := utf8.RuneCountInString(item) + 1
		if runes+n > PartChars || size+len(item)+1 > MaxAttrBytes {
			if runes == 1 {
				return "", true
			}
			return b.String(), true
		}
		b.WriteString(item)
		b.WriteString(",")
		runes += n
		size += len(item) + 1
	}
	return b.String(), false
}
