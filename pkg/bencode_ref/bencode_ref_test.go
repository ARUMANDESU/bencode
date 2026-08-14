package bencoderef

import (
	"bytes"
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
