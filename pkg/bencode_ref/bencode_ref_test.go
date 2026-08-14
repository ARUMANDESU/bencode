package bencoderef

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	bencodeast "github.com/ARUMANDESU/gotorrent/pkg/bencode_ast"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

// mustEncode builds bencode test fixtures via the already-tested bencode_ast
// encoder, instead of hand-writing raw bencode strings that are easy to get
// wrong (miscounted length prefixes, unbalanced e's, etc).
func mustEncode(t *testing.T, v bencodeast.Value) string {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, bencodeast.Encode(&buf, v))
	return buf.String()
}

// decode is a small wrapper so call sites don't need to name an intermediate
// Decoder variable just to work around Decode's pointer receiver.
func decode(input string, dst any) error {
	d := NewDecoder(strings.NewReader(input))
	return d.Decode(dst)
}

func TestDecoder_Decode_Scalar(t *testing.T) {
	t.Parallel()

	t.Run("int", func(t *testing.T) {
		t.Parallel()
		var got int
		err := decode("i42e", &got)
		require.NoError(t, err)
		assert.Equal(t, 42, got)
	})

	t.Run("int64 negative", func(t *testing.T) {
		t.Parallel()
		var got int64
		err := decode("i-7e", &got)
		require.NoError(t, err)
		assert.Equal(t, int64(-7), got)
	})

	t.Run("string", func(t *testing.T) {
		t.Parallel()
		var got string
		err := decode("4:spam", &got)
		require.NoError(t, err)
		assert.Equal(t, "spam", got)
	})

	t.Run("[]byte gets the raw bytes, no text decoding", func(t *testing.T) {
		t.Parallel()
		var got []byte
		err := decode("4:spam", &got)
		require.NoError(t, err)
		assert.Equal(t, []byte("spam"), got)
	})

	t.Run("non-pointer destination is an error", func(t *testing.T) {
		t.Parallel()
		var got int
		err := decode("i42e", got)
		require.Error(t, err)
	})

	t.Run("nil pointer destination is an error", func(t *testing.T) {
		t.Parallel()
		var got *int
		err := decode("i42e", got)
		require.Error(t, err)
	})
}

func TestDecoder_Decode_Slice(t *testing.T) {
	t.Parallel()

	t.Run("[]string", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{bencodeast.Str("spam"), bencodeast.Str("lol")})

		var got []string
		err := decode(input, &got)
		require.NoError(t, err)
		assert.Equal(t, []string{"spam", "lol"}, got)
	})

	t.Run("[]int", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{bencodeast.Int(1), bencodeast.Int(2), bencodeast.Int(3)})

		var got []int
		err := decode(input, &got)
		require.NoError(t, err)
		assert.Equal(t, []int{1, 2, 3}, got)
	})

	t.Run("empty list", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{})

		var got []string
		err := decode(input, &got)
		require.NoError(t, err)
		assert.Equal(t, []string{}, got)
	})

	t.Run("nested list of structs", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{
			bencodeast.Dict{"ff": bencodeast.Str("x")},
			bencodeast.Dict{"ff": bencodeast.Str("y")},
		})

		var got []TestStruct2
		err := decode(input, &got)
		require.NoError(t, err)
		assert.Equal(t, []TestStruct2{{FF: "x"}, {FF: "y"}}, got)
	})
}

func TestDecoder_Decode_Map(t *testing.T) {
	t.Parallel()

	t.Run("map[string]string", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{"cow": bencodeast.Str("moo"), "spam": bencodeast.Str("eggs")})

		var got map[string]string
		err := decode(input, &got)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"cow": "moo", "spam": "eggs"}, got)
	})

	t.Run("map[string]int", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{"a": bencodeast.Int(1), "b": bencodeast.Int(2)})

		var got map[string]int
		err := decode(input, &got)
		require.NoError(t, err)
		assert.Equal(t, map[string]int{"a": 1, "b": 2}, got)
	})

	t.Run("map[string]struct", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{"y": bencodeast.Dict{"ff": bencodeast.Str("z")}})

		var got map[string]TestStruct2
		err := decode(input, &got)
		require.NoError(t, err)
		assert.Equal(t, map[string]TestStruct2{"y": {FF: "z"}}, got)
	})
}

func TestDecoder_Decode_Any(t *testing.T) {
	t.Parallel()

	t.Run("top-level int", func(t *testing.T) {
		t.Parallel()
		var got any
		err := decode("i42e", &got)
		require.NoError(t, err)
		assert.Equal(t, int64(42), got)
	})

	t.Run("top-level string", func(t *testing.T) {
		t.Parallel()
		var got any
		err := decode("4:spam", &got)
		require.NoError(t, err)
		assert.Equal(t, "spam", got)
	})

	t.Run("top-level list", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{bencodeast.Str("spam"), bencodeast.Int(1)})

		var got any
		err := decode(input, &got)
		require.NoError(t, err)
		assert.Equal(t, []any{"spam", int64(1)}, got)
	})

	t.Run("top-level dict", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{"cow": bencodeast.Str("moo")})

		var got any
		err := decode(input, &got)
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"cow": "moo"}, got)
	})
}

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
	err := decode(input, &got)
	require.NoError(t, err)

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
	err := decode(input, &got)
	require.NoError(t, err)
	assert.Equal(t, "hello", got.F)
}

func TestDecoder_Decode_TypeMismatch(t *testing.T) {
	t.Parallel()

	t.Run("dict into int", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{"a": bencodeast.Str("b")})

		var got int
		err := decode(input, &got)
		require.Error(t, err)
	})

	t.Run("string into struct field expecting int", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{
			"f":  bencodeast.Str("hello"),
			"f2": bencodeast.Str("not-an-int"),
		})

		var got TestStruct
		err := decode(input, &got)
		require.Error(t, err)
	})

	t.Run("int into slice field", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{
			"f":  bencodeast.Str("hello"),
			"f3": bencodeast.Int(1),
		})

		var got TestStruct
		err := decode(input, &got)
		require.Error(t, err)
	})
}

// This file complements decode_test.go with edge cases: malformed input,
// numeric range, arrays, struct-shape oddities, and stream behaviour.
//
// Tests marked "KNOWN FAILURE" describe the behaviour the decoder *should*
// have. They fail against the current implementation on purpose.

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// decodeSafe turns a panic into an error. Several reflect paths in the decoder
// can panic on bad input, and a panic in any subtest tears down the whole test
// binary, hiding every other result. Use this anywhere a panic is plausible.
func decodeSafe(input string, dst any) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return decode(input, dst)
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

// ---------------------------------------------------------------------------
// malformed / truncated input
// ---------------------------------------------------------------------------

func TestDecoder_Decode_Malformed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  error // nil means "any error is fine"
	}{
		{"empty input", "", nil},
		{"unknown type byte", "x", ErrSyntax},
		{"bare terminator", "e", ErrSyntax},
		{"colon with no length", ":abc", ErrSyntax},

		{"int missing terminator", "i42", nil},
		{"int empty", "ie", ErrEmpty},
		{"int not numeric", "iabce", nil},
		{"int sign in middle", "i4-2e", nil},
		{"int lone minus", "i-e", nil},
		{"int leading zero", "i03e", ErrLeadingZero},
		{"int negative leading zero", "i-03e", ErrNegativeZero},
		{"int negative zero", "i-0e", ErrNegativeZero},

		{"string missing colon", "4spam", nil},
		{"string shorter than declared", "10:abc", nil},
		{"string length not numeric", "4x:spam", nil},

		{"list unterminated", "l", nil},
		{"list unterminated with element", "li1e", nil},
		{"dict unterminated", "d", nil},
		{"dict key with no value", "d3:foo", nil},
		{"dict value is terminator", "d3:fooe", ErrSyntax},
		{"dict key is not a string", "di1ei2ee", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got any
			err := decodeSafe(tt.input, &got)
			require.Error(t, err)
			if tt.want != nil {
				assert.ErrorIs(t, err, tt.want)
			}
		})
	}
}

func TestDecoder_Decode_IntBoundaries(t *testing.T) {
	t.Parallel()

	t.Run("zero", func(t *testing.T) {
		t.Parallel()
		var got int
		require.NoError(t, decode("i0e", &got))
		assert.Equal(t, 0, got)
	})

	t.Run("max int64", func(t *testing.T) {
		t.Parallel()
		var got int64
		require.NoError(t, decode("i9223372036854775807e", &got))
		assert.Equal(t, int64(9223372036854775807), got)
	})

	t.Run("min int64", func(t *testing.T) {
		t.Parallel()
		var got int64
		require.NoError(t, decode("i-9223372036854775808e", &got))
		assert.Equal(t, int64(-9223372036854775808), got)
	})

	t.Run("beyond int64 is an error", func(t *testing.T) {
		t.Parallel()
		var got int64
		require.Error(t, decode("i99999999999999999999e", &got))
	})

	// KNOWN FAILURE: SetInt truncates silently, yielding 44.
	t.Run("overflowing a narrow int is an error", func(t *testing.T) {
		t.Parallel()
		var got int8
		require.Error(t, decodeSafe("i300e", &got))
	})

	// KNOWN FAILURE: uint64(-5) wraps to 18446744073709551611.
	t.Run("negative into uint is an error", func(t *testing.T) {
		t.Parallel()
		var got uint64
		require.Error(t, decodeSafe("i-5e", &got))
	})

	// KNOWN FAILURE: same wrap, reached through a struct field.
	t.Run("negative into uint struct field is an error", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{"u": bencodeast.Int(-5)})

		var got UnsignedStruct
		require.Error(t, decodeSafe(input, &got))
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
		require.NoError(t, decode("0:", &got))
		assert.Equal(t, "", got)
	})

	t.Run("empty byte slice", func(t *testing.T) {
		t.Parallel()
		var got []byte
		require.NoError(t, decode("0:", &got))
		assert.Empty(t, got)
	})

	t.Run("string containing bencode metacharacters", func(t *testing.T) {
		t.Parallel()
		var got string
		require.NoError(t, decode("7:d3:abce", &got))
		assert.Equal(t, "d3:abc", got)
	})

	t.Run("non-utf8 bytes survive round trip", func(t *testing.T) {
		t.Parallel()
		var got []byte
		require.NoError(t, decode("4:\x00\xff\xfe\x01", &got))
		assert.Equal(t, []byte{0x00, 0xff, 0xfe, 0x01}, got)
	})

	t.Run("length digits over the cap are rejected", func(t *testing.T) {
		t.Parallel()
		var got string
		err := decode("1000000:whatever", &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrMaxStringLenDigits)
	})

	// KNOWN FAILURE: "05:hello" is currently accepted. Ints reject leading
	// zeros; string lengths should too.
	t.Run("leading zero in length is an error", func(t *testing.T) {
		t.Parallel()
		var got string
		require.Error(t, decodeSafe("05:hello", &got))
	})

	t.Run("decoded slices do not alias each other", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{bencodeast.Str("aaaa"), bencodeast.Str("bbbb")})

		var got [][]byte
		require.NoError(t, decode(input, &got))
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

	// KNOWN FAILURE: the array branch builds a *[N]T and never advances its
	// index, so Index panics and every element targets slot 0.
	t.Run("exact length", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{
			bencodeast.Str("a"), bencodeast.Str("b"), bencodeast.Str("c"),
		})

		var got [3]string
		require.NoError(t, decodeSafe(input, &got))
		assert.Equal(t, [3]string{"a", "b", "c"}, got)
	})

	// KNOWN FAILURE
	t.Run("fewer elements leaves the tail zeroed", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{bencodeast.Str("a"), bencodeast.Str("b")})

		var got [3]string
		require.NoError(t, decodeSafe(input, &got))
		assert.Equal(t, [3]string{"a", "b", ""}, got)
	})

	// KNOWN FAILURE: the overflow path calls Skip without UnreadByte, so the
	// skipped value is parsed one byte in.
	t.Run("extra elements are discarded", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{
			bencodeast.Str("a"), bencodeast.Str("b"), bencodeast.Str("c"),
		})

		var got [2]string
		require.NoError(t, decodeSafe(input, &got))
		assert.Equal(t, [2]string{"a", "b"}, got)
	})

	// KNOWN FAILURE: same missing UnreadByte, and the discarded tail must be
	// consumed fully or the next Decode reads garbage.
	t.Run("overflowing array leaves the stream positioned after the list", func(t *testing.T) {
		t.Parallel()
		d := NewDecoder(strings.NewReader("l1:a1:b1:cei99e"))

		var arr [1]string
		require.NoError(t, d.Decode(&arr))

		var after int
		require.NoError(t, d.Decode(&after))
		assert.Equal(t, 99, after)
	})

	// A 20-byte SHA-1 is the obvious real use for a fixed array.
	t.Run("byte array", func(t *testing.T) {
		t.Parallel()
		var got [4]byte
		require.NoError(t, decodeSafe("l i1e i2e i3e i4e e", &got))
	})
}

// ---------------------------------------------------------------------------
// container / destination mismatches
// ---------------------------------------------------------------------------

func TestDecoder_Decode_ContainerMismatch(t *testing.T) {
	t.Parallel()

	// KNOWN FAILURE: decodeList's switch has no default, so a list decoded
	// into a scalar returns nil without consuming any input.
	t.Run("list into int is an error", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{bencodeast.Int(1)})

		var got int
		require.Error(t, decodeSafe(input, &got))
	})

	// KNOWN FAILURE: same silent fallthrough.
	t.Run("list into struct is an error", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{bencodeast.Int(1)})

		var got TestStruct2
		require.Error(t, decodeSafe(input, &got))
	})

	// KNOWN FAILURE: the silent return also desynchronises the stream.
	t.Run("failed list decode does not leave the stream mid-value", func(t *testing.T) {
		t.Parallel()
		d := NewDecoder(strings.NewReader("li1eei42e"))

		var bad int
		_ = d.Decode(&bad)

		var after int
		err := d.Decode(&after)
		require.NoError(t, err)
		assert.Equal(t, 42, after)
	})

	t.Run("dict into slice is an error", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{"a": bencodeast.Str("b")})

		var got []string
		require.Error(t, decodeSafe(input, &got))
	})

	t.Run("string into map is an error", func(t *testing.T) {
		t.Parallel()
		var got map[string]string
		require.Error(t, decodeSafe("4:spam", &got))
	})

	t.Run("dict into slice struct field is an error", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{
			"f3": bencodeast.Dict{"a": bencodeast.Str("b")},
		})

		var got TestStruct
		require.Error(t, decodeSafe(input, &got))
	})

	// KNOWN FAILURE: SetMapIndex is handed a string key for an int-keyed map,
	// which panics inside reflect.
	t.Run("non-string map key type is an error", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{"1": bencodeast.Str("a")})

		var got map[int]string
		require.Error(t, decodeSafe(input, &got))
	})
}

// ---------------------------------------------------------------------------
// empty containers
// ---------------------------------------------------------------------------

func TestDecoder_Decode_EmptyContainers(t *testing.T) {
	t.Parallel()

	t.Run("empty dict into map", func(t *testing.T) {
		t.Parallel()
		var got map[string]string
		require.NoError(t, decode("de", &got))
		assert.NotNil(t, got)
		assert.Empty(t, got)
	})

	t.Run("empty dict into struct", func(t *testing.T) {
		t.Parallel()
		var got TestStruct
		require.NoError(t, decode("de", &got))
		assert.Equal(t, TestStruct{}, got)
	})

	t.Run("empty dict into any", func(t *testing.T) {
		t.Parallel()
		var got any
		require.NoError(t, decode("de", &got))
		assert.Equal(t, map[string]any{}, got)
	})

	t.Run("empty list into any", func(t *testing.T) {
		t.Parallel()
		var got any
		require.NoError(t, decode("le", &got))
		assert.Equal(t, []any{}, got)
	})
}

// ---------------------------------------------------------------------------
// struct shapes
// ---------------------------------------------------------------------------

func TestDecoder_Decode_StructShapes(t *testing.T) {
	t.Parallel()

	t.Run("plain nested struct field", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{
			"n":     bencodeast.Int(7),
			"inner": bencodeast.Dict{"ff": bencodeast.Str("deep")},
		})

		var got NestedStruct
		require.NoError(t, decode(input, &got))
		assert.Equal(t, NestedStruct{N: 7, Inner: TestStruct2{FF: "deep"}}, got)
	})

	t.Run("missing keys leave zero values", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{"f": bencodeast.Str("only")})

		var got TestStruct
		require.NoError(t, decode(input, &got))
		assert.Equal(t, TestStruct{F: "only"}, got)
	})

	// Documents current behaviour: fields absent from the input keep whatever
	// the destination already held. Worth deciding on deliberately.
	t.Run("absent keys do not clear a reused destination", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{"f2": bencodeast.Int(1)})

		got := TestStruct{F: "stale"}
		require.NoError(t, decode(input, &got))
		assert.Equal(t, "stale", got.F)
		assert.Equal(t, int64(1), got.F2)
	})

	t.Run("duplicate keys: last one wins", func(t *testing.T) {
		t.Parallel()
		var got TestStruct
		require.NoError(t, decode("d1:f5:first1:f6:seconde", &got))
		assert.Equal(t, "second", got.F)
	})

	// KNOWN FAILURE: the field is unsettable, so decode errors out. encoding/json
	// ignores unexported fields instead; this should too.
	t.Run("unexported fields are ignored", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{
			"e": bencodeast.Str("visible"),
			"u": bencodeast.Str("hidden"),
		})

		var got UnexportedStruct
		require.NoError(t, decodeSafe(input, &got))
		assert.Equal(t, "visible", got.Exported)
		assert.Equal(t, "", got.unexported)
	})

	// KNOWN FAILURE: every untagged field is registered under the "" key, so a
	// literal "" key in the input writes into one of them.
	t.Run("untagged fields are not addressable by the empty key", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{
			"":  bencodeast.Str("oops"),
			"t": bencodeast.Str("ok"),
		})

		var got UntaggedStruct
		require.NoError(t, decodeSafe(input, &got))
		assert.Equal(t, "ok", got.Tagged)
		assert.Equal(t, "", got.Untagged)
	})

	// Pointer fields are unsupported today. Flip this to a NoError assertion
	// if you decide to allocate through pointers.
	t.Run("pointer struct field", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{
			"p": bencodeast.Dict{"ff": bencodeast.Str("x")},
		})

		var got PtrFieldStruct
		require.Error(t, decodeSafe(input, &got))
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
		require.NoError(t, decode(input, &got))
		assert.Equal(t, "after", got.F)
	})
}

// ---------------------------------------------------------------------------
// any / interface targets
// ---------------------------------------------------------------------------

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
		require.NoError(t, decode(input, &got))
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
		require.NoError(t, decode("4:\x00\xff\x00\xff", &got))
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
		require.NoError(t, decode(input, &got))
		assert.Equal(t, []any{int64(1)}, got.V)
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
		require.NoError(t, d.Decode(&a))
		assert.Equal(t, 1, a)

		var b string
		require.NoError(t, d.Decode(&b))
		assert.Equal(t, "spam", b)

		var c []int
		require.NoError(t, d.Decode(&c))
		assert.Equal(t, []int{2}, c)

		var d4 int
		require.Error(t, d.Decode(&d4))
	})

	// Documents current behaviour: trailing bytes after a complete value are
	// left in the buffer rather than rejected. Fine for a stream decoder,
	// surprising for a one-shot Unmarshal.
	t.Run("trailing data is not rejected", func(t *testing.T) {
		t.Parallel()
		var got int
		require.NoError(t, decode("i42egarbage", &got))
		assert.Equal(t, 42, got)
	})

	t.Run("reused slice destination is replaced not appended", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{bencodeast.Str("new")})

		got := []string{"old"}
		require.NoError(t, decode(input, &got))
		assert.Equal(t, []string{"new"}, got)
	})

	t.Run("reused map destination is replaced not merged", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{"new": bencodeast.Str("v")})

		got := map[string]string{"old": "v"}
		require.NoError(t, decode(input, &got))
		assert.Equal(t, map[string]string{"new": "v"}, got)
	})
}

// ---------------------------------------------------------------------------
// resource limits
// ---------------------------------------------------------------------------

func TestDecoder_Decode_DepthLimit(t *testing.T) {
	// Enable once a depth limit exists. decode recurses once per nesting
	// level with no bound, so a hostile peer can exhaust the stack — and a
	// stack overflow is not recoverable, it takes the process down. That also
	// means this test cannot be made safe with decodeSafe.
	t.Skip("no depth limit implemented yet; enable after adding one")

	const depth = 10_000_000
	input := strings.Repeat("l", depth) + strings.Repeat("e", depth)

	var got any
	require.Error(t, decode(input, &got))
}

func TestDecoder_Decode_LargeString(t *testing.T) {
	t.Parallel()

	// The 6-digit cap means the largest decodable string is 999999 bytes.
	// A torrent's `pieces` field is 20 bytes per piece, so this ceiling is
	// reached at roughly 50k pieces — an ordinary large torrent.
	t.Run("just under the digit cap", func(t *testing.T) {
		t.Parallel()
		const n = 999999
		var got []byte
		require.NoError(t, decode(fmt.Sprintf("%d:%s", n, strings.Repeat("x", n)), &got))
		assert.Len(t, got, n)
	})

	t.Run("one digit over is rejected even though the value is small", func(t *testing.T) {
		t.Parallel()
		var got []byte
		err := decode("0000004:spam", &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrMaxStringLenDigits)
	})

	// A declared length far larger than the payload should fail on read, not
	// allocate the full amount up front.
	t.Run("declared length larger than payload", func(t *testing.T) {
		t.Parallel()
		var got []byte
		require.Error(t, decode("999999:short", &got))
	})
}
