package bencoderef

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	bencodeast "github.com/ARUMANDESU/gotorrent/pkg/bencode_ast"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file asserts SPEC.md and nothing else. Every test cites the section it
// enforces. When a test and the implementation disagree, the spec decides which
// one is wrong — a test that merely describes what the code happens to do today
// is worthless, because it can never fail for a reason worth knowing.
//
// Not covered here: SPEC §9 RawMessage, which is specified but deliberately not
// implemented yet. Add its tests with the feature.

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

type simple struct {
	S string `bencode:"s"`
	I int64  `bencode:"i"`
}

type nested struct {
	N     int64  `bencode:"n"`
	Inner simple `bencode:"inner"`
}

type containers struct {
	Strs    []string          `bencode:"strs"`
	Map     map[string]string `bencode:"map"`
	Structs []simple          `bencode:"structs"`
	Dicts   map[string]simple `bencode:"dicts"`
}

type pointers struct {
	P  *simple `bencode:"p"`
	PP **int64 `bencode:"pp"`
	PS *string `bencode:"ps"`
}

type tagged struct {
	Named      string `bencode:"named"`
	Skipped    string `bencode:"-"`
	DashKey    string `bencode:"-,"`
	OptsOnly   string `bencode:",omitempty"`
	UnknownOpt string `bencode:"uo,someopt,another"`
	Untagged   string
	unexported string `bencode:"unexported"`
}

// ambiguous pins SPEC §3.3: two tagged fields claiming one key bind to
// nothing. Declaration order must not decide.
type ambiguous struct {
	A string `bencode:"dup"`
	B string `bencode:"dup"`
}

// taggedBeatsUntagged pins the one collision SPEC §3.3 does resolve. The two
// types differ only in declaration order, which must not matter.
type taggedBeatsUntagged struct {
	A string `bencode:"B"`
	B string
}

type untaggedBeforeTagged struct {
	B string
	A string `bencode:"B"`
}

// ambiguousThree and ambiguousMixed catch a resolver that decides pairwise
// against the map it is building: once a contested key is removed, a third
// claimant finds a clean miss and installs itself. Ambiguity has to be sticky.
type ambiguousThree struct {
	A string `bencode:"dup"`
	B string `bencode:"dup"`
	C string `bencode:"dup"`
}

// The tag is capitalised so it matches Dup's field name verbatim; §3.3 folds
// no case, so a lowercase tag here would be a different key entirely and no
// collision would occur.
type ambiguousMixed struct {
	A   string `bencode:"Dup"`
	B   string `bencode:"Dup"`
	Dup string
}

type caseSensitive struct {
	IP string
}

// namedKey is string-kinded but not string-typed, which is the difference
// between Kind() and Type() when guarding SetMapIndex.
type namedKey string

type sha struct {
	Hash [20]byte `bencode:"hash"`
}

type unsigned struct {
	U uint64 `bencode:"u"`
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// mustEncode builds fixtures through the already-tested bencode_ast encoder
// rather than hand-written bencode, which is easy to get wrong: miscounted
// length prefixes and unbalanced 'e's produce tests that pass for the wrong
// reason. Raw strings are used only where the point of the test IS the exact
// bytes.
func mustEncode(t *testing.T, v bencodeast.Value) string {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, bencodeast.Encode(&buf, v))
	return buf.String()
}

// decode runs one value through a fresh Decoder.
//
// A panic is never a valid outcome (SPEC §8): callers cannot recover from one,
// and it tears down the whole test binary. So a panic fails the test loudly
// here and is deliberately NOT converted into an error — an errors-are-fine
// helper makes a crash indistinguishable from a rejection, and every negative
// test then passes for the wrong reason.
func decode(t *testing.T, input string, dst any) error {
	t.Helper()
	return decodeWith(t, NewDecoder(strings.NewReader(input)), dst)
}

// decodeWith is decode against an existing Decoder, for the stream-contract
// tests that read several values from one reader.
func decodeWith(t *testing.T, d *Decoder, dst any) error {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("decoder panicked (SPEC §8 forbids this): %v", r)
		}
	}()
	return d.Decode(dst)
}

// countingReader reports how many bytes the decoder actually pulled, proving
// it gives up on a hostile value instead of buffering the stream while it
// hunts for a terminator (SPEC §7.2).
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

func repeat(s string, n int) string { return strings.Repeat(s, n) }

// nest builds n levels of nested lists around an empty core.
func nest(n int) string { return repeat("l", n) + repeat("e", n) }

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

	t.Run("length zero into byte slice", func(t *testing.T) {
		t.Parallel()
		var got []byte
		require.NoError(t, decode(t, "0:", &got))
		assert.Empty(t, got)
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

func TestSpec2_Leniency(t *testing.T) {
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
// SPEC §3.1 — entry point
// ---------------------------------------------------------------------------

func TestSpec3_1_EntryPoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		dst  func() any
	}{
		{"non-pointer", func() any { var v int; return v }},
		{"nil typed pointer", func() any { var v *int; return v }},
		{"nil untyped", func() any { return nil }},
		{"nil pointer to struct", func() any { var v *simple; return v }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := decode(t, "i42e", tt.dst())
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidDestination)
		})
	}

	// SPEC §3.1: ErrInvalidDestination is reachable only from this check, and
	// §6 requires it to consume nothing. A caller that passes a bad
	// destination has not damaged the stream.
	t.Run("consumes no bytes and leaves the decoder usable", func(t *testing.T) {
		t.Parallel()
		d := NewDecoder(strings.NewReader("i42e"))

		var bad int
		require.ErrorIs(t, decodeWith(t, d, bad), ErrInvalidDestination)

		var good int
		require.NoError(t, decodeWith(t, d, &good))
		assert.Equal(t, 42, good)
	})
}

// ---------------------------------------------------------------------------
// SPEC §3.2 — pointers
// ---------------------------------------------------------------------------

func TestSpec3_2_Pointers(t *testing.T) {
	t.Parallel()

	t.Run("top level pointer is allocated", func(t *testing.T) {
		t.Parallel()
		var got *int64
		require.NoError(t, decode(t, "i42e", &got))
		require.NotNil(t, got)
		assert.Equal(t, int64(42), *got)
	})

	t.Run("pointer to struct is allocated", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{"s": bencodeast.Str("x")})

		var got *simple
		require.NoError(t, decode(t, input, &got))
		require.NotNil(t, got)
		assert.Equal(t, "x", got.S)
	})

	t.Run("pointer fields are allocated through", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{
			"p":  bencodeast.Dict{"s": bencodeast.Str("deep")},
			"pp": bencodeast.Int(9),
			"ps": bencodeast.Str("str"),
		})

		var got pointers
		require.NoError(t, decode(t, input, &got))

		require.NotNil(t, got.P)
		assert.Equal(t, "deep", got.P.S)

		require.NotNil(t, got.PP)
		require.NotNil(t, *got.PP)
		assert.Equal(t, int64(9), **got.PP)

		require.NotNil(t, got.PS)
		assert.Equal(t, "str", *got.PS)
	})

	// SPEC §3.2: bencode has no null, so a pointer is non-nil iff the key was
	// present. This is the whole reason pointer support is worth having.
	t.Run("absent key leaves the pointer nil", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{"ps": bencodeast.Str("only")})

		var got pointers
		require.NoError(t, decode(t, input, &got))
		assert.Nil(t, got.P)
		assert.Nil(t, got.PP)
		require.NotNil(t, got.PS)
	})

	t.Run("pointer elements inside a slice", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{
			bencodeast.Dict{"s": bencodeast.Str("a")},
			bencodeast.Dict{"s": bencodeast.Str("b")},
		})

		var got []*simple
		require.NoError(t, decode(t, input, &got))
		require.Len(t, got, 2)
		require.NotNil(t, got[0])
		require.NotNil(t, got[1])
		assert.Equal(t, "a", got[0].S)
		assert.Equal(t, "b", got[1].S)
	})

	t.Run("pointer values inside a map", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{"k": bencodeast.Dict{"s": bencodeast.Str("v")}})

		var got map[string]*simple
		require.NoError(t, decode(t, input, &got))
		require.NotNil(t, got["k"])
		assert.Equal(t, "v", got["k"].S)
	})
}

// ---------------------------------------------------------------------------
// SPEC §3.3 — struct field mapping
// ---------------------------------------------------------------------------

func TestSpec3_3_FieldMapping(t *testing.T) {
	t.Parallel()

	t.Run("tag name, dash skip, literal dash, opts, field-name fallback", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{
			"named":      bencodeast.Str("by tag"),
			"Skipped":    bencodeast.Str("nope"),
			"-":          bencodeast.Str("literal dash"),
			"OptsOnly":   bencodeast.Str("fallback"),
			"uo":         bencodeast.Str("opts ignored"),
			"Untagged":   bencodeast.Str("field name"),
			"unexported": bencodeast.Str("nope"),
		})

		var got tagged
		require.NoError(t, decode(t, input, &got))

		assert.Equal(t, "by tag", got.Named, `tag "named"`)
		assert.Equal(t, "", got.Skipped, `tag "-" means never map this field`)
		assert.Equal(t, "literal dash", got.DashKey, `tag "-," means the literal key -`)
		assert.Equal(t, "fallback", got.OptsOnly, "empty tag name falls back to the field name")
		assert.Equal(t, "opts ignored", got.UnknownOpt, "unknown options are ignored")
		assert.Equal(t, "field name", got.Untagged, "an untagged field matches its Go name")
		assert.Equal(t, "", got.unexported, "unexported fields are never mapped")
	})

	// SPEC §3.3: matching is exact bytes. This is the open question the
	// field-name fallback forces, answered "no case folding".
	t.Run("field name matching is case sensitive", func(t *testing.T) {
		t.Parallel()

		t.Run("exact matches", func(t *testing.T) {
			t.Parallel()
			var got caseSensitive
			require.NoError(t, decode(t, mustEncode(t, bencodeast.Dict{
				"IP": bencodeast.Str("1.2.3.4"),
			}), &got))
			assert.Equal(t, "1.2.3.4", got.IP)
		})

		t.Run("differing case does not match", func(t *testing.T) {
			t.Parallel()
			var got caseSensitive
			require.NoError(t, decode(t, mustEncode(t, bencodeast.Dict{
				"ip": bencodeast.Str("1.2.3.4"),
			}), &got))
			assert.Equal(t, "", got.IP)
		})
	})

	// SPEC §3.3: an ambiguous key binds to nothing, the way encoding/json
	// drops a name two same-depth fields both claim. Declaration order must
	// not decide, so neither field may be populated.
	t.Run("two tagged fields claiming one key: neither wins", func(t *testing.T) {
		t.Parallel()
		var got ambiguous
		require.NoError(t, decode(t, mustEncode(t, bencodeast.Dict{
			"dup": bencodeast.Str("v"),
		}), &got))
		assert.Equal(t, "", got.A)
		assert.Equal(t, "", got.B, "an ambiguous key is skipped, not assigned by position")
	})

	t.Run("ambiguity is sticky", func(t *testing.T) {
		t.Parallel()

		t.Run("three tagged claimants", func(t *testing.T) {
			t.Parallel()
			var got ambiguousThree
			require.NoError(t, decode(t, mustEncode(t, bencodeast.Dict{
				"dup": bencodeast.Str("v"),
			}), &got))
			assert.Equal(t, ambiguousThree{}, got,
				"a third claimant must not inherit a key two others already spoiled")
		})

		t.Run("two tagged claimants then an untagged one", func(t *testing.T) {
			t.Parallel()
			var got ambiguousMixed
			require.NoError(t, decode(t, mustEncode(t, bencodeast.Dict{
				"Dup": bencodeast.Str("w"),
			}), &got))
			assert.Equal(t, ambiguousMixed{}, got,
				"an untagged claimant must not inherit a key two tagged ones already spoiled")
		})
	})

	// SPEC §3.3 rule 1: the one collision that does resolve. Both orderings
	// must give the same answer.
	t.Run("a tagged field beats an untagged one claiming the same key", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{"B": bencodeast.Str("v")})

		t.Run("tagged declared first", func(t *testing.T) {
			t.Parallel()
			var got taggedBeatsUntagged
			require.NoError(t, decode(t, input, &got))
			assert.Equal(t, "v", got.A)
			assert.Equal(t, "", got.B)
		})

		t.Run("tagged declared last", func(t *testing.T) {
			t.Parallel()
			var got untaggedBeforeTagged
			require.NoError(t, decode(t, input, &got))
			assert.Equal(t, "v", got.A)
			assert.Equal(t, "", got.B)
		})
	})

	t.Run("omitempty is accepted and has no decoding effect", func(t *testing.T) {
		t.Parallel()
		// The key is absent, so omitempty has nothing to be tempted by; the
		// field must simply stay zero rather than error.
		var got tagged
		require.NoError(t, decode(t, "de", &got))
		assert.Equal(t, "", got.OptsOnly)
	})

	// SPEC §3.3: embedded fields are ordinary fields, not flattened.
	t.Run("embedded struct is not flattened", func(t *testing.T) {
		t.Parallel()
		type embedded struct {
			simple `bencode:"emb"`
			Extra  string `bencode:"extra"`
		}

		input := mustEncode(t, bencodeast.Dict{
			"emb":   bencodeast.Dict{"s": bencodeast.Str("inner")},
			"extra": bencodeast.Str("outer"),
			"s":     bencodeast.Str("must not leak into the embedded field"),
		})

		var got embedded
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, "inner", got.simple.S)
		assert.Equal(t, "outer", got.Extra)
	})
}

// ---------------------------------------------------------------------------
// SPEC §3.4 — reuse of a non-zero destination
// ---------------------------------------------------------------------------

func TestSpec3_4_Reuse(t *testing.T) {
	t.Parallel()

	t.Run("slice is replaced not appended", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{bencodeast.Str("new")})

		got := []string{"old"}
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, []string{"new"}, got)
	})

	t.Run("map is replaced not merged", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{"new": bencodeast.Str("v")})

		got := map[string]string{"old": "v"}
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, map[string]string{"new": "v"}, got)
	})

	// SPEC §3.4 states this as a contract, not an accident: a reused struct
	// can return a mix of fresh and stale data. Callers who care decode into a
	// zero value.
	t.Run("struct fields absent from the input keep their old value", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{"i": bencodeast.Int(1)})

		got := simple{S: "stale"}
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, "stale", got.S)
		assert.Equal(t, int64(1), got.I)
	})
}

// ---------------------------------------------------------------------------
// SPEC §4 — type mapping, success cases
// ---------------------------------------------------------------------------

func TestSpec4_IntegerDestinations(t *testing.T) {
	t.Parallel()

	t.Run("signed widths", func(t *testing.T) {
		t.Parallel()
		var i8 int8
		var i16 int16
		var i32 int32
		require.NoError(t, decode(t, "i-8e", &i8))
		require.NoError(t, decode(t, "i300e", &i16))
		require.NoError(t, decode(t, "i70000e", &i32))
		assert.Equal(t, int8(-8), i8)
		assert.Equal(t, int16(300), i16)
		assert.Equal(t, int32(70000), i32)
	})

	t.Run("unsigned", func(t *testing.T) {
		t.Parallel()
		var got uint64
		require.NoError(t, decode(t, "i18446744073709551615e", &got))
		assert.Equal(t, uint64(18446744073709551615), got)
	})

	t.Run("float accepts precision loss", func(t *testing.T) {
		t.Parallel()
		var got float64
		require.NoError(t, decode(t, "i42e", &got))
		assert.Equal(t, float64(42), got)
	})

	// SPEC §4: i0e is false, any other integer is true. Consistent with the
	// lenient stance in §2; listed in §10 as still open.
	t.Run("bool", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			in   string
			want bool
		}{
			{"i0e", false},
			{"i1e", true},
			{"i2e", true},
			{"i-1e", true},
		}
		for _, tt := range tests {
			t.Run(tt.in, func(t *testing.T) {
				t.Parallel()
				var got bool
				require.NoError(t, decode(t, tt.in, &got))
				assert.Equal(t, tt.want, got)
			})
		}
	})
}

func TestSpec4_StringDestinations(t *testing.T) {
	t.Parallel()

	t.Run("byte slices are fresh copies that do not alias", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{bencodeast.Str("aaaa"), bencodeast.Str("bbbb")})

		var got [][]byte
		require.NoError(t, decode(t, input, &got))
		require.Len(t, got, 2)

		got[0][0] = 'z'
		assert.Equal(t, []byte("bbbb"), got[1])
	})

	// SPEC §4: a SHA-1 is [20]byte and arrives as a bencode string, not a
	// list. Without this every hash has to be copied out of a []byte by hand.
	t.Run("byte array of exactly the right length", func(t *testing.T) {
		t.Parallel()
		var got [4]byte
		require.NoError(t, decode(t, "4:spam", &got))
		assert.Equal(t, [4]byte{'s', 'p', 'a', 'm'}, got)
	})

	t.Run("sha1 sized array field", func(t *testing.T) {
		t.Parallel()
		hash := repeat("\xab", 20)
		input := mustEncode(t, bencodeast.Dict{"hash": bencodeast.Str(hash)})

		var got sha
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, []byte(hash), got.Hash[:])
	})

	t.Run("length must equal the array exactly", func(t *testing.T) {
		t.Parallel()

		t.Run("shorter must not partially fill", func(t *testing.T) {
			t.Parallel()
			var got [20]byte
			err := decode(t, "4:spam", &got)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrTypeMismatch)
			assert.Equal(t, [20]byte{}, got, "a rejected value must not have been written")
		})

		t.Run("longer is rejected too", func(t *testing.T) {
			t.Parallel()
			var got [4]byte
			err := decode(t, "8:spamspam", &got)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrTypeMismatch)
		})
	})
}

func TestSpec4_ListDestinations(t *testing.T) {
	t.Parallel()

	t.Run("slice of strings", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{bencodeast.Str("spam"), bencodeast.Str("lol")})

		var got []string
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, []string{"spam", "lol"}, got)
	})

	t.Run("slice of ints", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{
			bencodeast.Int(1), bencodeast.Int(2), bencodeast.Int(3),
		})

		var got []int
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, []int{1, 2, 3}, got)
	})

	t.Run("slice of structs", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{
			bencodeast.Dict{"s": bencodeast.Str("x")},
			bencodeast.Dict{"s": bencodeast.Str("y")},
		})

		var got []simple
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, []simple{{S: "x"}, {S: "y"}}, got)
	})

	t.Run("empty list yields a non-nil empty slice", func(t *testing.T) {
		t.Parallel()
		var got []string
		require.NoError(t, decode(t, "le", &got))
		assert.NotNil(t, got)
		assert.Empty(t, got)
	})

	t.Run("array of exact length", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{
			bencodeast.Str("a"), bencodeast.Str("b"), bencodeast.Str("c"),
		})

		var got [3]string
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, [3]string{"a", "b", "c"}, got)
	})

	t.Run("short list leaves the array tail zeroed", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{bencodeast.Str("a"), bencodeast.Str("b")})

		var got [3]string
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, [3]string{"a", "b", ""}, got)
	})

	t.Run("surplus elements are consumed and discarded", func(t *testing.T) {
		t.Parallel()
		d := NewDecoder(strings.NewReader("l1:a1:b1:cei99e"))

		var arr [1]string
		require.NoError(t, decodeWith(t, d, &arr))
		assert.Equal(t, [1]string{"a"}, arr)

		var after int
		require.NoError(t, decodeWith(t, d, &after),
			"surplus elements must be consumed, not left in the stream")
		assert.Equal(t, 99, after)
	})
}

func TestSpec4_DictDestinations(t *testing.T) {
	t.Parallel()

	t.Run("map of strings", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{
			"cow": bencodeast.Str("moo"), "spam": bencodeast.Str("eggs"),
		})

		var got map[string]string
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, map[string]string{"cow": "moo", "spam": "eggs"}, got)
	})

	t.Run("empty dict yields a non-nil empty map", func(t *testing.T) {
		t.Parallel()
		var got map[string]string
		require.NoError(t, decode(t, "de", &got))
		assert.NotNil(t, got)
		assert.Empty(t, got)
	})

	t.Run("empty dict into a struct leaves it zero", func(t *testing.T) {
		t.Parallel()
		var got simple
		require.NoError(t, decode(t, "de", &got))
		assert.Equal(t, simple{}, got)
	})

	t.Run("struct with every container shape", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{
			"strs":    bencodeast.List{bencodeast.Str("a"), bencodeast.Str("b")},
			"map":     bencodeast.Dict{"k": bencodeast.Str("v")},
			"structs": bencodeast.List{bencodeast.Dict{"s": bencodeast.Str("x")}},
			"dicts":   bencodeast.Dict{"y": bencodeast.Dict{"s": bencodeast.Str("z")}},
		})

		var got containers
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, containers{
			Strs:    []string{"a", "b"},
			Map:     map[string]string{"k": "v"},
			Structs: []simple{{S: "x"}},
			Dicts:   map[string]simple{"y": {S: "z"}},
		}, got)
	})

	t.Run("nested struct field", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{
			"n":     bencodeast.Int(7),
			"inner": bencodeast.Dict{"s": bencodeast.Str("deep")},
		})

		var got nested
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, nested{N: 7, Inner: simple{S: "deep"}}, got)
	})

	t.Run("missing keys leave zero values", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{"s": bencodeast.Str("only")})

		var got simple
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, simple{S: "only"}, got)
	})
}

// SPEC §4: the `any` mapping is exact. int64, never int; string, never []byte.
func TestSpec4_AnyMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want any
	}{
		{"integer becomes int64", "i42e", int64(42)},
		{"string becomes string", "4:spam", "spam"},
		{"empty list", "le", []any{}},
		{"empty dict", "de", map[string]any{}},
		{"list", "l4:spami1ee", []any{"spam", int64(1)}},
		{"dict", "d3:cow3:mooe", map[string]any{"cow": "moo"}},
		{
			"dict of list of dict",
			"d5:filesld6:lengthi1e4:pathl1:aeeee",
			map[string]any{
				"files": []any{map[string]any{"length": int64(1), "path": []any{"a"}}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got any
			require.NoError(t, decode(t, tt.in, &got))
			assert.Equal(t, tt.want, got)
		})
	}

	// SPEC §4 flags this trap explicitly: binary blobs like `pieces` come back
	// as strings when decoded through any, so they need []byte(s).
	t.Run("binary data through any is a string", func(t *testing.T) {
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
// SPEC §4 — the destination matrix
//
// Every combination not listed as a success in §4 is ErrTypeMismatch. The
// bytes are well-formed; the target is wrong. Reporting ErrSyntax here sends a
// caller hunting for a corrupt file that does not exist.
// ---------------------------------------------------------------------------

func TestSpec4_DestinationMatrix(t *testing.T) {
	t.Parallel()

	const (
		anInt = "i1e"
		aStr  = "4:spam"
		aList = "l4:spame"
		aDict = "d1:s4:spame"
	)

	type dstCase struct {
		name string
		new  func() any
		want error // nil means the combination is valid
	}

	inputs := []struct {
		kind string
		in   string
		dsts []dstCase
	}{
		{"integer", anInt, []dstCase{
			{"int", func() any { return new(int) }, nil},
			{"uint64", func() any { return new(uint64) }, nil},
			{"float64", func() any { return new(float64) }, nil},
			{"bool", func() any { return new(bool) }, nil},
			{"any", func() any { return new(any) }, nil},
			{"*int", func() any { return new(*int) }, nil},
			{"string", func() any { return new(string) }, ErrTypeMismatch},
			{"[]byte", func() any { return new([]byte) }, ErrTypeMismatch},
			{"[]string", func() any { return new([]string) }, ErrTypeMismatch},
			{"[2]string", func() any { return new([2]string) }, ErrTypeMismatch},
			{"map", func() any { return new(map[string]string) }, ErrTypeMismatch},
			{"struct", func() any { return new(simple) }, ErrTypeMismatch},
			{"non-empty interface", func() any { return new(io.Reader) }, ErrTypeMismatch},
		}},

		{"string", aStr, []dstCase{
			{"string", func() any { return new(string) }, nil},
			{"[]byte", func() any { return new([]byte) }, nil},
			{"[4]byte", func() any { return new([4]byte) }, nil},
			{"any", func() any { return new(any) }, nil},
			{"*string", func() any { return new(*string) }, nil},
			{"int", func() any { return new(int) }, ErrTypeMismatch},
			{"bool", func() any { return new(bool) }, ErrTypeMismatch},
			{"float64", func() any { return new(float64) }, ErrTypeMismatch},
			{"[]string", func() any { return new([]string) }, ErrTypeMismatch},
			{"map", func() any { return new(map[string]string) }, ErrTypeMismatch},
			{"struct", func() any { return new(simple) }, ErrTypeMismatch},
			{"non-empty interface", func() any { return new(io.Reader) }, ErrTypeMismatch},
		}},

		{"list", aList, []dstCase{
			{"[]string", func() any { return new([]string) }, nil},
			{"[2]string", func() any { return new([2]string) }, nil},
			{"any", func() any { return new(any) }, nil},
			{"*[]string", func() any { return new(*[]string) }, nil},
			{"int", func() any { return new(int) }, ErrTypeMismatch},
			{"string", func() any { return new(string) }, ErrTypeMismatch},
			{"[]byte", func() any { return new([]byte) }, ErrTypeMismatch},
			{"map", func() any { return new(map[string]string) }, ErrTypeMismatch},
			{"struct", func() any { return new(simple) }, ErrTypeMismatch},
			{"non-empty interface", func() any { return new(io.Reader) }, ErrTypeMismatch},
		}},

		{"dict", aDict, []dstCase{
			{"map[string]string", func() any { return new(map[string]string) }, nil},
			{"struct", func() any { return new(simple) }, nil},
			{"any", func() any { return new(any) }, nil},
			{"*struct", func() any { return new(*simple) }, nil},
			{"int", func() any { return new(int) }, ErrTypeMismatch},
			{"string", func() any { return new(string) }, ErrTypeMismatch},
			{"[]string", func() any { return new([]string) }, ErrTypeMismatch},
			{"[2]string", func() any { return new([2]string) }, ErrTypeMismatch},
			{"non-empty interface", func() any { return new(io.Reader) }, ErrTypeMismatch},
			// SPEC §4 + §8: rejected before any entry is decoded, and never a
			// panic from SetMapIndex.
			{"map[int]string", func() any { return new(map[int]string) }, ErrTypeMismatch},
		}},
	}

	for _, in := range inputs {
		t.Run(in.kind, func(t *testing.T) {
			t.Parallel()
			for _, dc := range in.dsts {
				t.Run("into "+dc.name, func(t *testing.T) {
					t.Parallel()
					err := decode(t, in.in, dc.new())
					if dc.want == nil {
						assert.NoError(t, err)
						return
					}
					require.Error(t, err)
					assert.ErrorIs(t, err, dc.want)
				})
			}
		})
	}

	t.Run("mismatch reached through a struct field", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{
			"strs": bencodeast.Dict{"a": bencodeast.Str("b")},
		})

		var got containers
		err := decode(t, input, &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrTypeMismatch)
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

// SPEC §5.1: the specific sentinels say how the bytes were malformed and wrap
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

// SPEC §5.2: io.EOF means "no more values"; io.ErrUnexpectedEOF means "broken
// input". Conflating them is how a truncated file looks like a clean one.
func TestSpec5_2_EOF(t *testing.T) {
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

// SPEC §5: strconv, reflect and every other implementation detail stays
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
		nest(200),
	}

	for _, in := range corpus {
		t.Run(fmt.Sprintf("%q", truncateName(in)), func(t *testing.T) {
			t.Parallel()
			var got any
			err := decode(t, in, &got)
			if err == nil {
				return
			}
			for _, s := range sentinels {
				if errors.Is(err, s) {
					return
				}
			}
			t.Fatalf("error %#v matches no sentinel in SPEC §5", err)
		})
	}
}

func truncateName(s string) string {
	if len(s) > 24 {
		return s[:24] + "..."
	}
	return s
}

// ---------------------------------------------------------------------------
// SPEC §6 — stream contract
//
// This is the invariant that keeps a decoder honest. A desynced decoder
// corrupts every value after its first mistake, and nothing else in this file
// would notice.
// ---------------------------------------------------------------------------

func TestSpec6_RecoverableErrorsLeaveTheStreamAligned(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		bad  string // a complete, well-formed value the destination cannot hold
		dst  func() any
	}{
		{"list into int", "li1ee", func() any { return new(int) }},
		{"dict into int", "d1:a1:be", func() any { return new(int) }},
		{"dict into slice", "d1:a1:be", func() any { return new([]string) }},
		{"int into slice", "i1e", func() any { return new([]string) }},
		{"int into string", "i1e", func() any { return new(string) }},
		// The string path is the one that gets this wrong: the payload has
		// already been consumed, so skipping again eats the NEXT value.
		{"string into map", "4:spam", func() any { return new(map[string]string) }},
		{"string into int", "4:spam", func() any { return new(int) }},
		{"nested dict into int", "d1:ad1:bl1:ceee", func() any { return new(int) }},
		{"short string into byte array", "4:spam", func() any { return new([20]byte) }},
		{"non-string key map", "d1:11:ae", func() any { return new(map[int]string) }},
		// SPEC §7.5: overflow is recoverable — the value was fully consumed.
		{"overflow", "i300e", func() any { return new(int8) }},
		{"overflow beyond int64", "i99999999999999999999e", func() any { return new(int64) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := NewDecoder(strings.NewReader(tt.bad + "i42e"))

			require.Error(t, decodeWith(t, d, tt.dst()))

			var after int
			require.NoError(t, decodeWith(t, d, &after),
				"decoder desynced: the rejected value was not consumed exactly")
			assert.Equal(t, 42, after)
		})
	}
}

func TestSpec6_Poisoning(t *testing.T) {
	t.Parallel()

	poisoning := []struct {
		name string
		bad  string
	}{
		{"syntax error", "x"},
		{"malformed int", "iabce"},
		{"truncated value", "l"},
		{"nesting over the limit", nest(200)},
	}

	for _, tt := range poisoning {
		t.Run(tt.name+" is sticky", func(t *testing.T) {
			t.Parallel()
			d := NewDecoder(strings.NewReader(tt.bad + "i42e"))

			first := decodeWith(t, d, new(any))
			require.Error(t, first)

			var after int
			second := decodeWith(t, d, &after)
			require.Error(t, second,
				"a poisoned decoder must not pretend it found the next value")
			assert.Equal(t, first, second, "the stored error is returned verbatim")
			assert.Equal(t, 0, after)
		})
	}

	t.Run("a poisoned decoder reads nothing further", func(t *testing.T) {
		t.Parallel()
		r := &countingReader{src: strings.NewReader("x" + repeat("i42e", 1000))}
		d := NewDecoder(r)

		require.Error(t, decodeWith(t, d, new(any)))
		before := r.read

		require.Error(t, decodeWith(t, d, new(any)))
		assert.Equal(t, before, r.read, "poisoned decoder pulled more bytes")
	})
}

func TestSpec6_SuccessivePositioning(t *testing.T) {
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

// ---------------------------------------------------------------------------
// SPEC §7 — limits
//
// Hostile input is the normal case: a torrent client parses files and peer
// messages from strangers. "It errors eventually" is not good enough — it has
// to error without spending the machine's memory or stack first.
// ---------------------------------------------------------------------------

func TestSpec7_Depth(t *testing.T) {
	t.Parallel()

	// 200 is over the documented default of 128 and shallow enough to be
	// harmless if the limit is missing. Do not raise it to provoke a real
	// stack overflow: that is not a panic, cannot be recovered, and takes the
	// whole test binary down.
	t.Run("over the limit is ErrMaxDepth", func(t *testing.T) {
		t.Parallel()
		var got any
		err := decode(t, nest(200), &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrMaxDepth)
	})

	t.Run("under the limit is accepted", func(t *testing.T) {
		t.Parallel()
		var got any
		require.NoError(t, decode(t, nest(100), &got))
	})

	t.Run("the limit applies to dicts too", func(t *testing.T) {
		t.Parallel()
		var got any
		err := decode(t, repeat("d1:k", 200)+repeat("e", 200), &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrMaxDepth)
	})

	// A depth limit that only guards values you keep is not a limit.
	t.Run("the limit applies while skipping", func(t *testing.T) {
		t.Parallel()
		input := "d9:a_unknown" + nest(200) + "1:s5:helloe"

		var got simple
		err := decode(t, input, &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrMaxDepth)
	})
}

func TestSpec7_BoundedScans(t *testing.T) {
	t.Parallel()

	const junk = 8 << 20 // no terminator anywhere in 8 MiB
	const allowed = 1 << 20

	t.Run("integer with no terminator", func(t *testing.T) {
		t.Parallel()
		r := &countingReader{src: strings.NewReader("i" + repeat("1", junk))}

		var got int
		err := NewDecoder(r).Decode(&got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrExceedsMax)
		assert.Less(t, r.read, allowed,
			"read %d bytes hunting for 'e'; the scan must be bounded", r.read)
	})

	t.Run("string length with no colon", func(t *testing.T) {
		t.Parallel()
		r := &countingReader{src: strings.NewReader(repeat("1", junk))}

		var got string
		err := NewDecoder(r).Decode(&got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrExceedsMax)
		assert.Less(t, r.read, allowed,
			"read %d bytes hunting for ':'; the scan must be bounded", r.read)
	})

	// SPEC §7.3. Skipping is exactly where limits get forgotten.
	t.Run("skipping an unterminated integer is bounded", func(t *testing.T) {
		t.Parallel()
		r := &countingReader{src: strings.NewReader("d9:a_unknowni" + repeat("1", junk))}

		var got simple
		err := NewDecoder(r).Decode(&got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrExceedsMax)
		assert.Less(t, r.read, allowed,
			"read %d bytes skipping an unterminated int", r.read)
	})

	// SPEC §7.5: inside the scan window but outside int64 is a recoverable
	// ErrOverflow; beyond the window it is ErrExceedsMax and the decoder dies.
	t.Run("oversized integer is ErrExceedsMax not ErrOverflow", func(t *testing.T) {
		t.Parallel()
		var got int64
		err := decode(t, "i"+repeat("9", 200)+"e", &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrExceedsMax)
	})
}

func TestSpec7_StringSize(t *testing.T) {
	t.Parallel()

	// SPEC §7.1: the declared length is checked before anything is allocated.
	// 9 MiB is over the 8 MiB default; the payload is four bytes long, so an
	// implementation that allocates first will also read far past the limit.
	t.Run("declared length over the limit is rejected before allocating", func(t *testing.T) {
		t.Parallel()
		var got []byte
		err := decode(t, "9000000:tiny", &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrExceedsMax)
		assert.Nil(t, got)
	})

	t.Run("declared length under the limit but past the payload is a truncation", func(t *testing.T) {
		t.Parallel()
		var got []byte
		err := decode(t, "1000000:short", &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
	})

	t.Run("a large but legal string decodes", func(t *testing.T) {
		t.Parallel()
		const n = 1 << 20
		var got []byte
		require.NoError(t, decode(t, fmt.Sprintf("%d:%s", n, repeat("x", n)), &got))
		assert.Len(t, got, n)
	})
}

// SPEC §7.4: a peer that sends one unknown key holding a large value must not
// make you allocate all of it for nothing.
func TestSpec7_SkipDoesNotAllocateTheSkippedValue(t *testing.T) {
	// Not parallel: measures process-wide allocation.

	const size = 4 << 20
	input := "d9:a_unknown" + fmt.Sprintf("%d:%s", size, repeat("x", size)) + "1:s5:helloe"

	var got simple
	var err error
	allocated := allocatedBytes(func() {
		err = decode(t, input, &got)
	})

	require.NoError(t, err)
	require.Equal(t, "hello", got.S, "the decoder lost sync while skipping")
	assert.Less(t, allocated, uint64(size/2),
		"allocated %d bytes to skip a %d byte value it throws away", allocated, size)
}

// ---------------------------------------------------------------------------
// SPEC §8 — never panics
// ---------------------------------------------------------------------------

func TestSpec8_NeverPanics(t *testing.T) {
	t.Parallel()

	// Destinations chosen for the reflect operations that panic when unguarded:
	// SetMapIndex with a mismatched key type, Set with an unassignable value.
	dsts := []struct {
		name string
		new  func() any
	}{
		{"any", func() any { return new(any) }},
		{"int", func() any { return new(int) }},
		{"string", func() any { return new(string) }},
		{"[4]byte", func() any { return new([4]byte) }},
		// Same length as "spam", but reflect.Copy rejects arrays of a
		// different element type — a length check alone does not make the
		// copy legal.
		{"[4]string", func() any { return new([4]string) }},
		// Kind() is String but Type() is not string, so a plain
		// AssignableTo on the key fails where a Convert would succeed.
		{"map[namedKey]string", func() any { return new(map[namedKey]string) }},
		{"map[int]string", func() any { return new(map[int]string) }},
		{"map[string]int", func() any { return new(map[string]int) }},
		{"map[bool]any", func() any { return new(map[bool]any) }},
		{"non-empty interface", func() any { return new(io.Reader) }},
		{"chan", func() any { return new(chan int) }},
		{"func", func() any { return new(func()) }},
		{"struct", func() any { return new(simple) }},
		{"pointer", func() any { return new(*simple) }},
	}

	inputs := []string{
		"", "x", "e", "i1e", "i-1e", "4:spam", "0:", "le", "de",
		"l4:spame", "li1ee", "d1:s4:spame", "d1:si1ee", "di1ei2ee",
		"d1:11:ae", "l" + repeat("i1e", 5) + "e", "d0:0:e",
	}

	for _, dst := range dsts {
		for _, in := range inputs {
			t.Run(dst.name+"/"+fmt.Sprintf("%q", truncateName(in)), func(t *testing.T) {
				t.Parallel()
				// decode() turns any panic into a hard failure. The returned
				// error is irrelevant here — only the absence of a panic is.
				_ = decode(t, in, dst.new())
			})
		}
	}
}

// ---------------------------------------------------------------------------
// real input
// ---------------------------------------------------------------------------

type torrentFileEntry struct {
	Length int64    `bencode:"length"`
	Path   []string `bencode:"path"`
}

type torrentInfo struct {
	Name        string             `bencode:"name"`
	PieceLength int64              `bencode:"piece length"`
	Pieces      []byte             `bencode:"pieces"`
	Length      int64              `bencode:"length"`
	Files       []torrentFileEntry `bencode:"files"`
	Private     *int64             `bencode:"private"`
}

type torrentMeta struct {
	Announce     string      `bencode:"announce"`
	AnnounceList [][]string  `bencode:"announce-list"`
	Comment      string      `bencode:"comment"`
	CreatedBy    string      `bencode:"created by"`
	CreationDate int64       `bencode:"creation date"`
	Encoding     string      `bencode:"encoding"`
	Info         torrentInfo `bencode:"info"`
}

func TestDecode_RealTorrent(t *testing.T) {
	t.Parallel()

	matches, err := filepath.Glob(filepath.Join("..", "..", "testdata", "*.torrent"))
	require.NoError(t, err)
	require.NotEmpty(t, matches, "no .torrent file found in testdata")

	f, err := os.Open(matches[0])
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	var got torrentMeta
	require.NoError(t, NewDecoder(f).Decode(&got))

	assert.NotEmpty(t, got.Announce)
	assert.NotEmpty(t, got.Info.Name)
	assert.Positive(t, got.Info.PieceLength)
	require.NotEmpty(t, got.Info.Pieces)
	assert.Zero(t, len(got.Info.Pieces)%20, "pieces must be a whole number of SHA-1 hashes")

	// This fixture is multi-file, so `files` is populated and `length` is not.
	require.NotEmpty(t, got.Info.Files)
	assert.Zero(t, got.Info.Length)
	for _, fe := range got.Info.Files {
		assert.Positive(t, fe.Length)
		assert.NotEmpty(t, fe.Path)
	}

	// SPEC §3.2: the key is absent from this file, so the pointer stays nil —
	// distinguishable from a present `i0e`.
	assert.Nil(t, got.Info.Private)
}

// ---------------------------------------------------------------------------
// fuzz
//
// The tables above assert what the spec says. Fuzzing asserts the two things a
// table can never cover: that no input panics (SPEC §8), and that decoding is
// self-consistent.
//
// The oracle is deliberately re-encode-and-redecode rather than a differential
// comparison against bencode_ast: that decoder has its own limits and its own
// empty-key handling, so disagreement between them would prove nothing.
// ---------------------------------------------------------------------------

func FuzzDecodeAny(f *testing.F) {
	seeds := []string{
		"i0e", "i-1e", "i42e", "0:", "4:spam", "le", "de",
		"l4:spami1ee", "d3:cow3:mooe", "d1:ald1:bi1eeee",
		"d5:filesld6:lengthi1e4:pathl1:aeeee",
		"x", "i", "iabce", "4spam", "10:abc", "l", "d3:foo", "di1ei2ee",
		"i03e", "i-0e", "05:hello", nest(20),
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, in []byte) {
		var first any
		if err := NewDecoder(bytes.NewReader(in)).Decode(&first); err != nil {
			return
		}

		v, ok := toAST(first)
		require.True(t, ok, "decoded into %T, which is outside the any mapping in SPEC §4", first)

		var buf bytes.Buffer
		require.NoError(t, bencodeast.Encode(&buf, v))

		var second any
		require.NoError(t, NewDecoder(&buf).Decode(&second),
			"a value this decoder produced did not survive a round trip")
		require.True(t, reflect.DeepEqual(first, second),
			"decoding is not idempotent:\nfirst:  %#v\nsecond: %#v", first, second)
	})
}

// toAST converts the result of decoding into `any` back into a bencode AST.
// It fails on any type outside the mapping SPEC §4 promises, which is itself
// worth catching.
func toAST(v any) (bencodeast.Value, bool) {
	switch v := v.(type) {
	case string:
		return bencodeast.Str(v), true
	case int64:
		return bencodeast.Int(v), true
	case []any:
		l := make(bencodeast.List, 0, len(v))
		for _, e := range v {
			ev, ok := toAST(e)
			if !ok {
				return nil, false
			}
			l = append(l, ev)
		}
		return l, true
	case map[string]any:
		d := make(bencodeast.Dict, len(v))
		for k, e := range v {
			ev, ok := toAST(e)
			if !ok {
				return nil, false
			}
			d[k] = ev
		}
		return d, true
	default:
		return nil, false
	}
}

func TestGetTag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		tag        string
		expected   string
		expectedOk bool
	}{
		{tag: "tag", expected: "tag", expectedOk: true},
		{tag: "tag,", expected: "tag", expectedOk: true},
		{tag: "-,", expected: "-", expectedOk: true},
		{tag: "tag,opt1", expected: "tag", expectedOk: true},
		{tag: ",opt1", expected: "", expectedOk: true},
		{tag: "", expected: "", expectedOk: true},
		{tag: "-", expected: ""},
	}

	for _, tt := range tests {
		t.Run(tt.tag, func(t *testing.T) {
			t.Parallel()
			tag, ok := getTag(tt.tag)
			require.Equal(t, tt.expected, tag)
			assert.Equal(t, tt.expectedOk, ok)

		})
	}
}

func TestSplitTag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		tag      string
		expected []string
	}{
		{tag: "tag", expected: []string{"tag"}},
		{tag: "tag,opt1", expected: []string{"tag", "opt1"}},
		{tag: "tag,opt1,opt2", expected: []string{"tag", "opt1", "opt2"}},
		{tag: "-", expected: []string{"-"}},
		{tag: ",opt", expected: []string{"", "opt"}},
	}

	for _, tt := range tests {
		t.Run(tt.tag, func(t *testing.T) {
			t.Parallel()
			opts := splitTag(tt.tag)
			require.Equal(t, tt.expected, opts)
		})
	}
}

func TestCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		s        string
		ch       byte
		expected int
	}{
		{"test", 't', 2},
		{"test", 'a', 0},
		{"test", 'e', 1},
		{"test,", ',', 1},
		{"", ',', 0},
		{"test", 0, 0},
		{"test", 0, 0},
	}

	for _, tt := range tests {
		t.Run(tt.s, func(t *testing.T) {
			t.Parallel()
			c := count(tt.s, tt.ch)
			require.Equal(t, tt.expected, c)
		})
	}
}
