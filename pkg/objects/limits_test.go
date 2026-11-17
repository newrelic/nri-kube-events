package objects

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

func TestCapList(t *testing.T) {
	s, truncated := capList([]string{"a", "b"})
	assert.Equal(t, ",a,b,", s)
	assert.False(t, truncated)

	s, truncated = capList(nil)
	assert.Equal(t, "", s)
	assert.False(t, truncated)

	// 1,000 ASCII items of four runes: each joins as ",xxxx," and adds five runes after the
	// first, so the rune cap is reached first and whole items drop from the end.
	var ascii []string
	for i := 0; i < 1000; i++ {
		ascii = append(ascii, "abcd")
	}
	s, truncated = capList(ascii)
	assert.True(t, truncated)
	assert.LessOrEqual(t, utf8.RuneCountInString(s), PartChars)
	assert.LessOrEqual(t, len(s), MaxAttrBytes)
	assert.True(t, strings.HasPrefix(s, ",abcd,"))
	assert.True(t, strings.HasSuffix(s, ",abcd,"))
	assert.Equal(t, strings.Count(s, "abcd"), (utf8.RuneCountInString(s)-1)/5)

	// Items of three-byte runes stay far under the rune cap, so the byte cap drops them.
	var wide []string
	for i := 0; i < 1000; i++ {
		wide = append(wide, "中中中中")
	}
	s, truncated = capList(wide)
	assert.True(t, truncated)
	assert.LessOrEqual(t, len(s), MaxAttrBytes)
	assert.Less(t, utf8.RuneCountInString(s), PartChars)
	assert.True(t, strings.HasSuffix(s, ",中中中中,"))
	assert.Equal(t, strings.Count(s, "中中中中"), (len(s)-1)/13)

	// A first item that alone is too long leaves the result empty.
	s, truncated = capList([]string{strings.Repeat("x", PartChars), "b"})
	assert.Equal(t, "", s)
	assert.True(t, truncated)

	s, truncated = capList([]string{strings.Repeat("中", 1400)})
	assert.Equal(t, "", s)
	assert.True(t, truncated)
}
