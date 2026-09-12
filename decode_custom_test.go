package bencode

import (
	"errors"
	"io"
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// SPEC §10 — custom unmarshaling
//
// The failure mode that matters here is not an error, it is a method that is
// never called. A destination whose UnmarshalBencode is missed falls through
// to §4 and produces an ordinary-looking ErrTypeMismatch, or worse decodes
// successfully into the wrong shape — so these tests assert that the method
// *ran*, and on what bytes, rather than that the decode returned nil.
//
// The second failure mode is a panic. Probing for one interface and casting
// the result into the other takes down Decode for every type that implements
// exactly one of them, which is every type in §10.1's motivating list. The
// decode helpers convert a panic into a test failure (SPEC §8), so every test
// in this file carries that assertion whether or not it mentions it.
// ---------------------------------------------------------------------------

var errCustom = errors.New("custom unmarshaler said no")

// bencValue implements Unmarshaler and nothing else — the half of §10.2's
// "exactly one interface" rule that panicked when the probe cast an
// encoding.TextUnmarshaler into an Unmarshaler.
//
// It copies the bytes it is handed, which is what §10.3 requires of every
// implementation: the slice is the decoder's buffer, on loan for the length of
// the call.
type bencValue struct{ raw string }

func (b *bencValue) UnmarshalBencode(p []byte) error { b.raw = string(p); return nil }

// textValue implements encoding.TextUnmarshaler and nothing else — the other
// half, and the shape every stdlib type worth binding has (net.IP,
// netip.Addr, time.Time).
type textValue struct{ text string }

func (t *textValue) UnmarshalText(p []byte) error { t.text = string(p); return nil }

// bothValue implements both and records which method ran, which is the only
// way to observe §10.2's precedence: on a bencode string the two interfaces
// are both applicable and disagree about the bytes.
type bothValue struct{ via, got string }

func (b *bothValue) UnmarshalBencode(p []byte) error {
	b.via, b.got = "bencode", string(p)
	return nil
}

func (b *bothValue) UnmarshalText(p []byte) error {
	b.via, b.got = "text", string(p)
	return nil
}

// errValueReceiver reports that a value-receiver method was reached. A value
// receiver cannot mutate the destination, so returning is the only evidence it
// can leave; §10.2 promises it is still found.
var errValueReceiver = errors.New("value receiver ran")

type valueRecvText struct{ _ byte }

func (valueRecvText) UnmarshalText([]byte) error { return errValueReceiver }

type failBenc struct{ _ byte }

func (*failBenc) UnmarshalBencode([]byte) error { return errCustom }

type failText struct{ _ byte }

func (*failText) UnmarshalText([]byte) error { return errCustom }

// textKey is a map key type that is not string-kinded and not convertible from
// one. Before §10.4 the decoder converted key bytes into the key type, which
// panics for this shape and silently discarded the result for every other.
type textKey struct{ name string }

func (k *textKey) UnmarshalText(p []byte) error { k.name = string(p); return nil }

type failTextKey struct{}

func (k *failTextKey) UnmarshalText([]byte) error { return errCustom }

// bencKey implements only Unmarshaler, which §10.4 does not consult for keys.
// It must be rejected as an unusable key type, not called and not panicked on.
type bencKey struct{ name string }

func (k *bencKey) UnmarshalBencode(p []byte) error { k.name = string(p); return nil }

// stringKey is the §4 case §10.4 must not regress: a named string type still
// binds by conversion, with no interface in sight.
type stringKey string

// textString is a TextUnmarshaler whose underlying kind is a string rather
// than a struct, which is what decides its fate on a non-string value (§10.1).
type textString string

func (t *textString) UnmarshalText(p []byte) error { *t = textString(p); return nil }

// ---------------------------------------------------------------------------
// §10.1 what each interface receives
// ---------------------------------------------------------------------------

// SPEC §10.1: Unmarshaler receives the verbatim bytes of the complete value,
// for every bencode type — the same bytes §9.1 hands a RawMessage, which is
// why this reuses that section's fixtures. Capture runs on §9.3's machinery,
// so the §9.4 clock split applies here too and every case is run chunked.
func TestSpec10_1_UnmarshalerReceivesTheWholeValue(t *testing.T) {
	t.Parallel()

	for _, c := range captureCases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			eachChunking(t, c.in, func(t *testing.T, r io.Reader) {
				var got bencValue
				require.NoError(t, decodeWith(t, NewDecoder(r), &got))
				assert.Equal(t, c.in, got.raw,
					"UnmarshalBencode must receive the whole value, prefixes and terminator included")
			})
		})
	}
}

// SPEC §10.1: TextUnmarshaler receives the string's content with the length
// prefix stripped. That is the whole distinction between the two interfaces —
// content versus value — so it is asserted against the same input that §10.1
// tabulates for Unmarshaler.
func TestSpec10_1_TextUnmarshalerReceivesTheContent(t *testing.T) {
	t.Parallel()

	cases := []struct{ name, in, want string }{
		{"string", "6:string", "string"},
		{"empty string", "0:", ""},
		// SPEC §2: bencode strings are arbitrary byte strings. The interface is
		// named for text and this decoder feeds it bytes; nothing is validated,
		// replaced or truncated on the way.
		{"NUL and invalid UTF-8", "3:\x00\xff\n", "\x00\xff\n"},
		{"length prefix is not content", "2:12", "12"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var got textValue
			require.NoError(t, decode(t, c.in, &got))
			assert.Equal(t, c.want, got.text)
		})
	}
}

// SPEC §10.1: a TextUnmarshaler destination facing a non-string is not special
// cased — §4 applies unchanged. The alternative, improvising text out of an
// integer or a list, would make the contract meaningless.
func TestSpec10_1_TextUnmarshalerOnlyBindsStrings(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"i42e", "i0e", "le", "li4ee"} {
		t.Run(truncateName(in), func(t *testing.T) {
			t.Parallel()
			var got textValue
			err := decode(t, in, &got)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrTypeMismatch)
			assert.Empty(t, got.text, "UnmarshalText must not be called at all")
		})
	}

	// A named string type takes the §4 string row, so a dict is rejected there
	// for reasons that have nothing to do with §10 — which is what makes the
	// struct case below a property of the destination's kind and not of the
	// interface.
	t.Run("dict, named string kind", func(t *testing.T) {
		t.Parallel()
		var got textString
		err := decode(t, "d1:ai1ee", &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrTypeMismatch)
	})
}

// SPEC §12, open: a dict into a struct-kinded TextUnmarshaler binds by §3.3,
// matches no exported field and returns nil, leaving the destination zero.
// netip.Addr, time.Time and big.Int are all structs, so this is every stdlib
// type the feature exists to serve.
//
// Skipped rather than inverted to assert what the code does today: a silent
// success on input the type cannot represent is not behaviour worth locking
// in, and a test that describes it would never fail for a reason worth
// knowing. Unskip when §12 is decided.
func TestSpec10_1_DictIntoTextUnmarshalerIsRejected(t *testing.T) {
	t.Parallel()
	t.Skip("SPEC §12 open decision: dict into a struct-kinded TextUnmarshaler")

	for _, in := range []string{"de", "d1:ai1ee"} {
		t.Run(truncateName(in), func(t *testing.T) {
			t.Parallel()

			var got textValue
			err := decode(t, in, &got)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrTypeMismatch)

			var addr netip.Addr
			err = decode(t, in, &addr)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrTypeMismatch)
		})
	}
}

// ---------------------------------------------------------------------------
// §10.2 precedence and lookup
// ---------------------------------------------------------------------------

// SPEC §10.2: Unmarshaler outranks TextUnmarshaler. The two only compete on a
// bencode string, where they disagree about the bytes — everywhere else only
// one is applicable, so the string case is the whole test.
func TestSpec10_2_UnmarshalerOutranksTextUnmarshaler(t *testing.T) {
	t.Parallel()

	t.Run("on a string, where both apply", func(t *testing.T) {
		t.Parallel()
		var got bothValue
		require.NoError(t, decode(t, "4:spam", &got))
		assert.Equal(t, "bencode", got.via)
		assert.Equal(t, "4:spam", got.got, "the whole value, not the content")
	})

	for _, in := range []string{"i42e", "li4ee", "d1:ai1ee"} {
		t.Run("on "+truncateName(in), func(t *testing.T) {
			t.Parallel()
			var got bothValue
			require.NoError(t, decode(t, in, &got))
			assert.Equal(t, "bencode", got.via)
			assert.Equal(t, in, got.got)
		})
	}
}

// SPEC §10.2: lookup uses the addressable form of the destination, so a
// pointer receiver is found. This is the failure that is silent — a missed
// method falls through to §4 and returns an ordinary ErrTypeMismatch — and it
// has to hold in every position the decoder can put a value in, because each
// one reaches the probe through a different reflect.Value.
func TestSpec10_2_PointerReceiverIsFoundInEveryPosition(t *testing.T) {
	t.Parallel()

	t.Run("top-level destination", func(t *testing.T) {
		t.Parallel()
		var b bencValue
		require.NoError(t, decode(t, "i42e", &b))
		assert.Equal(t, "i42e", b.raw)

		var x textValue
		require.NoError(t, decode(t, "4:spam", &x))
		assert.Equal(t, "spam", x.text)
	})

	t.Run("struct fields, plain and pointer", func(t *testing.T) {
		t.Parallel()
		type holder struct {
			B  bencValue  `bencode:"b"`
			PB *bencValue `bencode:"pb"`
			X  textValue  `bencode:"x"`
			PX *textValue `bencode:"px"`
		}

		var got holder
		require.NoError(t, decode(t, "d1:bi1e2:pbli2ee1:x4:spam2:px3:egge", &got))
		assert.Equal(t, "i1e", got.B.raw)
		require.NotNil(t, got.PB)
		assert.Equal(t, "li2ee", got.PB.raw)
		assert.Equal(t, "spam", got.X.text)
		require.NotNil(t, got.PX)
		assert.Equal(t, "egg", got.PX.text)
	})

	t.Run("slice and array elements", func(t *testing.T) {
		t.Parallel()
		var bs []bencValue
		require.NoError(t, decode(t, "li1ei2ee", &bs))
		require.Len(t, bs, 2)
		assert.Equal(t, "i1e", bs[0].raw)
		assert.Equal(t, "i2e", bs[1].raw)

		var xs [2]textValue
		require.NoError(t, decode(t, "l4:spam3:egge", &xs))
		assert.Equal(t, "spam", xs[0].text)
		assert.Equal(t, "egg", xs[1].text)
	})

	t.Run("map values", func(t *testing.T) {
		t.Parallel()
		var m map[string]bencValue
		require.NoError(t, decode(t, "d1:ai1e1:bl3:fooee", &m))
		assert.Equal(t, "i1e", m["a"].raw)
		assert.Equal(t, "l3:fooe", m["b"].raw)
	})

	t.Run("through a pointer chain", func(t *testing.T) {
		t.Parallel()
		var pp **bencValue
		require.NoError(t, decode(t, "i7e", &pp))
		require.NotNil(t, pp)
		require.NotNil(t, *pp)
		assert.Equal(t, "i7e", (*pp).raw)
	})

	t.Run("nested inside a struct reached by embedding", func(t *testing.T) {
		t.Parallel()
		type inner struct {
			B bencValue `bencode:"b"`
		}
		type outer struct {
			inner
			S string `bencode:"s"`
		}

		var got outer
		require.NoError(t, decode(t, "d1:bi9e1:s4:spame", &got))
		assert.Equal(t, "i9e", got.B.raw)
		assert.Equal(t, "spam", got.S)
	})
}

// SPEC §10.2: the probe asserts to the interface the call site asked for. A
// type implementing exactly one of the two must decode; casting the result of
// one probe into the other panics out of Decode for precisely these types, and
// the decode helpers fail the test on a panic (SPEC §8).
func TestSpec10_2_ImplementingExactlyOneInterfaceDoesNotPanic(t *testing.T) {
	t.Parallel()

	t.Run("Unmarshaler only", func(t *testing.T) {
		t.Parallel()
		var got bencValue
		require.NoError(t, decode(t, "4:spam", &got))
		assert.Equal(t, "4:spam", got.raw)
	})

	t.Run("TextUnmarshaler only", func(t *testing.T) {
		t.Parallel()
		var got textValue
		require.NoError(t, decode(t, "4:spam", &got))
		assert.Equal(t, "spam", got.text)
	})

	// The stdlib types §10.1 names as the reason TextUnmarshaler is kept. If
	// these do not work the feature has no users.
	t.Run("netip.Addr", func(t *testing.T) {
		t.Parallel()
		var got netip.Addr
		require.NoError(t, decode(t, "7:1.2.3.4", &got))
		assert.Equal(t, "1.2.3.4", got.String())
	})

	t.Run("TextUnmarshaler only, as a map key", func(t *testing.T) {
		t.Parallel()
		var got map[netip.Addr]int
		require.NoError(t, decode(t, "d7:1.2.3.4i1ee", &got))
		assert.Equal(t, map[netip.Addr]int{netip.MustParseAddr("1.2.3.4"): 1}, got)
	})

	t.Run("Unmarshaler only, as a map key", func(t *testing.T) {
		t.Parallel()
		// §10.4 does not consult Unmarshaler for keys, so this is an unusable
		// key type. The point is that it is *rejected*, not that it panics on
		// the way to being rejected.
		var got map[bencKey]int
		err := decode(t, "d1:ai1ee", &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrTypeMismatch)
	})
}

// SPEC §10.2: a value receiver is found either way. It cannot mutate the
// destination, so the returned error is the only evidence available.
func TestSpec10_2_ValueReceiverIsFound(t *testing.T) {
	t.Parallel()

	var got valueRecvText
	err := decode(t, "4:spam", &got)
	require.Error(t, err)
	assert.ErrorIs(t, err, errValueReceiver)
}

// ---------------------------------------------------------------------------
// §10.3 capture
// ---------------------------------------------------------------------------

// SPEC §10.3: an Unmarshaler value is captured on §9.3's machinery, so
// MaxCaptureBytes bounds it exactly as it bounds a RawMessage, and recording
// is switched off again afterwards (§9.6).
func TestSpec10_3_CaptureLimitsApplyToUnmarshaler(t *testing.T) {
	t.Parallel()

	// Over tinyLimits().MaxCaptureBytes (64) and under MaxValueBytes (256), so
	// only the capture limit is in question.
	bigValue := "l" + repeat("i0e", 40) + "e"

	t.Run("a value over the limit is rejected", func(t *testing.T) {
		t.Parallel()
		var got bencValue
		err := decodeWith(t, tinyDecoder(bigValue), &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrExceedsMax)

		var limitErr *LimitError
		require.ErrorAs(t, err, &limitErr)
		assert.Equal(t, "MaxCaptureBytes", limitErr.Limit)
		assertOffsetInBounds(t, err, int64(len(bigValue)))
	})

	t.Run("recording is off again after the call", func(t *testing.T) {
		t.Parallel()
		// The Unmarshaler field is tiny; the junk sibling is not. Leaving the
		// recorder on past the call would charge those bytes to a window that
		// is no longer open and reject a legal input.
		type holder struct {
			B    bencValue `bencode:"b"`
			Junk []int64   `bencode:"junk"`
		}

		var got holder
		require.NoError(t, decodeWith(t, tinyDecoder("d1:bi7e4:junk"+bigValue+"e"), &got))
		assert.Equal(t, "i7e", got.B.raw)
		assert.Len(t, got.Junk, 40)
	})

	t.Run("a large value skipped before the call is not charged", func(t *testing.T) {
		t.Parallel()
		type holder struct {
			Junk []int64   `bencode:"junk"`
			B    bencValue `bencode:"b"`
		}

		var got holder
		require.NoError(t, decodeWith(t, tinyDecoder("d4:junk"+bigValue+"1:bi7ee"), &got))
		assert.Equal(t, "i7e", got.B.raw)
	})
}

// ---------------------------------------------------------------------------
// §10.4 map keys
// ---------------------------------------------------------------------------

// SPEC §10.4: map[K]T is accepted when *K implements encoding.TextUnmarshaler,
// whatever K's kind. Each key is allocated so the method has an addressable
// receiver — converting the key bytes instead leaves a pointer receiver
// invisible and a value receiver mutating a copy.
func TestSpec10_4_TextUnmarshalerMapKeys(t *testing.T) {
	t.Parallel()

	t.Run("a non-string-kinded key type", func(t *testing.T) {
		t.Parallel()
		var got map[textKey]int64
		require.NoError(t, decode(t, "d1:ai1e3:fooi2ee", &got))
		assert.Equal(t, map[textKey]int64{{name: "a"}: 1, {name: "foo"}: 2}, got)
	})

	t.Run("every key is unmarshaled independently", func(t *testing.T) {
		t.Parallel()
		// A key allocated once and reused would collapse these into one entry,
		// or leave every entry holding the last key decoded.
		var got map[textKey]int64
		require.NoError(t, decode(t, "d1:ai1e1:bi2e1:ci3ee", &got))
		assert.Len(t, got, 3)
		assert.Equal(t, int64(1), got[textKey{name: "a"}])
		assert.Equal(t, int64(3), got[textKey{name: "c"}])
	})

	t.Run("keys are bytes, not text", func(t *testing.T) {
		t.Parallel()
		var got map[textKey]int64
		require.NoError(t, decode(t, "d3:\x00\xff\ni1ee", &got))
		assert.Equal(t, map[textKey]int64{{name: "\x00\xff\n"}: 1}, got)
	})

	t.Run("string-kinded keys still bind by conversion", func(t *testing.T) {
		t.Parallel()
		// SPEC §4: no interface involved. §10.4 must not have displaced this.
		var got map[stringKey]int64
		require.NoError(t, decode(t, "d1:ai1ee", &got))
		assert.Equal(t, map[stringKey]int64{"a": 1}, got)
	})

	t.Run("an unusable key type is rejected before the map is allocated", func(t *testing.T) {
		t.Parallel()
		// SPEC §4 and §8: the check is on the map type, so it fires before any
		// entry is read and cannot become a panic on the first SetMapIndex.
		got := map[int]int64{99: 99}
		err := decode(t, "d1:ai1ee", &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrTypeMismatch)
		assert.Equal(t, map[int]int64{99: 99}, got,
			"the destination must not be replaced by a half-built map")
	})
}

// ---------------------------------------------------------------------------
// §10.5 errors
// ---------------------------------------------------------------------------

// SPEC §10.5: an error from either method is wrapped in a *TypeError carrying
// the offset and the destination type, with the returned error as the cause.
// It deliberately does NOT wrap ErrTypeMismatch — the failure belongs to the
// type, and the decoder has not classified anything as a mismatch.
func TestSpec10_5_MethodErrorsAreWrapped(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		dst  func() any
	}{
		{"Unmarshaler", "i42e", func() any { return new(failBenc) }},
		{"TextUnmarshaler", "4:spam", func() any { return new(failText) }},
		{"TextUnmarshaler map key", "d1:ai1ee", func() any { return new(map[failTextKey]int64) }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := decode(t, c.in, c.dst())
			require.Error(t, err)

			assert.ErrorIs(t, err, errCustom, "the cause must survive to the caller")
			assert.NotErrorIs(t, err, ErrTypeMismatch,
				"SPEC §10.5: the decoder has not classified a mismatch")

			var te *TypeError
			require.ErrorAs(t, err, &te)
			assert.NotNil(t, te.Type, "SPEC §5.2: the destination type must be reported")
			assertOffsetInBounds(t, err, int64(len(c.in)))
		})
	}
}

// SPEC §6.1: a custom unmarshaler's failure poisons the decoder like any other
// type error. Nothing about §10 opens a recovery path §6.2 closed.
func TestSpec10_5_MethodErrorPoisonsTheDecoder(t *testing.T) {
	t.Parallel()

	d := NewDecoder(strings.NewReader("i1ei2e"))

	var bad failBenc
	err := decodeWith(t, d, &bad)
	require.ErrorIs(t, err, errCustom)

	var again int64
	assert.Equal(t, err, decodeWith(t, d, &again))
}
