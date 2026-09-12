package bencode

import (
	"io"
	"strings"
	"testing"

	bencodeast "github.com/arumandesu/bencode/pkg/bencode_ast"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

	// SPEC §3.3.2 rule 3: an ambiguous key binds to nothing, the way
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

	// SPEC §3.3.2 rule 2: the one same-depth collision that does resolve. Both
	// orderings must give the same answer.
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
	// untagged claimant and cannot win §3.3.2 rule 2.
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

// SPEC §3.3.1: an untagged embedded struct is not a destination — its fields
// are promoted into the parent's key space, because that is what embedding
// means in Go and a key space that disagrees with the selector space makes
// shared field sets useless.
//
// The failure modes here are asymmetric, which is why this is a table of
// shapes rather than one happy-path test. Lifting too little is quiet: a
// promoted field stays zero and the key looks unknown. Lifting too much is
// loud: a read-only reflect.Value reaches a Set and the decoder panics (§8),
// which §3.1's settability invariant exists to prevent. The line between the
// two is reflect's own — an unexported EMBEDDED field's read-only marker is
// not inherited by its fields, an unexported NAMED field's is — so both sides
// of it get a test.
func TestSpec3_3_1_EmbeddedFields(t *testing.T) {
	t.Parallel()

	t.Run("an untagged embedded struct is lifted", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{
			"s":     bencodeast.Str("promoted"),
			"extra": bencodeast.Str("outer"),
		})

		var got liftsExported
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, "promoted", got.Exported.S, `"s" must reach the promoted field`)
		assert.Equal(t, "outer", got.Extra)
	})

	// The draft 2 → 3 breaking change, pinned from the losing side: under draft
	// 2 this key bound the embedded struct itself.
	t.Run("a lifted embed has no key of its own", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{
			"Exported": bencodeast.Dict{"s": bencodeast.Str("must not bind")},
			"extra":    bencodeast.Str("kept"),
		})

		var got liftsExported
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, "", got.Exported.S, "the type name of a lifted embed is not a key")
		assert.Equal(t, "kept", got.Extra, "the decoder must stay in sync while skipping it")
	})

	t.Run("a tagged embed is bound as an ordinary field, not lifted", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{
			"emb":   bencodeast.Dict{"s": bencodeast.Str("inner")},
			"extra": bencodeast.Str("outer"),
			"s":     bencodeast.Str("must not leak into the embedded field"),
		})

		var got embedsExported
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, "inner", got.Exported.S, "the tag names the field itself")
		assert.Equal(t, "outer", got.Extra)
	})

	t.Run("a dash-tagged embed lifts nothing", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{
			"s":        bencodeast.Str("must not bind"),
			"Exported": bencodeast.Dict{"s": bencodeast.Str("must not bind either")},
			"extra":    bencodeast.Str("kept"),
		})

		var got skipsEmbedded
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, "", got.Exported.S, `"-" skips the field AND its contents`)
		assert.Equal(t, "kept", got.Extra)
	})

	t.Run("lifting is transitive", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{
			"d":     bencodeast.Str("two levels up"),
			"extra": bencodeast.Str("outer"),
		})

		var got liftsTransitively
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, "two levels up", got.Mid.Deep.D)
		assert.Equal(t, "outer", got.Extra)
	})

	// An embedded field's name is its type's name, so these are unexported
	// fields. The struct is walked through; the pointer is pruned; the
	// non-struct kinds stay ordinary fields that rule 1 then skips.
	t.Run("unexported embeds", func(t *testing.T) {
		t.Parallel()

		t.Run("a struct is walked through to its exported fields", func(t *testing.T) {
			t.Parallel()
			input := mustEncode(t, bencodeast.Dict{
				"s":     bencodeast.Str("promoted"),
				"n":     bencodeast.Int(7),
				"extra": bencodeast.Str("outer"),
			})

			var got liftsUnexported
			require.NoError(t, decode(t, input, &got))
			assert.Equal(t, "promoted", got.hidden.S,
				"reflect does not propagate an embedded field's read-only marker to its fields")
			assert.Equal(t, int64(7), got.hidden.N)
			assert.Equal(t, "outer", got.Extra)
		})

		t.Run("the embed itself is still not a key", func(t *testing.T) {
			t.Parallel()
			input := mustEncode(t, bencodeast.Dict{
				"hidden": bencodeast.Dict{"s": bencodeast.Str("must not bind")},
				"extra":  bencodeast.Str("kept"),
			})

			var got liftsUnexported
			require.NoError(t, decode(t, input, &got))
			assert.Equal(t, hidden{}, got.hidden)
			assert.Equal(t, "kept", got.Extra)
		})

		// The prune is the alternative to a panic, not to an error: allocating
		// this pointer means Set on a read-only value. §3.3.1 makes it a
		// property of the type, so the keys under it simply go unbound.
		t.Run("a pointer to an unexported struct is pruned, not panicked on", func(t *testing.T) {
			t.Parallel()
			input := mustEncode(t, bencodeast.Dict{
				"s":     bencodeast.Str("unreachable"),
				"n":     bencodeast.Int(1),
				"extra": bencodeast.Str("kept"),
			})

			var got liftsUnexportedPtr
			require.NoError(t, decode(t, input, &got))
			assert.Nil(t, got.hidden, "the decoder must not allocate a read-only pointer")
			assert.Equal(t, "kept", got.Extra, "the rest of the dict still decodes")
		})

		t.Run("a tagged unexported embed is invisible", func(t *testing.T) {
			t.Parallel()
			input := mustEncode(t, bencodeast.Dict{
				"h":     bencodeast.Dict{"s": bencodeast.Str("must not bind")},
				"s":     bencodeast.Str("must not bind either"),
				"extra": bencodeast.Str("kept"),
			})

			var got taggedUnexportedEmbed
			require.NoError(t, decode(t, input, &got))
			assert.Equal(t, hidden{}, got.hidden,
				"a tag asks for a binding rule 1 forbids; it is not a request to lift")
			assert.Equal(t, "kept", got.Extra)
		})

		// Map and slice embeds are not struct-kinded, so they are ordinary
		// unexported fields and skipped. Kept apart because the failure mode
		// differs: these panic on the Set at the end of their branch.
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

	t.Run("an embedded non-struct type stays an ordinary field", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{
			"MyInt": bencodeast.Int(5),
			"extra": bencodeast.Str("outer"),
		})

		var got embedsNamedInt
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, MyInt(5), got.MyInt, "there is nothing to lift out of a named int")
		assert.Equal(t, "outer", got.Extra)
	})

	// SPEC §3.2: an embedded pointer is the one pointer that becomes non-nil
	// without a key naming it — and only when a key underneath it arrives.
	t.Run("embedded pointers", func(t *testing.T) {
		t.Parallel()

		t.Run("allocated when a promoted key arrives", func(t *testing.T) {
			t.Parallel()
			input := mustEncode(t, bencodeast.Dict{
				"s":     bencodeast.Str("through the pointer"),
				"extra": bencodeast.Str("outer"),
			})

			var got liftsExportedPtr
			require.NoError(t, decode(t, input, &got))
			require.NotNil(t, got.Exported)
			assert.Equal(t, "through the pointer", got.Exported.S)
			assert.Equal(t, "outer", got.Extra)
		})

		t.Run("left nil when no promoted key arrives", func(t *testing.T) {
			t.Parallel()
			input := mustEncode(t, bencodeast.Dict{"extra": bencodeast.Str("outer")})

			var got liftsExportedPtr
			require.NoError(t, decode(t, input, &got))
			assert.Nil(t, got.Exported,
				"allocating here would report a key the input never carried")
		})

		// SPEC §3.4: reuse reaches through the embed. A replaced pointer would
		// silently discard fields the input did not mention.
		t.Run("a non-nil embedded pointer is reused, not replaced", func(t *testing.T) {
			t.Parallel()
			input := mustEncode(t, bencodeast.Dict{"extra": bencodeast.Str("new")})

			existing := &Exported{S: "old"}
			got := liftsExportedPtr{Exported: existing, Extra: "stale"}
			require.NoError(t, decode(t, input, &got))

			assert.Same(t, existing, got.Exported, "the embedded pointer must not be reallocated")
			assert.Equal(t, "old", got.Exported.S, "a key absent from the input changes nothing")
			assert.Equal(t, "new", got.Extra)
		})
	})

	// A recursive type makes the field-tree walk infinite unless it stops
	// revisiting a type. If the field map hangs or blows the stack, this test
	// never reports — which is the point: it must be caught here and not by a
	// caller whose torrent parser stopped responding.
	t.Run("a recursive embed terminates and resolves to depth 0", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{"v": bencodeast.Str("outer")})

		var got Recursive
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, "outer", got.V)
		assert.Nil(t, got.Recursive, "no key bound below the embed, so nothing is allocated")
	})
}

// SPEC §3.3.2 rule 1 — shallowest wins. This is the rule draft 2 could not
// have, and the rule that makes embedding usable: an outer field shadows a
// promoted one exactly as `t.S` does in Go.
func TestSpec3_3_2_CompetingAcrossDepths(t *testing.T) {
	t.Parallel()

	t.Run("a depth 0 field shadows a promoted one", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{"s": bencodeast.Str("outer wins")})

		var got shadowsEmbedded
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, "outer wins", got.S)
		assert.Equal(t, "", got.Exported.S, "the shadowed field must not be written too")
	})

	t.Run("two embeds claiming one key at the same depth bind to neither", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{"name": bencodeast.Str("v")})

		var got ambiguousEmbeds
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, ambiguousEmbeds{}, got,
			"Go makes the same selector a compile error; the decoder binds nothing")
	})

	t.Run("one tag breaks a same-depth tie between embeds", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{"Name": bencodeast.Str("v")})

		var got embedTieBrokenByTag
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, "v", got.TaggedName.Other, "exactly one tagged candidate wins")
		assert.Equal(t, "", got.PlainName.Name)
	})

	// The diamond is the shape where a depth-first walk would quietly pick a
	// winner: whichever branch it reached first. Breadth-first plus rule 3
	// makes it ambiguous regardless of declaration order.
	t.Run("a diamond is ambiguous at depth 2", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.Dict{
			"s":     bencodeast.Str("v"),
			"extra": bencodeast.Str("kept"),
		})

		var got diamond
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, "", got.LeftDiamond.Exported.S)
		assert.Equal(t, "", got.RightDiamond.Exported.S)
		assert.Equal(t, "kept", got.Extra, "an ambiguous key is skipped, not fatal")
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

	// SPEC §3.4: an array is replaced wholesale. §4 requires an exact length
	// match, so every element is overwritten and no stale tail can survive —
	// there is no partial-fill case left for reuse to expose.
	t.Run("array is replaced wholesale on reuse", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{
			bencodeast.Str("new1"), bencodeast.Str("new2"), bencodeast.Str("new3"),
		})

		got := [3]string{"old1", "old2", "old3"}
		require.NoError(t, decode(t, input, &got))
		assert.Equal(t, [3]string{"new1", "new2", "new3"}, got)
	})

	// A reused array that fails the length check must not be left holding a
	// mix of new and stale elements — the decode is all or nothing.
	t.Run("a rejected list leaves a reused array untouched", func(t *testing.T) {
		t.Parallel()
		input := mustEncode(t, bencodeast.List{bencodeast.Str("new1")})

		got := [3]string{"old1", "old2", "old3"}
		err := decode(t, input, &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrArrayLength)
		assert.Equal(t, [3]string{"old1", "old2", "old3"}, got)
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

	// SPEC §4.1: an array destination is strict because it asserts a shape.
	// For a string it is almost always a fixed-width identifier, and a
	// partially filled SHA-1 is not a degraded hash — it is a different hash
	// that compares unequal far from its cause.
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

	// SPEC §4 + §4.1: a list into an array is strict in both directions, the
	// same as a string into [N]byte. A caller who wants "at most N" has []T
	// and can check len; reaching for [N]T asserts a shape, and a mismatch
	// means the input is not what the caller thinks it is.
	t.Run("length must equal the array exactly", func(t *testing.T) {
		t.Parallel()

		t.Run("shorter must not partially fill", func(t *testing.T) {
			t.Parallel()
			input := mustEncode(t, bencodeast.List{bencodeast.Str("a"), bencodeast.Str("b")})

			var got [3]string
			err := decode(t, input, &got)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrArrayLength, "the type matched; only the length did not")
			assert.ErrorIs(t, err, ErrTypeMismatch, "must still classify as a mismatch")
			assert.Equal(t, [3]string{}, got, "a rejected value must not have been written")
		})

		t.Run("longer is rejected too", func(t *testing.T) {
			t.Parallel()
			input := mustEncode(t, bencodeast.List{
				bencodeast.Str("a"), bencodeast.Str("b"), bencodeast.Str("c"),
			})

			var got [2]string
			err := decode(t, input, &got)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrArrayLength)
			assert.ErrorIs(t, err, ErrTypeMismatch)
			assert.Equal(t, [2]string{}, got, "a rejected value must not have been written")
		})

		// The old contract discarded surplus and required it to be consumed
		// so the next Decode saw a clean stream. Rejecting makes that moot:
		// §6 poisons the decoder, so there is no next value to protect.
		t.Run("a surplus rejection poisons the decoder", func(t *testing.T) {
			t.Parallel()
			d := NewDecoder(strings.NewReader("l1:a1:b1:cei99e"))

			var arr [1]string
			require.Error(t, decodeWith(t, d, &arr))

			var after int
			assert.Error(t, decodeWith(t, d, &after),
				"the decoder is poisoned; the trailing integer must not be readable")
		})
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
			// SPEC §4 + §9.1: capture never type-mismatches, for any input.
			{"RawMessage", func() any { return new(RawMessage) }, nil},
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
			{"RawMessage", func() any { return new(RawMessage) }, nil},
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
			{"[1]string", func() any { return new([1]string) }, nil},
			// aList holds one element, so the length check rejects this.
			{"[2]string", func() any { return new([2]string) }, ErrArrayLength},
			{"any", func() any { return new(any) }, nil},
			{"*[]string", func() any { return new(*[]string) }, nil},
			{"RawMessage", func() any { return new(RawMessage) }, nil},
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
			{"RawMessage", func() any { return new(RawMessage) }, nil},
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
