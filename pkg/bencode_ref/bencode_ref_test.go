package bencoderef

import (
	"bytes"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"

	bencodeast "github.com/ARUMANDESU/gotorrent/pkg/bencode_ast"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests marked "FAILS TODAY" specify behaviour the decoder does not have yet.
// They are red on purpose; each one names the defect it pins down.

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

type TestStruct struct {
	F  string                 `bencode:"f"`
	F2 int64                  `bencode:"f2"`
	F3 []string               `bencode:"f3"`
	F4 map[string]string      `bencode:"f4"`
	F5 []TestStruct2          `bencode:"f5"`
	F6 map[string]TestStruct2 `bencode:"f6"`
}

type TestStruct2 struct {
	FF string `bencode:"ff"`
}

type NestedStruct struct {
	N     int64       `bencode:"n"`
	Inner TestStruct2 `bencode:"inner"`
}

type PtrFieldStruct struct {
	P *TestStruct2 `bencode:"p"`
}

type UnexportedStruct struct {
	Exported   string `bencode:"e"`
	unexported string `bencode:"u"`
}

type UntaggedStruct struct {
	Untagged string
	Tagged   string `bencode:"t"`
}

type UnsignedStruct struct {
	U uint64 `bencode:"u"`
}

type ShaStruct struct {
	Hash [20]byte `bencode:"hash"`
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// mustEncode builds bencode test fixtures via the already-tested bencode_ast
// encoder, instead of hand-writing raw bencode strings that are easy to get
// wrong (miscounted length prefixes, unbalanced e's, etc).
func mustEncode(t *testing.T, v bencodeast.Value) string {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, bencodeast.Encode(&buf, v))
	return buf.String()
}

// decode runs one value through a fresh Decoder.
//
// A panic is never correct behaviour for a decoder: callers cannot recover
// from one, and inside a test binary it tears down every other test. So a
// panic fails this test immediately and loudly. It is deliberately NOT
// converted into an error — an errors-are-fine helper makes a crash
// indistinguishable from a rejection, and every negative test then passes for
// the wrong reason.
func decode(t *testing.T, input string, dst any) error {
	t.Helper()
	return decodeWith(t, NewDecoder(strings.NewReader(input)), dst)
}

// decodeWith is decode against an existing Decoder, for tests that read
// several values from one stream.
func decodeWith(t *testing.T, d *Decoder, dst any) error {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("decoder panicked: %v", r)
		}
	}()
	return d.Decode(dst)
}

// countingReader reports how many bytes the decoder actually pulled. Used to
// prove the decoder gives up on a malformed value instead of buffering the
// rest of the stream while it looks for a terminator.
type countingReader struct {
	src  io.Reader
	read int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.src.Read(p)
	c.read += n
	return n, err
}

// allocatedBytes reports how much fn allocated in total.
func allocatedBytes(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// ---------------------------------------------------------------------------
// scalars
// ---------------------------------------------------------------------------

func TestDecoder_Decode_Scalar(t *testing.T) {
	t.Parallel()

	t.Run("int", func(t *testing.T) {
		t.Parallel()
		var got int
		require.NoError(t, decode(t, "i42e", &got))
		assert.Equal(t, 42, got)
	})

	t.Run("int64 negative", func(t *testing.T) {
		t.Parallel()
		var got int64
		require.NoError(t, decode(t, "i-7e", &got))
		assert.Equal(t, int64(-7), got)
	})

	t.Run("string", func(t *testing.T) {
		t.Parallel()
		var got string
		require.NoError(t, decode(t, "4:spam", &got))
		assert.Equal(t, "spam", got)
	})

	t.Run("[]byte gets the raw bytes, no text decoding", func(t *testing.T) {
		t.Parallel()
		var got []byte
		require.NoError(t, decode(t, "4:spam", &got))
		assert.Equal(t, []byte("spam"), got)
	})

	// FAILS TODAY: returns fmt.Errorf("non-pointer destionation TODO"), which
	// no caller can classify.
	t.Run("non-pointer destination is ErrInvalidDestination", func(t *testing.T) {
		t.Parallel()
		var got int
		err := decode(t, "i42e", got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidDestination)
	})

	// FAILS TODAY: reaches decodeInt's !CanSet branch and returns "TODO".
	t.Run("nil pointer destination is ErrInvalidDestination", func(t *testing.T) {
		t.Parallel()
		var got *int
		err := decode(t, "i42e", got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidDestination)
	})
}

// ---------------------------------------------------------------------------
// slices
// ---------------------------------------------------------------------------

func TestDecoder_Decode_Slice(t *testing.T) {
	t.Parallel()

	t.Run("[]string", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{bencodeast.Str("spam"), bencodeast.Str("lol")})

		var got []string
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, []string{"spam", "lol"}, got)
	})

	t.Run("[]int", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{bencodeast.Int(1), bencodeast.Int(2), bencodeast.Int(3)})

		var got []int
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, []int{1, 2, 3}, got)
	})

	t.Run("empty list", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{})

		var got []string
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, []string{}, got)
	})

	t.Run("nested list of structs", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{
			bencodeast.Dict{"ff": bencodeast.Str("x")},
			bencodeast.Dict{"ff": bencodeast.Str("y")},
		})

		var got []TestStruct2
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, []TestStruct2{{FF: "x"}, {FF: "y"}}, got)
	})
}

// ---------------------------------------------------------------------------
// maps
// ---------------------------------------------------------------------------

func TestDecoder_Decode_Map(t *testing.T) {
	t.Parallel()

	t.Run("map[string]string", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{"cow": bencodeast.Str("moo"), "spam": bencodeast.Str("eggs")})

		var got map[string]string
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, map[string]string{"cow": "moo", "spam": "eggs"}, got)
	})

	t.Run("map[string]int", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{"a": bencodeast.Int(1), "b": bencodeast.Int(2)})

		var got map[string]int
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, map[string]int{"a": 1, "b": 2}, got)
	})

	t.Run("map[string]struct", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{"y": bencodeast.Dict{"ff": bencodeast.Str("z")}})

		var got map[string]TestStruct2
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, map[string]TestStruct2{"y": {FF: "z"}}, got)
	})
}

// ---------------------------------------------------------------------------
// any / interface targets
// ---------------------------------------------------------------------------

func TestDecoder_Decode_Any(t *testing.T) {
	t.Parallel()

	t.Run("top-level int", func(t *testing.T) {
		t.Parallel()
		var got any
		require.NoError(t, decode(t, "i42e", &got))
		assert.Equal(t, int64(42), got)
	})

	t.Run("top-level string", func(t *testing.T) {
		t.Parallel()
		var got any
		require.NoError(t, decode(t, "4:spam", &got))
		assert.Equal(t, "spam", got)
	})

	t.Run("top-level list", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{bencodeast.Str("spam"), bencodeast.Int(1)})

		var got any
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, []any{"spam", int64(1)}, got)
	})

	t.Run("top-level dict", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{"cow": bencodeast.Str("moo")})

		var got any
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, map[string]any{"cow": "moo"}, got)
	})
}

func TestDecoder_Decode_AnyNesting(t *testing.T) {
	t.Parallel()

	t.Run("dict of list of dict", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{
			"files": bencodeast.List{
				bencodeast.Dict{"length": bencodeast.Int(1), "path": bencodeast.List{bencodeast.Str("a")}},
			},
		})

		var got any
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, map[string]any{
			"files": []any{
				map[string]any{"length": int64(1), "path": []any{"a"}},
			},
		}, got)
	})

	// Documents a real trap: binary blobs like `pieces` come back as strings
	// when decoded through any, so they must be converted with []byte(s).
	t.Run("binary data through any becomes a string", func(t *testing.T) {
		t.Parallel()
		var got any
		require.NoError(t, decode(t, "4:\x00\xff\x00\xff", &got))
		s, ok := got.(string)
		require.True(t, ok)
		assert.Equal(t, []byte{0x00, 0xff, 0x00, 0xff}, []byte(s))
	})

	t.Run("any field inside a struct", func(t *testing.T) {
		t.Parallel()
		type anyField struct {
			V any `bencode:"v"`
		}
		input := mustEncode(t, bencodeast.Dict{"v": bencodeast.List{bencodeast.Int(1)}})

		var got anyField
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, []any{int64(1)}, got.V)
	})
}

// ---------------------------------------------------------------------------
// structs
// ---------------------------------------------------------------------------

func TestDecoder_Decode_Struct(t *testing.T) {
	t.Parallel()

	input := mustEncode(t, bencodeast.Dict{
		"f":  bencodeast.Str("hello"),
		"f2": bencodeast.Int(42),
		"f3": bencodeast.List{bencodeast.Str("a"), bencodeast.Str("b")},
		"f4": bencodeast.Dict{"k": bencodeast.Str("v")},
		"f5": bencodeast.List{bencodeast.Dict{"ff": bencodeast.Str("x")}},
		"f6": bencodeast.Dict{"y": bencodeast.Dict{"ff": bencodeast.Str("z")}},
	})

	var got TestStruct
	require.NoError(t, decode(t, input, &got))

	assert.Equal(t, TestStruct{
		F:  "hello",
		F2: 42,
		F3: []string{"a", "b"},
		F4: map[string]string{"k": "v"},
		F5: []TestStruct2{{FF: "x"}},
		F6: map[string]TestStruct2{"y": {FF: "z"}},
	}, got)
}

func TestDecoder_Decode_UnknownFieldsAreIgnored(t *testing.T) {
	t.Parallel()

	input := mustEncode(t, bencodeast.Dict{
		"f":         bencodeast.Str("hello"),
		"unknown":   bencodeast.Int(999),
		"also_skip": bencodeast.List{bencodeast.Str("x")},
	})

	var got TestStruct
	require.NoError(t, decode(t, input, &got))
	assert.Equal(t, "hello", got.F)
}

func TestDecoder_Decode_StructShapes(t *testing.T) {
	t.Parallel()

	t.Run("plain nested struct field", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{
			"n":     bencodeast.Int(7),
			"inner": bencodeast.Dict{"ff": bencodeast.Str("deep")},
		})

		var got NestedStruct
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, NestedStruct{N: 7, Inner: TestStruct2{FF: "deep"}}, got)
	})

	t.Run("missing keys leave zero values", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{"f": bencodeast.Str("only")})

		var got TestStruct
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, TestStruct{F: "only"}, got)
	})

	// Documents current behaviour: fields absent from the input keep whatever
	// the destination already held. Worth deciding on deliberately.
	t.Run("absent keys do not clear a reused destination", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{"f2": bencodeast.Int(1)})

		got := TestStruct{F: "stale"}
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, "stale", got.F)
		assert.Equal(t, int64(1), got.F2)
	})

	t.Run("duplicate keys: last one wins", func(t *testing.T) {
		t.Parallel()
		var got TestStruct
		require.NoError(t, decode(t, "d1:f5:first1:f6:seconde", &got))
		assert.Equal(t, "second", got.F)
	})

	t.Run("unexported fields are ignored", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{
			"e": bencodeast.Str("visible"),
			"u": bencodeast.Str("hidden"),
		})

		var got UnexportedStruct
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, "visible", got.Exported)
		assert.Equal(t, "", got.unexported)
	})

	t.Run("untagged fields are not addressable by the empty key", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{
			"":  bencodeast.Str("oops"),
			"t": bencodeast.Str("ok"),
		})

		var got UntaggedStruct
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, "ok", got.Tagged)
		assert.Equal(t, "", got.Untagged)
	})

	// Pointer fields are unsupported today. Flip this to a NoError assertion
	// if you decide to allocate through pointers.
	//
	// FAILS TODAY: returns a bare ErrSyntax, which is wrong — the input is
	// perfectly well-formed bencode. The problem is the destination.
	t.Run("pointer struct field is ErrInvalidDestination", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{
			"p": bencodeast.Dict{"ff": bencodeast.Str("x")},
		})

		var got PtrFieldStruct
		err := decode(t, input, &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidDestination)
	})

	t.Run("skipping a large unknown value stays in sync", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{
			"a_unknown": bencodeast.Dict{
				"deep": bencodeast.List{
					bencodeast.Dict{"x": bencodeast.List{bencodeast.Int(1), bencodeast.Str("y")}},
				},
			},
			"f": bencodeast.Str("after"),
		})

		var got TestStruct
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, "after", got.F)
	})
}

// ---------------------------------------------------------------------------
// malformed / truncated input
// ---------------------------------------------------------------------------

func TestDecoder_Decode_Malformed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want error // nil means "any error is fine"
	}{
		{"empty input", "", nil},
		{"unknown type byte", "x", ErrSyntax},
		{"bare terminator", "e", ErrSyntax},
		{"colon with no length", ":abc", ErrSyntax},

		// Truncation. Left as "any error" on purpose: whether a cut-off value
		// reports io.ErrUnexpectedEOF or a syntax error is a design decision
		// you have not made yet.
		{"int missing terminator", "i42", nil},
		{"string missing colon", "4spam", nil},
		{"string shorter than declared", "10:abc", nil},
		{"list unterminated", "l", nil},
		{"list unterminated with element", "li1e", nil},
		{"dict unterminated", "d", nil},
		{"dict key with no value", "d3:foo", nil},

		{"int empty", "ie", ErrEmpty},
		{"int leading zero", "i03e", ErrLeadingZero},
		{"int negative leading zero", "i-03e", ErrNegativeZero},
		{"int negative zero", "i-0e", ErrNegativeZero},

		// FAILS TODAY: these four leak a raw *strconv.NumError or an
		// unclassifiable "TODO" to the caller. Malformed bytes are ErrSyntax.
		{"int not numeric", "iabce", ErrSyntax},
		{"int sign in middle", "i4-2e", ErrSyntax},
		{"int lone minus", "i-e", ErrSyntax},
		{"string length not numeric", "4x:spam", ErrSyntax},

		{"dict value is terminator", "d3:fooe", ErrSyntax},
		// FAILS TODAY: bencode dict keys are byte strings by spec, so a
		// non-string key is malformed input, not a destination problem.
		{"dict key is not a string", "di1ei2ee", ErrSyntax},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got any
			err := decode(t, tt.in, &got)
			require.Error(t, err)
			if tt.want != nil {
				assert.ErrorIs(t, err, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// integers
// ---------------------------------------------------------------------------

func TestDecoder_Decode_IntBoundaries(t *testing.T) {
	t.Parallel()

	t.Run("zero", func(t *testing.T) {
		t.Parallel()
		var got int
		require.NoError(t, decode(t, "i0e", &got))
		assert.Equal(t, 0, got)
	})

	t.Run("max int64", func(t *testing.T) {
		t.Parallel()
		var got int64
		require.NoError(t, decode(t, "i9223372036854775807e", &got))
		assert.Equal(t, int64(9223372036854775807), got)
	})

	t.Run("min int64", func(t *testing.T) {
		t.Parallel()
		var got int64
		require.NoError(t, decode(t, "i-9223372036854775808e", &got))
		assert.Equal(t, int64(-9223372036854775808), got)
	})

	// FAILS TODAY: leaks *strconv.NumError.
	t.Run("beyond int64 is ErrOverflow", func(t *testing.T) {
		t.Parallel()
		var got int64
		err := decode(t, "i99999999999999999999e", &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrOverflow)
	})

	// FAILS TODAY: OverflowInt is checked, but returns "TODO".
	t.Run("overflowing a narrow int is ErrOverflow", func(t *testing.T) {
		t.Parallel()
		var got int8
		err := decode(t, "i300e", &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrOverflow)
	})

	// FAILS TODAY: leaks *strconv.NumError from ParseUint.
	t.Run("negative into uint is ErrOverflow", func(t *testing.T) {
		t.Parallel()
		var got uint64
		err := decode(t, "i-5e", &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrOverflow)
	})

	// FAILS TODAY: same, reached through a struct field.
	t.Run("negative into uint struct field is ErrOverflow", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{"u": bencodeast.Int(-5)})

		var got UnsignedStruct
		err := decode(t, input, &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrOverflow)
	})
}

// ---------------------------------------------------------------------------
// strings
// ---------------------------------------------------------------------------

func TestDecoder_Decode_StringEdgeCases(t *testing.T) {
	t.Parallel()

	t.Run("empty string", func(t *testing.T) {
		t.Parallel()
		var got string
		require.NoError(t, decode(t, "0:", &got))
		assert.Equal(t, "", got)
	})

	t.Run("empty byte slice", func(t *testing.T) {
		t.Parallel()
		var got []byte
		require.NoError(t, decode(t, "0:", &got))
		assert.Empty(t, got)
	})

	t.Run("string containing bencode metacharacters", func(t *testing.T) {
		t.Parallel()
		var got string
		require.NoError(t, decode(t, "6:d3:abce", &got))
		assert.Equal(t, "d3:abc", got)
	})

	t.Run("non-utf8 bytes survive round trip", func(t *testing.T) {
		t.Parallel()
		var got []byte
		require.NoError(t, decode(t, "4:\x00\xff\xfe\x01", &got))
		assert.Equal(t, []byte{0x00, 0xff, 0xfe, 0x01}, got)
	})

	t.Run("length digits over the cap are rejected", func(t *testing.T) {
		t.Parallel()
		var got string
		err := decode(t, "1000000:whatever", &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrMaxStringLenDigits)
	})

	t.Run("leading zero in length is an error", func(t *testing.T) {
		t.Parallel()
		var got string
		err := decode(t, "05:hello", &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrLeadingZero)
	})

	// Zero-padding past the digit cap is still a leading zero, and that check
	// runs first. Pins the ordering of the two length checks.
	t.Run("zero padded length is rejected as a leading zero", func(t *testing.T) {
		t.Parallel()
		var got []byte
		err := decode(t, "0000004:spam", &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrLeadingZero)
	})

	t.Run("decoded slices do not alias each other", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{bencodeast.Str("aaaa"), bencodeast.Str("bbbb")})

		var got [][]byte
		require.NoError(t, decode(t, input, &got))
		require.Len(t, got, 2)

		got[0][0] = 'z'
		assert.Equal(t, []byte("bbbb"), got[1])
	})
}

// ---------------------------------------------------------------------------
// arrays
// ---------------------------------------------------------------------------

func TestDecoder_Decode_Array(t *testing.T) {
	t.Parallel()

	t.Run("exact length", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{
			bencodeast.Str("a"), bencodeast.Str("b"), bencodeast.Str("c"),
		})

		var got [3]string
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, [3]string{"a", "b", "c"}, got)
	})

	t.Run("fewer elements leaves the tail zeroed", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{bencodeast.Str("a"), bencodeast.Str("b")})

		var got [3]string
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, [3]string{"a", "b", ""}, got)
	})

	t.Run("extra elements are discarded", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{
			bencodeast.Str("a"), bencodeast.Str("b"), bencodeast.Str("c"),
		})

		var got [2]string
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, [2]string{"a", "b"}, got)
	})

	t.Run("overflowing array leaves the stream positioned after the list", func(t *testing.T) {
		t.Parallel()
		d := NewDecoder(strings.NewReader("l1:a1:b1:cei99e"))

		var arr [1]string
		require.NoError(t, decodeWith(t, d, &arr))

		var after int
		require.NoError(t, decodeWith(t, d, &after))
		assert.Equal(t, 99, after)
	})

	// FAILS TODAY: decodeString handles []byte but not [N]byte, so this hits
	// the default branch and errors.
	//
	// This is the single most natural destination in a torrent client: a
	// SHA-1 is exactly [20]byte, and it arrives as a bencode string, not a
	// list. Without it every hash has to be copied out of a []byte by hand.
	t.Run("bencode string into a byte array", func(t *testing.T) {
		t.Parallel()
		var got [4]byte
		require.NoError(t, decode(t, "4:spam", &got))
		assert.Equal(t, [4]byte{'s', 'p', 'a', 'm'}, got)
	})

	// FAILS TODAY: same gap, through a struct field.
	t.Run("sha1 sized array field", func(t *testing.T) {
		t.Parallel()
		hash := strings.Repeat("\xab", 20)
		input := mustEncode(t, bencodeast.Dict{"hash": bencodeast.Str(hash)})

		var got ShaStruct
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, []byte(hash), got.Hash[:])
	})

	// FAILS TODAY: a short string should not partially fill the array.
	t.Run("string shorter than the array is ErrTypeMismatch", func(t *testing.T) {
		t.Parallel()
		var got [20]byte
		err := decode(t, "4:spam", &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrTypeMismatch)
	})
}

// ---------------------------------------------------------------------------
// destination mismatches
// ---------------------------------------------------------------------------

func TestDecoder_Decode_ContainerMismatch(t *testing.T) {
	t.Parallel()

	// Every case here is well-formed bencode aimed at a destination that
	// cannot hold it. That is ErrTypeMismatch, never ErrSyntax — the bytes
	// are fine, the target is not. Several currently report ErrSyntax, which
	// tells a caller to go looking for a corrupt file that does not exist.
	tests := []struct {
		name string
		in   string
		dst  func() any
	}{
		{"list into int", "li1ee", func() any { return new(int) }},
		{"list into struct", "li1ee", func() any { return new(TestStruct2) }},
		{"dict into int", "d1:a1:be", func() any { return new(int) }},
		{"dict into slice", "d1:a1:be", func() any { return new([]string) }},
		{"string into map", "4:spam", func() any { return new(map[string]string) }},
		{"string into int", "4:spam", func() any { return new(int) }},
		{"int into slice", "i1e", func() any { return new([]string) }},
		{"int into string", "i1e", func() any { return new(string) }},
		// FAILS TODAY (panic): SetMapIndex is handed a string key for an
		// int-keyed map. decode() turns that into a hard test failure.
		{"non-string map key type", "d1:11:ae", func() any { return new(map[int]string) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := decode(t, tt.in, tt.dst())
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrTypeMismatch)
		})
	}

	t.Run("dict into slice struct field", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{
			"f3": bencodeast.Dict{"a": bencodeast.Str("b")},
		})

		var got TestStruct
		err := decode(t, input, &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrTypeMismatch)
	})
}

// A failed decode must still consume exactly the value it failed on, so the
// next Decode on the same stream starts on a value boundary. This is the axis
// with the least coverage and the one that hides the nastiest bugs: a decoder
// that desyncs corrupts every value after the first mistake.
func TestDecoder_Decode_MismatchLeavesStreamAligned(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		bad  string // complete, well-formed value the destination cannot hold
		dst  func() any
	}{
		{"list into int", "li1ee", func() any { return new(int) }},
		{"dict into int", "d1:a1:be", func() any { return new(int) }},
		{"dict into slice", "d1:a1:be", func() any { return new([]string) }},
		{"int into slice", "i1e", func() any { return new([]string) }},
		// FAILS TODAY: decodeString's default branch calls skip() after the
		// string was already fully consumed, so skip eats the NEXT value.
		{"string into map", "4:spam", func() any { return new(map[string]string) }},
		{"string into int", "4:spam", func() any { return new(int) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := NewDecoder(strings.NewReader(tt.bad + "i42e"))

			require.Error(t, decodeWith(t, d, tt.dst()))

			var after int
			require.NoError(t, decodeWith(t, d, &after), "stream desynced after the failed decode")
			assert.Equal(t, 42, after)
		})
	}
}

// ---------------------------------------------------------------------------
// empty containers
// ---------------------------------------------------------------------------

func TestDecoder_Decode_EmptyContainers(t *testing.T) {
	t.Parallel()

	t.Run("empty dict into map", func(t *testing.T) {
		t.Parallel()
		var got map[string]string
		require.NoError(t, decode(t, "de", &got))
		assert.NotNil(t, got)
		assert.Empty(t, got)
	})

	t.Run("empty dict into struct", func(t *testing.T) {
		t.Parallel()
		var got TestStruct
		require.NoError(t, decode(t, "de", &got))
		assert.Equal(t, TestStruct{}, got)
	})

	t.Run("empty dict into any", func(t *testing.T) {
		t.Parallel()
		var got any
		require.NoError(t, decode(t, "de", &got))
		assert.Equal(t, map[string]any{}, got)
	})

	t.Run("empty list into any", func(t *testing.T) {
		t.Parallel()
		var got any
		require.NoError(t, decode(t, "le", &got))
		assert.Equal(t, []any{}, got)
	})
}

// ---------------------------------------------------------------------------
// stream behaviour
// ---------------------------------------------------------------------------

func TestDecoder_Stream(t *testing.T) {
	t.Parallel()

	t.Run("successive values from one decoder", func(t *testing.T) {
		t.Parallel()
		d := NewDecoder(strings.NewReader("i1e4:spamli2ee"))

		var a int
		require.NoError(t, decodeWith(t, d, &a))
		assert.Equal(t, 1, a)

		var b string
		require.NoError(t, decodeWith(t, d, &b))
		assert.Equal(t, "spam", b)

		var c []int
		require.NoError(t, decodeWith(t, d, &c))
		assert.Equal(t, []int{2}, c)

		var d4 int
		require.Error(t, decodeWith(t, d, &d4))
	})

	// Documents current behaviour: trailing bytes after a complete value are
	// left in the buffer rather than rejected. Fine for a stream decoder,
	// surprising for a one-shot Unmarshal.
	t.Run("trailing data is not rejected", func(t *testing.T) {
		t.Parallel()
		var got int
		require.NoError(t, decode(t, "i42egarbage", &got))
		assert.Equal(t, 42, got)
	})

	t.Run("reused slice destination is replaced not appended", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{bencodeast.Str("new")})

		got := []string{"old"}
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, []string{"new"}, got)
	})

	t.Run("reused map destination is replaced not merged", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{"new": bencodeast.Str("v")})

		got := map[string]string{"old": "v"}
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, map[string]string{"new": "v"}, got)
	})
}

// ---------------------------------------------------------------------------
// resource limits
//
// Everything below is about hostile input. A torrent client parses .torrent
// files and peer messages from strangers, so "it errors eventually" is not
// good enough — it has to error without spending the machine's memory or
// stack first.
// ---------------------------------------------------------------------------

// FAILS TODAY: decode recurses once per nesting level with no bound.
//
// 100_000 is chosen so the test fails red rather than dying: it is far deeper
// than any legitimate torrent, but still shallow enough to fit the goroutine
// stack today. Do not raise it — a real stack overflow is not a panic and
// cannot be recovered, it takes the whole test binary down with it.
func TestDecoder_Decode_DepthLimit(t *testing.T) {
	t.Parallel()

	const depth = 100_000
	input := strings.Repeat("l", depth) + strings.Repeat("e", depth)

	var got any
	err := decode(t, input, &got)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMaxDepth)
}

// FAILS TODAY: both cases use bufio.Reader.ReadString, which grows a buffer
// until it finds the delimiter or hits EOF. With no delimiter in sight that
// is the entire remaining stream, held in memory, before any length check
// runs. Over a peer socket that is the whole connection buffered into RAM.
func TestDecoder_Decode_DoesNotBufferUnboundedInput(t *testing.T) {
	t.Parallel()

	const junk = 8 << 20 // 8 MiB with no terminator anywhere
	const allowed = 1 << 20

	t.Run("integer with no terminator", func(t *testing.T) {
		t.Parallel()
		r := &countingReader{src: strings.NewReader("i" + strings.Repeat("1", junk))}

		var got int
		require.Error(t, NewDecoder(r).Decode(&got))
		assert.Less(t, r.read, allowed,
			"read %d bytes looking for 'e'; a bounded scan should give up far sooner", r.read)
	})

	t.Run("string length with no colon", func(t *testing.T) {
		t.Parallel()
		r := &countingReader{src: strings.NewReader(strings.Repeat("1", junk))}

		var got string
		require.Error(t, NewDecoder(r).Decode(&got))
		assert.Less(t, r.read, allowed,
			"read %d bytes looking for ':'; MaxStringLenDigits should bound this", r.read)
	})
}

// FAILS TODAY: skip() decodes into an `any`, fully materialising a value it is
// about to throw away. A peer that sends one unknown key holding a large value
// makes you allocate all of it for nothing.
func TestDecoder_Decode_SkipDoesNotAllocateSkippedValue(t *testing.T) {
	// Not parallel: measures process-wide allocation.

	const size = 999999 // largest string MaxStringLenDigits allows
	input := "d9:a_unknown" + fmt.Sprintf("%d:%s", size, strings.Repeat("x", size)) +
		"1:f5:helloe"

	var got TestStruct
	var err error
	allocated := allocatedBytes(func() {
		err = decode(t, input, &got)
	})

	require.NoError(t, err)
	require.Equal(t, "hello", got.F, "decoder lost sync while skipping")
	assert.Less(t, allocated, uint64(size/2),
		"allocated %d bytes to skip a %d byte value it discards", allocated, size)
}

func TestDecoder_Decode_LargeString(t *testing.T) {
	t.Parallel()

	// The 6-digit cap means the largest decodable string is 999999 bytes.
	// A torrent's `pieces` field is 20 bytes per piece, so this ceiling is
	// reached at roughly 50k pieces — an ordinary large torrent. Worth
	// replacing with a byte limit you can raise, rather than a digit count.
	t.Run("just under the digit cap", func(t *testing.T) {
		t.Parallel()
		const n = 999999
		var got []byte
		require.NoError(t, decode(t, fmt.Sprintf("%d:%s", n, strings.Repeat("x", n)), &got))
		assert.Len(t, got, n)
	})

	// A declared length far larger than the payload should fail on read, not
	// allocate the full amount up front.
	t.Run("declared length larger than payload", func(t *testing.T) {
		t.Parallel()
		var got []byte
		require.Error(t, decode(t, "999999:short", &got))
	})
}
