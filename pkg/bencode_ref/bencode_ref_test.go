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
	"sync"
	"testing"

	bencodeast "github.com/ARUMANDESU/gotorrent/pkg/bencode_ast"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file asserts SPEC.md draft 2 and nothing else. Every test cites the
// section it enforces. When a test and the implementation disagree, the spec
// decides which one is wrong — a test that merely describes what the code
// happens to do today is worthless, because it can never fail for a reason
// worth knowing.
//
// Not covered here: SPEC §9 RawMessage, which is specified but deliberately
// not implemented yet. Add its tests with the feature.

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

// ambiguous pins SPEC §3.3.1: two tagged fields claiming one key bind to
// nothing. Declaration order must not decide.
type ambiguous struct {
	A string `bencode:"dup"`
	B string `bencode:"dup"`
}

// taggedBeatsUntagged pins the one collision SPEC §3.3.1 does resolve. The two
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

// optsOnlyVsUntagged pins SPEC §3.3 rules 3 and 4 read together: a tag that
// supplies no name does NOT make the field tagged for §3.3.1. Both fields here
// are untagged claimants of "V", so the key is ambiguous. If `,omitempty`
// counted as tagged, A would win and this test would fail.
type optsOnlyVsUntagged struct {
	A string `bencode:"V,omitempty"`
	V string
}

type caseSensitive struct {
	IP string
}

// namedKey is string-kinded but not string-typed, which is the difference
// between Kind() and Type() when guarding SetMapIndex.
type namedKey string

// The embedded fixtures below split a case draft 1 conflated. SPEC §3.3 rule 1
// skips every unexported field, and for an embedded field the field's name is
// its type's name — so `simple` embedded is an UNEXPORTED field and must be
// skipped, while `Exported` embedded must not be.
type Exported struct {
	S string `bencode:"s"`
}

type embedsExported struct {
	Exported `bencode:"emb"`
	Extra    string `bencode:"extra"`
}

type unexportedMap map[string]int

type unexportedSlice []string

type embedsUnexportedStruct struct {
	simple
	Extra string `bencode:"extra"`
}

type embedsUnexportedMap struct {
	unexportedMap
	Extra string `bencode:"extra"`
}

type embedsUnexportedSlice struct {
	unexportedSlice
	Extra string `bencode:"extra"`
}

type sha struct {
	Hash [20]byte `bencode:"hash"`
}

type unsigned struct {
	U uint64 `bencode:"u"`
}

type boolField struct {
	B bool `bencode:"b"`
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

// tinyLimits shrinks every bound in SPEC §7 to something a test can exceed in
// a few dozen bytes. The properties being asserted are about behaviour AT the
// boundary, never about where the boundary sits, so testing them at 8 MiB only
// buys a slower suite and multi-megabyte fixtures.
func tinyLimits() Limits {
	return Limits{
		MaxStringBytes:  64,
		MaxValueBytes:   256,
		MaxCaptureBytes: 64,
		MaxDepth:        16,
	}
}

// tinyDecoder is a Decoder over input with tinyLimits already applied.
func tinyDecoder(input string) *Decoder {
	d := NewDecoder(strings.NewReader(input))
	d.Limits = tinyLimits()
	return d
}

// countingReader reports how many bytes the decoder actually pulled, proving
// it gives up on a hostile value instead of buffering the stream while it
// hunts for a terminator (SPEC §7.2), and that a poisoned decoder reads
// nothing further (SPEC §6.1).
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
//
// This reads process-wide counters, so it is only meaningful while nothing
// else in the binary is allocating. `go test` runs non-parallel tests to
// completion before resuming parallel ones, which is what keeps this honest —
// the caller must NOT call t.Parallel().
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

func truncateName(s string) string {
	if len(s) > 24 {
		return s[:24] + "..."
	}
	return s
}

// everyDestinationShape is the set of destinations that exercise every branch
// of SPEC §4, including the ones whose reflect operations panic when
// unguarded. Shared between the §8 table and the §8 fuzz target so the two can
// never drift apart.
var everyDestinationShape = []struct {
	name string
	new  func() any
}{
	{"any", func() any { return new(any) }},
	{"int", func() any { return new(int) }},
	{"int8", func() any { return new(int8) }},
	{"uint64", func() any { return new(uint64) }},
	{"float64", func() any { return new(float64) }},
	{"bool", func() any { return new(bool) }},
	{"string", func() any { return new(string) }},
	{"[]byte", func() any { return new([]byte) }},
	{"[4]byte", func() any { return new([4]byte) }},
	// Same length as "spam", but reflect.Copy rejects arrays of a different
	// element type — a length check alone does not make the copy legal.
	{"[4]string", func() any { return new([4]string) }},
	{"[]string", func() any { return new([]string) }},
	// Kind() is String but Type() is not string, so a plain AssignableTo on
	// the key fails where a Convert would succeed.
	{"map[namedKey]string", func() any { return new(map[namedKey]string) }},
	{"map[int]string", func() any { return new(map[int]string) }},
	{"map[string]int", func() any { return new(map[string]int) }},
	{"map[bool]any", func() any { return new(map[bool]any) }},
	{"non-empty interface", func() any { return new(io.Reader) }},
	{"chan", func() any { return new(chan int) }},
	{"func", func() any { return new(func()) }},
	{"struct", func() any { return new(simple) }},
	{"pointer to struct", func() any { return new(*simple) }},
	{"embedded unexported map", func() any { return new(embedsUnexportedMap) }},
}

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

	// SPEC §6.1: ErrInvalidDestination is the sole error that does not poison,
	// because it is raised before the reader is touched. The caller's argument
	// was bad; the stream is untouched.
	t.Run("consumes no bytes and leaves the decoder usable", func(t *testing.T) {
		t.Parallel()
		r := &countingReader{src: strings.NewReader("i42e")}
		d := NewDecoder(r)

		var bad int
		require.ErrorIs(t, decodeWith(t, d, bad), ErrInvalidDestination)
		assert.Zero(t, r.read, "a rejected destination must not have moved the stream")

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

	// SPEC §3.2, second half: "present AND BOUND". An ambiguous key is present
	// in the input and binds to nothing, so it must allocate nothing either.
	t.Run("a present but ambiguous key allocates nothing", func(t *testing.T) {
		t.Parallel()
		type ambiguousPtr struct {
			A *string `bencode:"dup"`
			B *string `bencode:"dup"`
		}

		input := mustEncode(t, bencodeast.Dict{"dup": bencodeast.Str("v")})

		var got ambiguousPtr
		require.NoError(t, decode(t, input, &got))
		assert.Nil(t, got.A)
		assert.Nil(t, got.B)
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

	// SPEC §3.3.1 rule 2: an ambiguous key binds to nothing, the way
	// encoding/json drops a name two same-depth fields both claim. Declaration
	// order must not decide, so neither field may be populated.
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

	// SPEC §3.3.1 rule 1: the one collision that does resolve. Both orderings
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

	// SPEC §3.3 rules 3 and 4: only a tag that supplies a NAME makes a field
	// tagged. `bencode:",omitempty"` supplies options and no name, so it is an
	// untagged claimant and cannot win rule 1.
	t.Run("an options-only tag does not make a field tagged", func(t *testing.T) {
		t.Parallel()
		var got optsOnlyVsUntagged
		require.NoError(t, decode(t, mustEncode(t, bencodeast.Dict{
			"V": bencodeast.Str("v"),
		}), &got))
		assert.Equal(t, optsOnlyVsUntagged{A: "v"}, got,
			"two untagged claimants are ambiguous; an options-only tag must not break the tie")
	})

	t.Run("omitempty is accepted and has no decoding effect", func(t *testing.T) {
		t.Parallel()
		// The key is absent, so omitempty has nothing to be tempted by; the
		// field must simply stay zero rather than error.
		var got tagged
		require.NoError(t, decode(t, "de", &got))
		assert.Equal(t, "", got.OptsOnly)
	})
}

// SPEC §3.3 rule 1 says "unexported → always skipped, INCLUDING embedded
// fields", and §3.1 depends on it: admitting a read-only reflect.Value into
// the decode path turns the internal CanSet assertions into reachable code and
// panics in the container paths, which call Set on the whole value.
//
// An embedded field's name is its type's name, so `simple` embedded is an
// unexported field. Draft 1 admitted it and appeared to work — but only for
// struct-typed embeds, because reflect does not propagate flagEmbedRO to the
// fields underneath. Map- and slice-typed embeds panic on the Set at the end
// of their branch. That is one working shape out of four, not a feature.
func TestSpec3_3_EmbeddedFields(t *testing.T) {
	t.Parallel()

	t.Run("an exported embedded field is an ordinary field, not flattened", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{
			"emb":   bencodeast.Dict{"s": bencodeast.Str("inner")},
			"extra": bencodeast.Str("outer"),
			"s":     bencodeast.Str("must not leak into the embedded field"),
		})

		var got embedsExported
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, "inner", got.Exported.S)
		assert.Equal(t, "outer", got.Extra)
	})

	// One subtest per underlying kind, because the failure mode differs: the
	// struct case silently populates, the map and slice cases panic.
	t.Run("unexported embedded fields are skipped for every underlying kind", func(t *testing.T) {
		t.Parallel()

		t.Run("struct", func(t *testing.T) {
			t.Parallel()
			input := mustEncode(t, bencodeast.Dict{
				"simple": bencodeast.Dict{"s": bencodeast.Str("must not bind")},
				"extra":  bencodeast.Str("kept"),
			})

			var got embedsUnexportedStruct
			require.NoError(t, decode(t, input, &got))
			assert.Equal(t, simple{}, got.simple,
				"the type name of an unexported embed is not a key")
			assert.Equal(t, "kept", got.Extra, "the decoder must stay in sync while skipping it")
		})

		t.Run("map", func(t *testing.T) {
			t.Parallel()
			input := mustEncode(t, bencodeast.Dict{
				"unexportedMap": bencodeast.Dict{"k": bencodeast.Int(1)},
				"extra":         bencodeast.Str("kept"),
			})

			var got embedsUnexportedMap
			require.NoError(t, decode(t, input, &got))
			assert.Nil(t, got.unexportedMap)
			assert.Equal(t, "kept", got.Extra)
		})

		t.Run("slice", func(t *testing.T) {
			t.Parallel()
			input := mustEncode(t, bencodeast.Dict{
				"unexportedSlice": bencodeast.List{bencodeast.Str("a")},
				"extra":           bencodeast.Str("kept"),
			})

			var got embedsUnexportedSlice
			require.NoError(t, decode(t, input, &got))
			assert.Nil(t, got.unexportedSlice)
			assert.Equal(t, "kept", got.Extra)
		})
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

	// SPEC §3.4: an array is replaced wholesale and the tail is zeroed — a
	// reused array must not show leftovers past the input's length.
	t.Run("array tail is zeroed on reuse", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{bencodeast.Str("new")})

		got := [3]string{"old1", "old2", "old3"}
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, [3]string{"new", "", ""}, got)
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
	// lenient stance in §2.1; listed in §11 as still open.
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

	// SPEC §4: "the destination is ALWAYS assigned, never left as it was".
	// The natural implementation — `if v != 0 { SetBool(true) }` — passes
	// every test above and fails this one, which is exactly why it is here.
	// Destinations get reused (§3.4), and a false that cannot overwrite a true
	// is a stale value with no way to detect it.
	t.Run("i0e clears a destination that was already true", func(t *testing.T) {
		t.Parallel()

		t.Run("top level", func(t *testing.T) {
			t.Parallel()
			got := true
			require.NoError(t, decode(t, "i0e", &got))
			assert.False(t, got)
		})

		t.Run("struct field", func(t *testing.T) {
			t.Parallel()
			input := mustEncode(t, bencodeast.Dict{"b": bencodeast.Int(0)})

			got := boolField{B: true}
			require.NoError(t, decode(t, input, &got))
			assert.False(t, got.B)
		})
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

	// SPEC §4.1: strings into arrays are strict where lists into arrays
	// truncate, because an array destination for a string is almost always a
	// fixed-width identifier. A partially filled SHA-1 is not a degraded
	// hash — it is a different hash that compares unequal far from its cause.
	t.Run("length must equal the array exactly", func(t *testing.T) {
		t.Parallel()

		t.Run("shorter must not partially fill", func(t *testing.T) {
			t.Parallel()
			var got [20]byte
			err := decode(t, "4:spam", &got)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrArrayLength, "the type matched; only the length did not")
			assert.ErrorIs(t, err, ErrTypeMismatch, "must still classify as a mismatch")
			assert.Equal(t, [20]byte{}, got, "a rejected value must not have been written")
		})

		t.Run("longer is rejected too", func(t *testing.T) {
			t.Parallel()
			var got [4]byte
			err := decode(t, "8:spamspam", &got)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrArrayLength)
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

	// SPEC §4.1: an array destination for a LIST is a capacity choice —
	// "give me at most N" — so surplus is discarded, not rejected. It must
	// still be consumed, or the next Decode reads the leftovers.
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

	// SPEC §4: string-KINDED keys are supported, including named string types.
	// This is the combination that panics in SetMapIndex without an explicit
	// Convert — §8 lists it as a standing panic source, and this test is the
	// positive half: it must not merely avoid panicking, it must work.
	t.Run("named string key type", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{
			"cow": bencodeast.Str("moo"), "spam": bencodeast.Str("eggs"),
		})

		var got map[namedKey]string
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, map[namedKey]string{"cow": "moo", "spam": "eggs"}, got)
	})

	t.Run("named string key type inside a struct field", func(t *testing.T) {
		t.Parallel()
		type namedKeyField struct {
			M map[namedKey]int `bencode:"m"`
		}
		input := mustEncode(t, bencodeast.Dict{"m": bencodeast.Dict{"k": bencodeast.Int(1)}})

		var got namedKeyField
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, map[namedKey]int{"k": 1}, got.M)
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
			{"[20]byte", func() any { return new([20]byte) }, ErrArrayLength},
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
			{"map[namedKey]string", func() any { return new(map[namedKey]string) }, nil},
			{"struct", func() any { return new(simple) }, nil},
			{"any", func() any { return new(any) }, nil},
			{"*struct", func() any { return new(*simple) }, nil},
			{"int", func() any { return new(int) }, ErrTypeMismatch},
			{"string", func() any { return new(string) }, ErrTypeMismatch},
			{"[]string", func() any { return new([]string) }, ErrTypeMismatch},
			{"[2]string", func() any { return new([2]string) }, ErrTypeMismatch},
			{"non-empty interface", func() any { return new(io.Reader) }, ErrTypeMismatch},
			// SPEC §4 + §8: rejected before the map is allocated and before
			// any entry is decoded, and never a panic from SetMapIndex.
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

		// SPEC §11 lists Struct/Field as an open decision. If they are dropped
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

// ---------------------------------------------------------------------------
// SPEC §7 — limits
//
// Hostile input is the normal case: a torrent client parses files and peer
// messages from strangers. "It errors eventually" is not good enough — it has
// to error without spending the machine's memory or stack first.
// ---------------------------------------------------------------------------

// SPEC §7.1: Limits is read at the start of each Decode, and a zero field
// means "use the default". Without the second half, a caller who overrides one
// bound silently sets every other one to zero and nothing decodes at all.
func TestSpec7_1_Configuration(t *testing.T) {
	t.Parallel()

	t.Run("zero fields fall back to defaults", func(t *testing.T) {
		t.Parallel()
		d := NewDecoder(strings.NewReader("d1:s4:spame"))
		d.Limits = Limits{MaxDepth: 8} // everything else zero

		var got simple
		require.NoError(t, decodeWith(t, d, &got),
			"a zero MaxStringBytes must mean the default, not a zero-byte ceiling")
		assert.Equal(t, "spam", got.S)
	})

	t.Run("an override takes effect", func(t *testing.T) {
		t.Parallel()
		var got string
		err := decodeWith(t, tinyDecoder("100:"+repeat("x", 100)), &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrExceedsMax)
	})

	t.Run("limits are re-read between calls", func(t *testing.T) {
		t.Parallel()
		big := "100:" + repeat("x", 100)
		d := NewDecoder(strings.NewReader(big + big))

		var first string
		require.NoError(t, decodeWith(t, d, &first), "default limits accept this")

		d.Limits.MaxStringBytes = 64

		var second string
		err := decodeWith(t, d, &second)
		require.Error(t, err, "the tightened limit must apply to the next value")
		assert.ErrorIs(t, err, ErrExceedsMax)
	})
}

func TestSpec7_Depth(t *testing.T) {
	t.Parallel()

	t.Run("over the limit is ErrMaxDepth", func(t *testing.T) {
		t.Parallel()
		var got any
		err := decodeWith(t, tinyDecoder(nest(64)), &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrMaxDepth)
	})

	t.Run("under the limit is accepted", func(t *testing.T) {
		t.Parallel()
		var got any
		require.NoError(t, decodeWith(t, tinyDecoder(nest(8)), &got))
	})

	// The default is exercised separately from the configurable path, because
	// a bug that reads the wrong field would pass every tinyLimits test.
	// 200 is over the documented default of 128 and shallow enough to be
	// harmless if the limit is missing entirely. Do not raise it to provoke a
	// real stack overflow: that is not a panic, cannot be recovered, and takes
	// the whole test binary down.
	t.Run("the default limit applies when unconfigured", func(t *testing.T) {
		t.Parallel()
		var got any
		err := decode(t, nest(200), &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrMaxDepth)

		var ok any
		require.NoError(t, decode(t, nest(100), &ok))
	})

	t.Run("the limit applies to dicts too", func(t *testing.T) {
		t.Parallel()
		var got any
		err := decodeWith(t, tinyDecoder(repeat("d1:k", 64)+repeat("e", 64)), &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrMaxDepth)
	})

	// SPEC §7.2 rule 3. A depth limit that only guards values you keep is not
	// a limit.
	t.Run("the limit applies while skipping", func(t *testing.T) {
		t.Parallel()
		input := "d9:a_unknown" + nest(64) + "1:s5:helloe"

		var got simple
		err := decodeWith(t, tinyDecoder(input), &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrMaxDepth)
	})
}

func TestSpec7_2_BoundedScans(t *testing.T) {
	t.Parallel()

	// With tinyLimits the scan bounds are tens of bytes, so a few hundred
	// bytes of junk is already far past every one of them. The assertion is
	// that the decoder stops early — not where exactly it stops.
	const junk = 4096
	const allowed = 512

	t.Run("integer with no terminator", func(t *testing.T) {
		t.Parallel()
		r := &countingReader{src: strings.NewReader("i" + repeat("1", junk))}
		d := NewDecoder(r)
		d.Limits = tinyLimits()

		var got int
		err := decodeWith(t, d, &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrExceedsMax)
		assert.Less(t, r.read, allowed,
			"read %d bytes hunting for 'e'; the scan must be bounded", r.read)
	})

	t.Run("string length with no colon", func(t *testing.T) {
		t.Parallel()
		r := &countingReader{src: strings.NewReader(repeat("1", junk))}
		d := NewDecoder(r)
		d.Limits = tinyLimits()

		var got string
		err := decodeWith(t, d, &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrExceedsMax)
		assert.Less(t, r.read, allowed,
			"read %d bytes hunting for ':'; the scan must be bounded", r.read)
	})

	// SPEC §7.2 rule 3. Skipping is exactly where limits get forgotten.
	t.Run("skipping an unterminated integer is bounded", func(t *testing.T) {
		t.Parallel()
		r := &countingReader{src: strings.NewReader("d9:a_unknowni" + repeat("1", junk))}
		d := NewDecoder(r)
		d.Limits = tinyLimits()

		var got simple
		err := decodeWith(t, d, &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrExceedsMax)
		assert.Less(t, r.read, allowed,
			"read %d bytes skipping an unterminated int", r.read)
	})

	// SPEC §7.2 rule 5: inside the internal scan window but outside int64 is
	// ErrOverflow; beyond the window it is ErrExceedsMax. Both poison — the
	// distinction is diagnostic. maxIntBytes is not configurable, so this uses
	// the real bound.
	t.Run("oversized integer is ErrExceedsMax not ErrOverflow", func(t *testing.T) {
		t.Parallel()
		var got int64
		err := decode(t, "i"+repeat("9", 200)+"e", &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrExceedsMax)
		assert.NotErrorIs(t, err, ErrOverflow)
	})

	t.Run("an integer just outside int64 is ErrOverflow", func(t *testing.T) {
		t.Parallel()
		var got int64
		err := decode(t, "i99999999999999999999e", &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrOverflow)
		assert.NotErrorIs(t, err, ErrExceedsMax)
	})
}

func TestSpec7_1_StringSize(t *testing.T) {
	t.Parallel()

	// SPEC §7.2 rule 1: the DECLARED length is checked before anything is
	// allocated. The payload here is four bytes, so an implementation that
	// allocates first will also read far past the limit.
	t.Run("declared length over the limit is rejected before allocating", func(t *testing.T) {
		t.Parallel()
		var got []byte
		err := decodeWith(t, tinyDecoder("100:tiny"), &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrExceedsMax)
		assert.Nil(t, got)
	})

	t.Run("declared length under the limit but past the payload is a truncation", func(t *testing.T) {
		t.Parallel()
		var got []byte
		err := decodeWith(t, tinyDecoder("60:short"), &got)
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

// SPEC §7.3 — the limit draft 1 was missing. Depth and string length together
// bound nothing that grows by REPETITION: a million-element list of `i0e`
// passes every other check while the destination slice grows linearly with the
// input, and on a socket the input has no end.
func TestSpec7_3_MaxValueBytes(t *testing.T) {
	t.Parallel()

	t.Run("a long flat list is refused", func(t *testing.T) {
		t.Parallel()
		var got []int
		err := decodeWith(t, tinyDecoder("l"+repeat("i0e", 500)+"e"), &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrExceedsMax)
	})

	t.Run("a wide flat dict is refused", func(t *testing.T) {
		t.Parallel()
		var sb strings.Builder
		sb.WriteString("d")
		for i := range 200 {
			fmt.Fprintf(&sb, "4:k%03d1:v", i)
		}
		sb.WriteString("e")

		var got map[string]string
		err := decodeWith(t, tinyDecoder(sb.String()), &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrExceedsMax)
	})

	t.Run("the decoder stops reading at the limit", func(t *testing.T) {
		t.Parallel()
		r := &countingReader{src: strings.NewReader("l" + repeat("i0e", 5000) + "e")}
		d := NewDecoder(r)
		d.Limits = tinyLimits()

		var got []int
		require.Error(t, decodeWith(t, d, &got))
		assert.Less(t, r.read, 4096,
			"read %d bytes past a 256-byte value limit", r.read)
	})

	// SPEC §7.2 rule 3 again: skipping is not a way around a limit.
	t.Run("the limit applies while skipping", func(t *testing.T) {
		t.Parallel()
		input := "d9:a_unknownl" + repeat("i0e", 500) + "e1:s5:helloe"

		var got simple
		err := decodeWith(t, tinyDecoder(input), &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrExceedsMax)
	})
}

// SPEC §7.2 rule 4: a peer that sends one unknown key holding a large value
// must not make you allocate all of it for nothing.
func TestSpec7_2_SkipDoesNotAllocateTheSkippedValue(t *testing.T) {
	// Deliberately NOT parallel: allocatedBytes reads process-wide counters.
	// `go test` runs non-parallel tests to completion before resuming parallel
	// ones, which is the only thing keeping this measurement honest. Adding
	// t.Parallel() here silently turns it into noise.

	const size = 4 << 20
	input := "d9:a_unknown" + fmt.Sprintf("%d:%s", size, repeat("x", size)) + "1:s5:helloe"

	// Default limits: the skipped string is 4 MiB, under the 8 MiB default,
	// so it is legal — the point is that legality does not mean materialising
	// it.
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

	inputs := []string{
		"", "x", "e", "i1e", "i-1e", "4:spam", "0:", "le", "de",
		"l4:spame", "li1ee", "d1:s4:spame", "d1:si1ee", "di1ei2ee",
		"d1:11:ae", "l" + repeat("i1e", 5) + "e", "d0:0:e",
		nest(64), "l" + repeat("i0e", 500) + "e", "100:" + repeat("x", 100),
	}

	for _, dst := range everyDestinationShape {
		for _, in := range inputs {
			t.Run(dst.name+"/"+fmt.Sprintf("%q", truncateName(in)), func(t *testing.T) {
				t.Parallel()
				// decode() turns any panic into a hard failure. The returned
				// error is irrelevant here — only the absence of a panic is.
				_ = decodeWith(t, tinyDecoder(in), dst.new())
			})
		}
	}
}

// ---------------------------------------------------------------------------
// SPEC §10.1 — concurrency
// ---------------------------------------------------------------------------

// The field map cache (§3.3.2) is shared across every decoder in the process,
// so the claim that it is safe without locking needs a standing check. Run
// under -race; without it this test proves almost nothing.
//
// The fixture type is declared inside the test so the cache entry is cold when
// the goroutines start: a warm entry means every goroutine takes the read path
// and the publish race is never exercised.
func TestSpec10_1_FieldCacheIsConcurrencySafe(t *testing.T) {
	t.Parallel()

	type coldType struct {
		A string            `bencode:"a"`
		B int64             `bencode:"b"`
		C []string          `bencode:"c"`
		D map[string]string `bencode:"d"`
	}

	input := mustEncode(t, bencodeast.Dict{
		"a": bencodeast.Str("x"),
		"b": bencodeast.Int(1),
		"c": bencodeast.List{bencodeast.Str("y")},
		"d": bencodeast.Dict{"k": bencodeast.Str("v")},
	})

	const goroutines = 64

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)

	start := make(chan struct{})
	for range goroutines {
		wg.Go(func() {
			<-start // maximise the overlap on the cold cache entry

			var got coldType
			if err := NewDecoder(strings.NewReader(input)).Decode(&got); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			if got.A != "x" || got.B != 1 {
				mu.Lock()
				errs = append(errs, fmt.Errorf("bad decode: %+v", got))
				mu.Unlock()
			}
		})
	}
	close(start)
	wg.Wait()

	assert.Empty(t, errs)
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
// The tables above assert what the spec says. Fuzzing asserts what a table can
// never cover: that no input panics (SPEC §8), that decoding is
// self-consistent, and that error offsets stay inside the input.
// ---------------------------------------------------------------------------

var fuzzSeeds = []string{
	"i0e", "i-1e", "i42e", "0:", "4:spam", "le", "de",
	"l4:spami1ee", "d3:cow3:mooe", "d1:ald1:bi1eeee",
	"d5:filesld6:lengthi1e4:pathl1:aeeee",
	"x", "i", "iabce", "4spam", "10:abc", "l", "d3:foo", "di1ei2ee",
	"i03e", "i-0e", "05:hello", nest(20), "l" + repeat("i0e", 300) + "e",
}

// FuzzDecodeAny checks that anything this decoder accepts survives a round
// trip, and that anything it rejects reports a position inside the input.
//
// The oracle is re-encode-and-redecode rather than a differential comparison
// against bencode_ast: that decoder has its own limits and its own empty-key
// handling, so disagreement between them would prove nothing.
func FuzzDecodeAny(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, in []byte) {
		var first any
		if err := NewDecoder(bytes.NewReader(in)).Decode(&first); err != nil {
			// SPEC §5.2: an offset outside the input means the consumed
			// counter has drifted. That counter is also what RawMessage
			// capture (§9.4) and MaxValueBytes (§7.3) are built on, so this
			// one assertion guards three mechanisms.
			assertOffsetInBounds(t, err, int64(len(in)))
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

// FuzzDecodeIntoEveryDestination is the standing test for SPEC §8. The §8
// table pairs 21 destinations with 20 hand-written inputs; this pairs the same
// destinations with inputs nobody thought of. A panic here is the failure —
// the returned error is irrelevant.
func FuzzDecodeIntoEveryDestination(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, in []byte) {
		for _, dst := range everyDestinationShape {
			d := NewDecoder(bytes.NewReader(in))
			d.Limits = tinyLimits()

			err := d.Decode(dst.new())
			if err != nil {
				assertOffsetInBounds(t, err, int64(len(in)))
			}
		}
	})
}

// assertOffsetInBounds checks that whichever structured error came back
// reports a position within the input that produced it.
func assertOffsetInBounds(t *testing.T, err error, n int64) {
	t.Helper()

	var offset int64
	var se *SyntaxError
	var te *TypeError
	var le *LimitError

	switch {
	case errors.As(err, &se):
		offset = se.Offset
	case errors.As(err, &te):
		offset = te.Offset
	case errors.As(err, &le):
		offset = le.Offset
	default:
		return // ErrInvalidDestination and bare io.EOF carry no position
	}

	require.GreaterOrEqual(t, offset, int64(0), "negative offset in %v", err)
	require.LessOrEqual(t, offset, n, "offset past the end of a %d byte input in %v", n, err)
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
