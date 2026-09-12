package bencode

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// SPEC §6 — stream contract
//
// This is the invariant that keeps a decoder honest. A desynced decoder
// corrupts every value after its first mistake, and nothing else in this file
// would notice.
//
// Draft 1 promised that ErrTypeMismatch and ErrOverflow left the stream
// aligned. That guarantee was only true for a mismatch at the top level of a
// value: a mismatch on the third pair of a ten-pair dict leaves seven pairs
// and a terminator unread. §6.2 records the reason this decoder cannot offer
// what encoding/json does — it is single-pass, where json delimits a complete
// value with a scanner before touching the destination. So the table below is
// the inverse of draft 1's: every one of these inputs poisons.
// ---------------------------------------------------------------------------

func TestSpec6_1_EveryErrorPoisons(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		bad  string
		dst  func() any
	}{
		// Type mismatches. Every one of these is a complete, well-formed value
		// the destination cannot hold — draft 1 expected recovery from all of
		// them.
		{"list into int", "li1ee", func() any { return new(int) }},
		{"dict into int", "d1:a1:be", func() any { return new(int) }},
		{"dict into slice", "d1:a1:be", func() any { return new([]string) }},
		{"int into slice", "i1e", func() any { return new([]string) }},
		{"int into string", "i1e", func() any { return new(string) }},
		{"string into map", "4:spam", func() any { return new(map[string]string) }},
		{"string into int", "4:spam", func() any { return new(int) }},
		{"nested dict into int", "d1:ad1:bl1:ceee", func() any { return new(int) }},
		{"short string into byte array", "4:spam", func() any { return new([20]byte) }},
		{"short list into array", "l1:a1:be", func() any { return new([3]string) }},
		{"long list into array", "l1:a1:b1:ce", func() any { return new([1]string) }},
		// This one exposed the contradiction inside draft 1: §4 required the
		// non-string key type to be rejected BEFORE any entry was decoded,
		// which leaves the stream just past the 'd' — while §6 claimed the
		// whole value had been consumed. Both cannot hold. §4 wins; the
		// decoder is poisoned.
		{"non-string key map", "d1:11:ae", func() any { return new(map[int]string) }},
		{"mismatch inside a container", "d1:sli1eee", func() any { return new(simple) }},

		// Overflow.
		{"overflow into narrow int", "i300e", func() any { return new(int8) }},
		{"overflow beyond int64", "i99999999999999999999e", func() any { return new(int64) }},

		// Syntax.
		{"unknown type byte", "x", func() any { return new(any) }},
		{"malformed int", "iabce", func() any { return new(any) }},
		{"non-string dict key", "di1ei2ee", func() any { return new(any) }},

		// Truncation.
		{"truncated list", "l", func() any { return new(any) }},
		{"truncated dict value", "d3:fooi42", func() any { return new(any) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := &countingReader{src: strings.NewReader(tt.bad + "i42e")}
			d := NewDecoder(r)

			first := decodeWith(t, d, tt.dst())
			require.Error(t, first)
			consumed := r.read

			var after int
			second := decodeWith(t, d, &after)
			require.Error(t, second,
				"a poisoned decoder must not pretend it found the next value")
			assert.Equal(t, first, second, "the stored error is returned verbatim")
			assert.Zero(t, after, "the destination must be untouched")
			assert.Equal(t, consumed, r.read, "a poisoned decoder must not read further")
		})
	}
}

func TestSpec6_1_LimitErrorsPoison(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		bad  string
	}{
		{"nesting over the limit", nest(64)},
		{"string over the limit", "100:" + repeat("x", 100)},
		{"value over the limit", "l" + repeat("i0e", 200) + "e"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := tinyDecoder(tt.bad + "i42e")

			first := decodeWith(t, d, new(any))
			require.Error(t, first)

			var after int
			require.Error(t, decodeWith(t, d, &after))
			assert.Zero(t, after)
		})
	}
}

// SPEC §6.1: "the decoder stores the error and every later Decode returns THAT
// SAME error". Identity, not equality — a decoder that rebuilds an equivalent
// error on each call is doing work it promised not to do, and would read the
// stream to do it.
func TestSpec6_1_PoisonedErrorIsIdentical(t *testing.T) {
	t.Parallel()

	d := NewDecoder(strings.NewReader("x" + repeat("i42e", 100)))

	first := decodeWith(t, d, new(any))
	require.Error(t, first)

	second := decodeWith(t, d, new(any))
	require.Error(t, second)
	assert.Same(t, first, second)
}

// SPEC §6.3: the escape hatch. A caller who wants to tolerate a value of an
// unexpected shape decodes into `any` first, which can never produce a type
// error and therefore never poisons.
func TestSpec6_3_DecodingIntoAnyNeverTypeErrors(t *testing.T) {
	t.Parallel()

	d := NewDecoder(strings.NewReader("li1eed1:a1:bei42e"))

	var first any
	require.NoError(t, decodeWith(t, d, &first))
	assert.Equal(t, []any{int64(1)}, first)

	var second any
	require.NoError(t, decodeWith(t, d, &second))
	assert.Equal(t, map[string]any{"a": "b"}, second)

	var third int
	require.NoError(t, decodeWith(t, d, &third))
	assert.Equal(t, 42, third)
}

func TestSpec6_1_SuccessivePositioning(t *testing.T) {
	t.Parallel()

	d := NewDecoder(strings.NewReader("i1e4:spamli2eed1:s1:xe"))

	var a int
	require.NoError(t, decodeWith(t, d, &a))
	assert.Equal(t, 1, a)

	var b string
	require.NoError(t, decodeWith(t, d, &b))
	assert.Equal(t, "spam", b)

	var c []int
	require.NoError(t, decodeWith(t, d, &c))
	assert.Equal(t, []int{2}, c)

	var e simple
	require.NoError(t, decodeWith(t, d, &e))
	assert.Equal(t, simple{S: "x"}, e)

	var f int
	assert.ErrorIs(t, decodeWith(t, d, &f), io.EOF)
}

// SPEC §6.4 — Unmarshal is the one place the leniency of §2.1 is withdrawn.
// A caller who handed over a finite byte slice asserted that the slice IS the
// message, so anything after the value is an error.
func TestSpec6_4_Unmarshal(t *testing.T) {
	t.Parallel()

	t.Run("decodes a single value", func(t *testing.T) {
		t.Parallel()
		var got simple
		require.NoError(t, Unmarshal([]byte("d1:s4:spam1:ii7ee"), &got))
		assert.Equal(t, simple{S: "spam", I: 7}, got)
	})

	t.Run("rejects trailing bytes", func(t *testing.T) {
		t.Parallel()
		var got int
		err := Unmarshal([]byte("i42egarbage"), &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrSyntax)

		var se *SyntaxError
		require.ErrorAs(t, err, &se)
		assert.Equal(t, int64(4), se.Offset, "the offset of the first trailing byte")
	})

	// bencode permits whitespace nowhere, so a stray newline is trailing data
	// like any other byte. Worth its own case because every other format
	// tolerates it and the habit carries over.
	t.Run("rejects trailing whitespace", func(t *testing.T) {
		t.Parallel()
		var got int
		err := Unmarshal([]byte("i42e\n"), &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrSyntax)
	})

	t.Run("accepts a second value's worth of nothing", func(t *testing.T) {
		t.Parallel()
		var got []string
		require.NoError(t, Unmarshal([]byte("l1:a1:be"), &got))
		assert.Equal(t, []string{"a", "b"}, got)
	})

	t.Run("propagates the same errors as Decode", func(t *testing.T) {
		t.Parallel()
		var got int
		assert.ErrorIs(t, Unmarshal([]byte("4:spam"), &got), ErrTypeMismatch)
		assert.ErrorIs(t, Unmarshal([]byte("x"), &got), ErrSyntax)
		assert.ErrorIs(t, Unmarshal([]byte("i42"), &got), io.ErrUnexpectedEOF)
		assert.ErrorIs(t, Unmarshal([]byte("i42e"), got), ErrInvalidDestination)
	})
}
