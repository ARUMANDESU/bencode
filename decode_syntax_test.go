package bencode

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	bencodeast "github.com/arumandesu/bencode/pkg/bencode_ast"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// SPEC §2 — accepted grammar, and the leniency rules
// ---------------------------------------------------------------------------

func TestSpec2_Integers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want int64
	}{
		{"positive", "i42e", 42},
		{"negative", "i-7e", -7},
		{"zero", "i0e", 0},
		{"max int64", "i9223372036854775807e", 9223372036854775807},
		{"min int64", "i-9223372036854775808e", -9223372036854775808},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got int64
			require.NoError(t, decode(t, tt.in, &got))
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSpec2_Strings(t *testing.T) {
	t.Parallel()

	t.Run("plain", func(t *testing.T) {
		t.Parallel()
		var got string
		require.NoError(t, decode(t, "4:spam", &got))
		assert.Equal(t, "spam", got)
	})

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		var got string
		require.NoError(t, decode(t, "0:", &got))
		assert.Equal(t, "", got)
	})

	// SPEC §4 promises a non-nil empty slice for an empty LIST and says
	// nothing about an empty string. Pinning it explicitly rather than with
	// assert.Empty, which passes for nil too: the difference surfaces later in
	// reflect.DeepEqual and in JSON round-trips, and "undecided" is not an
	// answer a caller can code against.
	t.Run("empty string into []byte yields a non-nil empty slice", func(t *testing.T) {
		t.Parallel()
		var got []byte
		require.NoError(t, decode(t, "0:", &got))
		assert.NotNil(t, got, "an empty bencode string is a value, not an absence")
		assert.Len(t, got, 0)
	})

	t.Run("payload containing bencode metacharacters is not parsed", func(t *testing.T) {
		t.Parallel()
		var got string
		require.NoError(t, decode(t, "6:d3:abce", &got))
		assert.Equal(t, "d3:abc", got)
	})

	t.Run("arbitrary bytes survive", func(t *testing.T) {
		t.Parallel()
		var got []byte
		require.NoError(t, decode(t, "4:\x00\xff\xfe\x01", &got))
		assert.Equal(t, []byte{0x00, 0xff, 0xfe, 0x01}, got)
	})
}

func TestSpec2_1_Leniency(t *testing.T) {
	t.Parallel()

	t.Run("unsorted keys are accepted", func(t *testing.T) {
		t.Parallel()
		var got map[string]string
		require.NoError(t, decode(t, "d1:b1:x1:a1:ye", &got))
		assert.Equal(t, map[string]string{"a": "y", "b": "x"}, got)
	})

	t.Run("duplicate keys: last one wins, map", func(t *testing.T) {
		t.Parallel()
		var got map[string]string
		require.NoError(t, decode(t, "d1:a1:x1:a1:ye", &got))
		assert.Equal(t, map[string]string{"a": "y"}, got)
	})

	t.Run("duplicate keys: last one wins, struct", func(t *testing.T) {
		t.Parallel()
		var got simple
		require.NoError(t, decode(t, "d1:s5:first1:s6:seconde", &got))
		assert.Equal(t, "second", got.S)
	})

	t.Run("trailing bytes are left for the next Decode", func(t *testing.T) {
		t.Parallel()
		d := NewDecoder(strings.NewReader("i42ei7e"))

		var a int64
		require.NoError(t, decodeWith(t, d, &a))
		assert.Equal(t, int64(42), a)

		var b int64
		require.NoError(t, decodeWith(t, d, &b))
		assert.Equal(t, int64(7), b)
	})

	t.Run("trailing garbage does not invalidate a complete value", func(t *testing.T) {
		t.Parallel()
		var got int64
		require.NoError(t, decode(t, "i42egarbage", &got))
		assert.Equal(t, int64(42), got)
	})

	t.Run("unknown dict keys are skipped", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{
			"s":         bencodeast.Str("kept"),
			"unknown":   bencodeast.Int(999),
			"also_skip": bencodeast.List{bencodeast.Str("x")},
		})

		var got simple
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, "kept", got.S)
	})

	t.Run("skipping a deeply nested unknown value stays in sync", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{
			"a_unknown": bencodeast.Dict{
				"deep": bencodeast.List{
					bencodeast.Dict{"x": bencodeast.List{bencodeast.Int(1), bencodeast.Str("y")}},
				},
			},
			"s": bencodeast.Str("after"),
		})

		var got simple
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, "after", got.S)
	})
}

// ---------------------------------------------------------------------------
// SPEC §5 — errors
// ---------------------------------------------------------------------------

func TestSpec5_Syntax(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
	}{
		{"unknown type byte", "x"},
		{"bare terminator", "e"},
		{"colon with no length", ":abc"},
		{"int not numeric", "iabce"},
		{"int sign in the middle", "i4-2e"},
		{"int lone minus", "i-e"},
		{"int trailing sign", "i4-e"},
		{"string length not numeric", "4x:spam"},
		{"string length with no colon at all", "4spam"},
		{"string length negative", "-1:x"},
		{"dict value is a terminator", "d3:fooe"},
		// bencode dict keys are byte strings by spec, so this is malformed
		// input — not a destination problem.
		{"dict key is an integer", "di1ei2ee"},
		{"dict key is a list", "dl1:aei1ee"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got any
			err := decode(t, tt.in, &got)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrSyntax)
		})
	}
}

// SPEC §5.1: the sub-sentinels say how the bytes were malformed and wrap
// ErrSyntax, so callers can classify on one axis while tests stay precise.
func TestSpec5_1_SubSentinels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want error
	}{
		{"int empty", "ie", ErrEmpty},
		{"int leading zero", "i03e", ErrLeadingZero},
		{"int negative zero", "i-0e", ErrNegativeZero},
		{"int negative leading zero", "i-03e", ErrNegativeZero},
		{"string length leading zero", "05:hello", ErrLeadingZero},
		{"string length zero padded", "0000004:spam", ErrLeadingZero},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got any
			err := decode(t, tt.in, &got)
			require.Error(t, err)
			assert.ErrorIs(t, err, tt.want, "specific cause")
			assert.ErrorIs(t, err, ErrSyntax, "must also classify as a syntax error")
		})
	}

	// ErrArrayLength is the one sub-sentinel that hangs off ErrTypeMismatch
	// rather than ErrSyntax: the bytes were fine, the type was fine, only the
	// width was wrong.
	t.Run("array length wraps ErrTypeMismatch", func(t *testing.T) {
		t.Parallel()
		var got [20]byte
		err := decode(t, "4:spam", &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrArrayLength)
		assert.ErrorIs(t, err, ErrTypeMismatch)
		assert.NotErrorIs(t, err, ErrSyntax, "well-formed bytes are not a syntax error")
	})
}

func TestSpec5_Overflow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		dst  func() any
	}{
		{"beyond int64", "i99999999999999999999e", func() any { return new(int64) }},
		{"below int64", "i-99999999999999999999e", func() any { return new(int64) }},
		{"narrow signed", "i300e", func() any { return new(int8) }},
		{"narrow unsigned", "i300e", func() any { return new(uint8) }},
		{"negative into unsigned", "i-5e", func() any { return new(uint64) }},
		{"beyond uint64", "i18446744073709551616e", func() any { return new(uint64) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := decode(t, tt.in, tt.dst())
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrOverflow)
		})
	}

	t.Run("through a struct field", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{"u": bencodeast.Int(-5)})

		var got unsigned
		err := decode(t, input, &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrOverflow)
	})
}

// SPEC §5.2 — structured errors.
//
// Sentinels classify; they do not locate. "syntax error: unexpected 'x'" with
// no position sends the caller hexdumping the whole file. These tests pin the
// offsets, which is also the cheapest standing check on the consumed-offset
// counter of §9.4 — the same counter RawMessage capture and MaxValueBytes both
// depend on.
func TestSpec5_2_StructuredErrors(t *testing.T) {
	t.Parallel()

	t.Run("SyntaxError carries the offset of the offending byte", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name string
			in   string
			want int64
		}{
			{"first byte", "x", 0},
			// SPEC §2: a non-string dict key is detected from the FIRST byte
			// of the key. An offset past the integer would mean the decoder
			// parsed the key before noticing it could not be one.
			{"non-string dict key", "di1ei2ee", 1},
			{"inside a list", "l4:spamxe", 7},
			{"inside a dict value", "d1:sxe", 4},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				var got any
				err := decode(t, tt.in, &got)
				require.Error(t, err)

				var se *SyntaxError
				require.ErrorAs(t, err, &se)
				assert.Equal(t, tt.want, se.Offset)
				assert.ErrorIs(t, se, ErrSyntax)
			})
		}
	})

	// SPEC §5.2: offsets count from the first byte the Decoder ever read, not
	// from the start of the current value. Anything else is useless for
	// locating a fault in a stream.
	t.Run("offsets are stream-global, not per-value", func(t *testing.T) {
		t.Parallel()
		d := NewDecoder(strings.NewReader("i42ex"))

		var first int
		require.NoError(t, decodeWith(t, d, &first))

		var second any
		err := decodeWith(t, d, &second)
		require.Error(t, err)

		var se *SyntaxError
		require.ErrorAs(t, err, &se)
		assert.Equal(t, int64(4), se.Offset,
			"the bad byte is at stream offset 4, not offset 0 of the second value")
	})

	t.Run("TypeError carries the value kind, destination and offset", func(t *testing.T) {
		t.Parallel()

		t.Run("top level", func(t *testing.T) {
			t.Parallel()
			var got string
			err := decode(t, "i1e", &got)
			require.Error(t, err)

			var te *TypeError
			require.ErrorAs(t, err, &te)
			assert.Equal(t, int64(0), te.Offset)
			assert.Equal(t, "integer", te.Value)
			assert.Equal(t, reflect.TypeFor[string](), te.Type)
			assert.ErrorIs(t, te, ErrTypeMismatch)
		})

		t.Run("offset points at the start of the offending value", func(t *testing.T) {
			t.Parallel()
			// d 1 : s l 1 : a e e
			// 0 1 2 3 4 5 6 7 8 9   — the list begins at 4.
			var got simple
			err := decode(t, "d1:sl1:aee", &got)
			require.Error(t, err)

			var te *TypeError
			require.ErrorAs(t, err, &te)
			assert.Equal(t, int64(4), te.Offset)
			assert.Equal(t, "list", te.Value)
			assert.Equal(t, reflect.TypeFor[string](), te.Type)
		})

		// SPEC §12 lists Struct/Field as an open decision. If they are dropped
		// from TypeError, delete this subtest with them — do not weaken it to
		// "may or may not be set", which asserts nothing.
		t.Run("struct and field name when reached through a struct", func(t *testing.T) {
			t.Parallel()
			var got simple
			err := decode(t, "d1:sl1:aee", &got)
			require.Error(t, err)

			var te *TypeError
			require.ErrorAs(t, err, &te)
			assert.Equal(t, "simple", te.Struct)
			assert.Equal(t, "S", te.Field)
		})

		// SPEC §5.2: for a promoted field, Struct names the type that DECLARES
		// it, not the one being decoded into. The outer type is visible at the
		// call site; the declaring type is the one the reader has to hunt for.
		t.Run("a promoted field reports its declaring type", func(t *testing.T) {
			t.Parallel()
			var got liftsExported
			err := decode(t, "d1:sl1:aee", &got)
			require.Error(t, err)

			var te *TypeError
			require.ErrorAs(t, err, &te)
			assert.Equal(t, "Exported", te.Struct, "not liftsExported")
			assert.Equal(t, "S", te.Field)
		})

		t.Run("overflow is also a TypeError", func(t *testing.T) {
			t.Parallel()
			var got int8
			err := decode(t, "i300e", &got)
			require.Error(t, err)

			var te *TypeError
			require.ErrorAs(t, err, &te)
			assert.Equal(t, "integer", te.Value)
			assert.Equal(t, reflect.TypeFor[int8](), te.Type)
			assert.ErrorIs(t, te, ErrOverflow)
		})
	})

	t.Run("LimitError names the limit it hit", func(t *testing.T) {
		t.Parallel()

		t.Run("MaxStringBytes", func(t *testing.T) {
			t.Parallel()
			var got []byte
			err := decodeWith(t, tinyDecoder("100:"+repeat("x", 100)), &got)
			require.Error(t, err)

			var le *LimitError
			require.ErrorAs(t, err, &le)
			assert.Equal(t, "MaxStringBytes", le.Limit)
			assert.Equal(t, int64(100), le.Value, "the declared length that was refused")
			assert.ErrorIs(t, le, ErrExceedsMax)
		})

		t.Run("MaxDepth", func(t *testing.T) {
			t.Parallel()
			var got any
			err := decodeWith(t, tinyDecoder(nest(64)), &got)
			require.Error(t, err)

			var le *LimitError
			require.ErrorAs(t, err, &le)
			assert.Equal(t, "MaxDepth", le.Limit)
			assert.ErrorIs(t, le, ErrMaxDepth)
		})

		t.Run("MaxValueBytes", func(t *testing.T) {
			t.Parallel()
			var got any
			err := decodeWith(t, tinyDecoder("l"+repeat("i0e", 200)+"e"), &got)
			require.Error(t, err)

			var le *LimitError
			require.ErrorAs(t, err, &le)
			assert.Equal(t, "MaxValueBytes", le.Limit)
			assert.ErrorIs(t, le, ErrExceedsMax)
		})
	})
}

// SPEC §5.3: io.EOF means "no more values"; io.ErrUnexpectedEOF means "broken
// input". Conflating them is how a truncated file looks like a clean one.
func TestSpec5_3_EOF(t *testing.T) {
	t.Parallel()

	t.Run("empty input is a clean end of stream", func(t *testing.T) {
		t.Parallel()
		var got any
		assert.ErrorIs(t, decode(t, "", &got), io.EOF)
	})

	t.Run("end of stream after a complete value", func(t *testing.T) {
		t.Parallel()
		d := NewDecoder(strings.NewReader("i1e"))

		var first int
		require.NoError(t, decodeWith(t, d, &first))

		var second int
		assert.ErrorIs(t, decodeWith(t, d, &second), io.EOF)
	})

	// SPEC §5.3: io.EOF is sticky. This is what makes the standard
	// `for { if err == io.EOF { break } }` loop terminate instead of spinning
	// on a decoder that has run dry.
	t.Run("io.EOF is returned forever", func(t *testing.T) {
		t.Parallel()
		d := NewDecoder(strings.NewReader("i1e"))

		var first int
		require.NoError(t, decodeWith(t, d, &first))

		for i := range 3 {
			var v any
			assert.ErrorIs(t, decodeWith(t, d, &v), io.EOF, "call %d", i+2)
		}
	})

	truncated := []struct {
		name string
		in   string
	}{
		{"int missing terminator", "i42"},
		{"int missing everything", "i"},
		// Note "4spam" does NOT belong here: 's' is an invalid byte in a
		// length prefix, which is a syntax error no matter how much data
		// follows. A truncation is valid bytes that simply stop.
		{"string length runs out", "4"},
		{"string shorter than declared", "10:abc"},
		{"string missing payload", "4:"},
		{"list unterminated", "l"},
		{"list unterminated with element", "li1e"},
		{"dict unterminated", "d"},
		{"dict key with no value", "d3:foo"},
		{"dict value truncated", "d3:fooi42"},
	}

	for _, tt := range truncated {
		t.Run("truncated: "+tt.name, func(t *testing.T) {
			t.Parallel()
			var got any
			err := decode(t, tt.in, &got)
			require.Error(t, err)
			assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
			assert.NotErrorIs(t, err, io.EOF,
				"a value cut in half is not a clean end of stream")
		})
	}
}

// SPEC §5.1: strconv, reflect and every other implementation detail stays
// inside. An error a caller cannot classify is an error a caller cannot handle.
func TestSpec5_NoUnclassifiableErrors(t *testing.T) {
	t.Parallel()

	sentinels := []error{
		ErrSyntax, ErrTypeMismatch, ErrOverflow,
		ErrInvalidDestination, ErrMaxDepth, ErrExceedsMax,
		io.EOF, io.ErrUnexpectedEOF,
	}

	corpus := []string{
		"", "x", "e", ":abc", "-1:x",
		"i", "i42", "ie", "iabce", "i-e", "i4-2e", "i03e", "i-0e",
		"i99999999999999999999e", "i" + repeat("1", 200) + "e",
		"4spam", "4:", "10:abc", "05:hello", "0000004:spam", "9000000:short",
		"l", "li1e", "le", "l4:spame",
		"d", "d3:foo", "d3:fooe", "di1ei2ee", "de", "d1:s4:spame",
		nest(200), "l" + repeat("i0e", 500) + "e",
	}

	for _, in := range corpus {
		t.Run(fmt.Sprintf("%q", truncateName(in)), func(t *testing.T) {
			t.Parallel()
			var got any
			err := decodeWith(t, tinyDecoder(in), &got)
			if err == nil {
				return
			}
			for _, s := range sentinels {
				if errors.Is(err, s) {
					return
				}
			}
			t.Fatalf("error %#v matches no sentinel in SPEC §5.1", err)
		})
	}
}
