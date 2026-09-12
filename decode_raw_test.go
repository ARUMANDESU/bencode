package bencode

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bencodeast "github.com/arumandesu/bencode/pkg/bencode_ast"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// SPEC §9 — RawMessage
//
// Capture is the one feature whose failure mode is silent. A byte dropped or a
// window misaligned does not error and does not panic; it yields a hash that
// is merely wrong, for a torrent that then never downloads. So these tests
// assert the bytes themselves, and — wherever the assertion survives it —
// assert them again under source chunkings that separate bufio's refill clock
// from the parser's consume clock (§9.4).
// ---------------------------------------------------------------------------

// The input tabulated in SPEC §9.4, kept byte-for-byte so the offsets in that
// table (capture start 7, end 20) still describe this fixture.
const (
	specWorkedExample = "d4:infod6:lengthi7ee8:announce3:abce"
	specWorkedInfo    = "d6:lengthi7ee"
)

// captureCases are the four rows of the SPEC §9.1 table plus the shapes where
// "the value" and "the content" diverge most: empty containers, whose whole
// byte content is the delimiters an off-by-one would eat, and nesting deep
// enough that the end of the value is only findable by parsing to it (§9.3).
var captureCases = []struct{ name, in string }{
	{"integer", "i4e"},
	{"negative integer", "i-42e"},
	{"zero", "i0e"},
	{"string", "6:string"},
	{"empty string", "0:"},
	{"binary string", "3:\x00\xff\n"},
	{"list", "li4ee"},
	{"empty list", "le"},
	{"dict", "d6:lengthi7ee"},
	{"empty dict", "de"},
	{"nested containers", "d1:ald1:bi1eeleee"},
	{"repetitive list", "l" + repeat("i0e", 40) + "e"},
	{"deeply nested", nest(32)},
}

// SPEC §9.1: a capture is the complete value — type prefix, length prefix and
// terminator included — for every one of the four bencode types.
func TestSpec9_1_CaptureIsTheWholeValue(t *testing.T) {
	t.Parallel()

	for _, tt := range captureCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			eachChunking(t, tt.in, func(t *testing.T, r io.Reader) {
				var got RawMessage
				require.NoError(t, decodeWith(t, NewDecoder(r), &got))
				assert.Equal(t, tt.in, string(got))
			})
		})
	}
}

// SPEC §9.1: "[]byte gives you the content. RawMessage gives you the value."
// The two destinations have the same underlying type and must not behave the
// same way.
func TestSpec9_1_ByteSliceTakesContentCaptureTakesTheValue(t *testing.T) {
	t.Parallel()

	var content []byte
	require.NoError(t, decode(t, "4:spam", &content))
	assert.Equal(t, "spam", string(content))

	var value RawMessage
	require.NoError(t, decode(t, "4:spam", &value))
	assert.Equal(t, "4:spam", string(value),
		"content-only capture would make RawMessage a duplicate of []byte and "+
			"would break the §9.2.1 round trip: \"spam\" is not a bencode value")
}

// SPEC §9.1 + §4: capture is selected by destination type, so it has to work
// wherever a destination can appear — not only as a struct field.
func TestSpec9_1_CaptureInEveryPosition(t *testing.T) {
	t.Parallel()

	t.Run("top level", func(t *testing.T) {
		t.Parallel()
		var got RawMessage
		require.NoError(t, decode(t, specWorkedExample, &got))
		assert.Equal(t, specWorkedExample, string(got))
	})

	t.Run("struct field, siblings decode normally", func(t *testing.T) {
		t.Parallel()
		var got rawHolder
		require.NoError(t, decode(t, specWorkedExample, &got))
		assert.Equal(t, specWorkedInfo, string(got.Info))
		assert.Equal(t, "abc", got.Announce)
	})

	t.Run("pointer field is allocated", func(t *testing.T) {
		t.Parallel()
		var got twoRaw
		require.NoError(t, decode(t, "d1:pd1:x0:ee", &got))
		require.NotNil(t, got.P)
		assert.Equal(t, "d1:x0:e", string(*got.P))
	})

	t.Run("slice element", func(t *testing.T) {
		t.Parallel()
		var got []RawMessage
		require.NoError(t, decode(t, "ld1:ai1eei2e4:spame", &got))
		assert.Equal(t,
			[]string{"d1:ai1ee", "i2e", "4:spam"},
			[]string{string(got[0]), string(got[1]), string(got[2])})
	})

	t.Run("map value", func(t *testing.T) {
		t.Parallel()
		var got map[string]RawMessage
		require.NoError(t, decode(t, "d1:ai1e1:bl1:xee", &got))
		assert.Equal(t, map[string]string{"a": "i1e", "b": "l1:xe"},
			map[string]string{"a": string(got["a"]), "b": string(got["b"])})
	})

	t.Run("field of a nested struct", func(t *testing.T) {
		t.Parallel()
		type outer struct {
			N     int64     `bencode:"n"`
			Inner rawHolder `bencode:"inner"`
		}

		var got outer
		require.NoError(t, decode(t, "d1:ni5e5:innerd8:announce3:abc4:infod1:xi1eeee", &got))
		assert.Equal(t, int64(5), got.N)
		assert.Equal(t, "abc", got.Inner.Announce)
		assert.Equal(t, "d1:xi1ee", string(got.Inner.Info))
	})
}

// SPEC §3.4 applies to captures like any other field: a reused destination is
// replaced, never appended to. SetBytes on a non-empty slice is exactly where
// an append would hide.
func TestSpec9_1_CaptureReplacesAnExistingValue(t *testing.T) {
	t.Parallel()

	got := rawHolder{Info: RawMessage("d9:leftoveri1ee"), Announce: "stale"}
	require.NoError(t, decode(t, specWorkedExample, &got))
	assert.Equal(t, specWorkedInfo, string(got.Info))
}

// SPEC §9.2.1: captured bytes fed to a fresh Decoder produce a decode
// identical to decoding the value in place. This is the guarantee that makes
// deferred decoding work, and it is the reason capture is whole-value.
func TestSpec9_2_RoundTrip(t *testing.T) {
	t.Parallel()

	for _, tt := range captureCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var inPlace any
			require.NoError(t, decode(t, tt.in, &inPlace))

			var raw RawMessage
			require.NoError(t, decode(t, tt.in, &raw))

			var viaRaw any
			// Unmarshal, not Decode: a capture must be a complete value with
			// nothing trailing, and §6.4 is what actually checks that.
			require.NoError(t, Unmarshal(raw, &viaRaw))

			assert.Equal(t, inPlace, viaRaw)
		})
	}
}

// SPEC §9.2.2: the capture is the input's bytes, not a re-encoding. §2.1
// leniency means a re-encode cannot recover key order or duplicates — which is
// the entire reason this type exists, since an infohash is sha1 of the bytes
// that were on the wire.
func TestSpec9_2_ByteExactness(t *testing.T) {
	t.Parallel()

	nonCanonical := []struct{ name, in string }{
		{"unsorted keys", "d1:b1:x1:a1:ye"},
		{"duplicate keys", "d1:a1:x1:a1:ye"},
		{"unsorted keys nested", "d1:bd2:zz1:x1:a1:ye1:ai0ee"},
	}

	for _, tt := range nonCanonical {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var raw RawMessage
			require.NoError(t, decode(t, tt.in, &raw))
			assert.Equal(t, tt.in, string(raw))

			// The control: a re-encoding of the same input differs, so the
			// assertion above cannot be passing by accident.
			v, err := bencodeast.Decode(strings.NewReader(tt.in))
			require.NoError(t, err)
			assert.NotEqual(t, tt.in, mustEncode(t, v),
				"fixture is canonical, so it proves nothing about re-encoding")
		})
	}
}

// SPEC §9.2.3: a RawMessage never aliases the decoder's buffers. The recorder
// reuses one window across captures, so a returned sub-slice of it would be
// silently overwritten by the next capture — the caller's first hash would be
// wrong and nothing would report it.
func TestSpec9_2_Ownership(t *testing.T) {
	t.Parallel()

	t.Run("a later capture does not clobber an earlier one", func(t *testing.T) {
		t.Parallel()

		var got twoRaw
		require.NoError(t, decode(t, "d1:a4:spam1:bli1ei2ee1:pd1:x0:ee", &got))
		assert.Equal(t, "4:spam", string(got.A))
		assert.Equal(t, "li1ei2ee", string(got.B))
		require.NotNil(t, got.P)
		assert.Equal(t, "d1:x0:e", string(*got.P))
	})

	t.Run("captures from successive Decode calls are independent", func(t *testing.T) {
		t.Parallel()
		d := NewDecoder(strings.NewReader("d1:s4:spamei-1el1:ae"))

		var first, second, third RawMessage
		require.NoError(t, decodeWith(t, d, &first))
		require.NoError(t, decodeWith(t, d, &second))
		require.NoError(t, decodeWith(t, d, &third))

		assert.Equal(t, "d1:s4:spame", string(first))
		assert.Equal(t, "i-1e", string(second))
		assert.Equal(t, "l1:ae", string(third))
	})

	t.Run("the caller may mutate a capture", func(t *testing.T) {
		t.Parallel()
		d := NewDecoder(strings.NewReader("4:spam4:eggs"))

		var first RawMessage
		require.NoError(t, decodeWith(t, d, &first))
		for i := range first {
			first[i] = 'Z'
		}

		var second RawMessage
		require.NoError(t, decodeWith(t, d, &second))
		assert.Equal(t, "4:eggs", string(second),
			"the decoder read through a buffer the caller was allowed to scribble on")
		assert.Equal(t, "ZZZZZZ", string(first))
	})
}

// SPEC §9.3: capture is skipValue with recording on, so the end of a container
// is found by parsing, never by scanning for a terminator byte. A value whose
// payload is full of 'e', 'd' and 'l' bytes is what tells the two apart.
func TestSpec9_3_CaptureFindsTheEndByParsing(t *testing.T) {
	t.Parallel()

	type holder struct {
		Raw RawMessage `bencode:"raw"`
		S   string     `bencode:"s"`
	}

	cases := []struct{ name, in, want string }{
		{
			"string payload full of delimiters",
			"d3:raw10:deeeleiiie1:s5:aftere",
			"10:deeeleiiie",
		},
		{
			"nested containers ending in a run of terminators",
			"d3:rawld1:aleelee1:s5:aftere",
			"ld1:aleelee",
		},
		{
			"container whose strings hold terminators",
			"d3:rawd1:e1:e1:l2:eee1:s5:aftere",
			"d1:e1:e1:l2:eee",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			eachChunking(t, tt.in, func(t *testing.T, r io.Reader) {
				var got holder
				require.NoError(t, decodeWith(t, NewDecoder(r), &got))
				assert.Equal(t, tt.want, string(got.Raw))
				// The sibling proves the stream is positioned exactly after
				// the captured value (§6.1): a capture one byte long or one
				// byte short would desynchronise the enclosing dict.
				assert.Equal(t, "after", got.S)
			})
		})
	}
}

// SPEC §9.4: the capture window is a pair of *consumed* offsets, corrected for
// bufio's read-ahead. Marks taken on the refill clock instead would move with
// the source's chunking — the failure mode is non-deterministic and looks
// exactly like a plausible hash.
func TestSpec9_4_CaptureIsIndependentOfSourceChunking(t *testing.T) {
	t.Parallel()

	t.Run("worked example from the spec", func(t *testing.T) {
		t.Parallel()
		eachChunking(t, specWorkedExample, func(t *testing.T, r io.Reader) {
			var got rawHolder
			require.NoError(t, decodeWith(t, NewDecoder(r), &got))
			assert.Equal(t, specWorkedInfo, string(got.Info))
		})
	})

	// A value far larger than bufio's buffer, so recording spans many refills
	// and the correction is applied over and over rather than once.
	t.Run("value larger than the read buffer", func(t *testing.T) {
		t.Parallel()

		infoDict := bencodeast.Dict{
			"name":         bencodeast.Str("fixture"),
			"piece length": bencodeast.Int(262144),
			"pieces":       bencodeast.Str(repeat("\xab\x00\xff\n", 4000)),
		}
		// The expected bytes come from the encoder, not from the decoder under
		// test, and the fixture is canonical so the nested encoding is
		// literally a substring of the whole.
		wantInfo := mustEncode(t, infoDict)
		input := mustEncode(t, bencodeast.Dict{
			"announce": bencodeast.Str("http://tracker.example/announce"),
			"info":     infoDict,
		})
		require.Contains(t, input, wantInfo)
		require.Greater(t, len(wantInfo), 4096, "fixture must outgrow the read buffer")

		eachChunking(t, input, func(t *testing.T, r io.Reader) {
			var got rawHolder
			require.NoError(t, decodeWith(t, NewDecoder(r), &got))
			assert.Equal(t, len(wantInfo), len(got.Info))
			assert.True(t, bytes.Equal([]byte(wantInfo), got.Info))
		})
	})
}

// SPEC §9.5: NewDecoder always wraps, so the recorder sits under a bufio.Reader
// the decoder itself constructed. Adopting a caller's *bufio.Reader would put
// the source behind someone else's read-ahead — and would corrupt captures
// only for callers who pass one, which is the worst possible way to be wrong.
func TestSpec9_5_CaptureSurvivesACallerSuppliedBufioReader(t *testing.T) {
	t.Parallel()

	sources := []struct {
		name string
		new  func() io.Reader
	}{
		{"bufio over a whole-answer reader", func() io.Reader {
			return bufio.NewReader(strings.NewReader(specWorkedExample))
		}},
		{"bufio with a tiny buffer over a chunked reader", func() io.Reader {
			return bufio.NewReaderSize(&chunkyReader{src: []byte(specWorkedExample), size: 3}, 16)
		}},
		{"bufio that has already read ahead", func() io.Reader {
			br := bufio.NewReader(strings.NewReader(specWorkedExample))
			// Pull bytes into the caller's buffer before the decoder ever
			// sees the reader: the offset correction must be measured
			// against the decoder's own buffer, not this one.
			if _, err := br.Peek(8); err != nil {
				panic(err)
			}
			return br
		}},
	}

	for _, src := range sources {
		t.Run(src.name, func(t *testing.T) {
			t.Parallel()
			var got rawHolder
			require.NoError(t, decodeWith(t, NewDecoder(src.new()), &got))
			assert.Equal(t, specWorkedInfo, string(got.Info))
			assert.Equal(t, "abc", got.Announce)
		})
	}
}

// SPEC §9.6: recording is off by default. With no RawMessage in the
// destination nothing is retained, so MaxCaptureBytes cannot fire — and a
// value skipped *around* a capture is not retained either, because the window
// opens at the start of the capture and not before.
func TestSpec9_6_RecordingIsOffByDefault(t *testing.T) {
	t.Parallel()

	// Comfortably over tinyLimits().MaxCaptureBytes (64) and under
	// MaxValueBytes (256), so only the capture limit is in question.
	bigValue := "l" + repeat("i0e", 40) + "e"

	t.Run("no capture in the destination retains nothing", func(t *testing.T) {
		t.Parallel()
		var got simple
		require.NoError(t, decodeWith(t, tinyDecoder("d4:junk"+bigValue+"1:s4:spame"), &got))
		assert.Equal(t, "spam", got.S)
	})

	t.Run("a large sibling of a capture is not retained", func(t *testing.T) {
		t.Parallel()
		type holder struct {
			Raw RawMessage `bencode:"raw"`
		}

		var got holder
		require.NoError(t, decodeWith(t, tinyDecoder("d4:junk"+bigValue+"3:rawi7ee"), &got))
		assert.Equal(t, "i7e", string(got.Raw))
	})

	t.Run("a large value skipped after a capture is not retained", func(t *testing.T) {
		t.Parallel()
		type holder struct {
			Raw RawMessage `bencode:"raw"`
		}

		var got holder
		require.NoError(t, decodeWith(t, tinyDecoder("d3:rawi7e4:junk"+bigValue+"e"), &got))
		assert.Equal(t, "i7e", string(got.Raw))
	})
}

// SPEC §7.1 + §9.6: MaxCaptureBytes bounds the retention window as it grows.
// It is the only thing standing between a hostile peer and unbounded memory on
// the capture path, so it must hold for every source chunking — a limit
// enforced only when the source happens to dribble is not a limit.
func TestSpec9_6_MaxCaptureBytes(t *testing.T) {
	t.Parallel()

	const limit = 64

	t.Run("a capture at the limit is accepted", func(t *testing.T) {
		t.Parallel()
		in := "l" + repeat("i0e", 20) + "e" // 62 bytes
		require.LessOrEqual(t, len(in), limit)

		var got RawMessage
		require.NoError(t, decodeWith(t, tinyDecoder(in), &got))
		assert.Equal(t, in, string(got))
	})

	t.Run("a capture over the limit is rejected", func(t *testing.T) {
		t.Parallel()
		in := "l" + repeat("i0e", 40) + "e" // 122 bytes
		require.Greater(t, len(in), limit)

		eachChunking(t, in, func(t *testing.T, r io.Reader) {
			d := NewDecoder(r)
			d.Limits = tinyLimits()

			var got RawMessage
			err := decodeWith(t, d, &got)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrExceedsMax)

			var limitErr *LimitError
			require.ErrorAs(t, err, &limitErr)
			assert.Equal(t, "MaxCaptureBytes", limitErr.Limit)
			// SPEC §5.2: an offset outside the input sends the caller looking
			// at the wrong byte. The recorder maintains the offset clock, so
			// its own failure path is where the clock is most likely to slip.
			assertOffsetInBounds(t, err, int64(len(in)))

			// SPEC §6.1: a limit error poisons like any other.
			var again RawMessage
			assert.Equal(t, err, decodeWith(t, d, &again))
		})
	})

	t.Run("the limit is the window, not the whole stream", func(t *testing.T) {
		t.Parallel()
		// Every value is small; only their sum is not. Charging a capture for
		// bytes consumed before it opened would reject this.
		in := "d1:a" + repeat("i0e", 1) + "4:junk" + "l" + repeat("i0e", 30) + "e" + "1:bi1ee"

		type holder struct {
			A RawMessage `bencode:"a"`
			B RawMessage `bencode:"b"`
		}

		var got holder
		require.NoError(t, decodeWith(t, tinyDecoder(in), &got))
		assert.Equal(t, "i0e", string(got.A))
		assert.Equal(t, "i1e", string(got.B))
	})
}

// SPEC §9.6 + §5.3 + §6.1: a capture interrupted by an error is abandoned —
// there is nothing to unwind, because the decoder is poisoned and will produce
// no further values.
func TestSpec9_6_InterruptedCapture(t *testing.T) {
	t.Parallel()

	truncated := []struct{ name, in string }{
		{"int missing terminator", "i42"},
		{"string shorter than declared", "10:abc"},
		{"list unterminated", "li1e"},
		{"dict value truncated", "d3:fooi42"},
		{"dict missing final terminator", "d3:foo3:bar"},
	}

	for _, tt := range truncated {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := NewDecoder(strings.NewReader(tt.in))

			var got RawMessage
			err := decodeWith(t, d, &got)
			require.Error(t, err)
			assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
			assert.NotErrorIs(t, err, io.EOF,
				"a value cut in half is not a clean end of stream")

			var again RawMessage
			assert.Equal(t, err, decodeWith(t, d, &again))
		})
	}

	t.Run("syntax error inside a captured value", func(t *testing.T) {
		t.Parallel()
		d := NewDecoder(strings.NewReader("l1:ax"))

		var got RawMessage
		err := decodeWith(t, d, &got)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrSyntax)

		var again RawMessage
		assert.Equal(t, err, decodeWith(t, d, &again))
	})
}

// SPEC §5.2 + §9.1: a capture accepts every bencode value, so "this value
// cannot go into this destination" is a category that cannot arise for a
// RawMessage. Whatever went wrong was the input, and the error has to keep the
// shape §5.2 gives it — a *SyntaxError with the offending byte and its offset.
//
// Re-wrapping the walk's error as a *TypeError costs the caller the diagnosis
// twice over: the message names the destination for a fault the destination
// did not have ("cannot unmarshal  into Go value of type bencode.RawMessage",
// with Value empty because there is no offending value), and errors.As for
// *SyntaxError — the thing that carries the byte and the position — misses.
func TestSpec9_ErrorsFromACaptureKeepTheirSyntaxShape(t *testing.T) {
	t.Parallel()

	malformed := []struct{ name, in string }{
		{"invalid leading byte", "x"},
		{"invalid byte inside a list", "l1:ax"},
		{"malformed integer", "i4-2e"},
		{"leading zero", "i03e"},
	}

	for _, tt := range malformed {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got RawMessage
			err := decode(t, tt.in, &got)
			require.Error(t, err)

			// §5.2: the sentinel classifies, the struct locates. Capture must
			// not lose either half.
			var syntaxErr *SyntaxError
			require.ErrorAs(t, err, &syntaxErr)
			assertOffsetInBounds(t, err, int64(len(tt.in)))

			var typeErr *TypeError
			assert.NotErrorAs(t, err, &typeErr,
				"§9.1: every input is a valid capture, so no input mismatches the destination")
			assert.NotErrorIs(t, err, ErrTypeMismatch)
		})
	}
}

// SPEC §5.3: capture does not get its own EOF rules. A stream that ends at a
// value boundary is a clean end of stream, reported as a bare io.EOF, whatever
// the destination is — that is what terminates the caller's decode loop.
func TestSpec9_6_CleanEndOfStreamIntoACapture(t *testing.T) {
	t.Parallel()

	t.Run("empty input", func(t *testing.T) {
		t.Parallel()
		var got RawMessage
		assert.ErrorIs(t, decode(t, "", &got), io.EOF)
	})

	t.Run("after a complete value", func(t *testing.T) {
		t.Parallel()
		d := NewDecoder(strings.NewReader("i1e"))

		var first RawMessage
		require.NoError(t, decodeWith(t, d, &first))
		assert.Equal(t, "i1e", string(first))

		var second RawMessage
		assert.ErrorIs(t, decodeWith(t, d, &second), io.EOF)
	})

	// The other half of the same rule, and the one that is dangerous to get
	// wrong: a stream that runs out *inside* a container is a truncation, not
	// a clean end. Reporting io.EOF there hands the caller's drain loop a
	// `break` on a corrupt file — the loop exits, the error is swallowed, and
	// the half-read torrent looks complete.
	//
	// Capture reaches its destination through a different door than every
	// other type: the RawMessage branch is taken before the type byte is read,
	// so the depth-aware EOF classification the normal path applies has to be
	// applied on the capture path too.
	t.Run("truncated inside a container is not a clean end", func(t *testing.T) {
		t.Parallel()

		cases := []struct {
			name string
			in   string
			new  func() any
		}{
			{"dict key with no value", "d3:foo", func() any { return new(map[string]RawMessage) }},
			{"dict value truncated", "d3:fooi42", func() any { return new(map[string]RawMessage) }},
			{"struct field with no value", "d4:info", func() any { return new(rawHolder) }},
			{"list element missing", "l", func() any { return new([]RawMessage) }},
			{"list unterminated after an element", "li1e", func() any { return new([]RawMessage) }},
		}

		for _, tt := range cases {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				err := decode(t, tt.in, tt.new())
				require.Error(t, err)
				assert.NotErrorIs(t, err, io.EOF)
				assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
			})
		}
	})
}

// SPEC §9.7: the infohash comes from the bytes that were on the wire, and the
// same bytes decode again on demand. This is the whole feature, end to end,
// against a file nobody wrote for this test.
//
// The expected hash was produced by an independent bencode parser, not by this
// package — a golden value computed by the code under test would agree with
// any bug it has.
func TestSpec9_7_InfohashFromRealTorrent(t *testing.T) {
	t.Parallel()

	const wantInfohash = "b21c2f693c38293d45e8d9b1fe07458149f4122e"

	matches, err := filepath.Glob(filepath.Join("testdata", "*.torrent"))
	require.NoError(t, err)
	require.NotEmpty(t, matches, "no .torrent file found in testdata")

	fileBytes, err := os.ReadFile(matches[0])
	require.NoError(t, err)

	type torrentRaw struct {
		Announce string     `bencode:"announce"`
		Info     RawMessage `bencode:"info"`
	}

	var got torrentRaw
	require.NoError(t, NewDecoder(bytes.NewReader(fileBytes)).Decode(&got))
	require.NotEmpty(t, got.Info)

	sum := sha1.Sum(got.Info)
	assert.Equal(t, wantInfohash, hex.EncodeToString(sum[:]))

	// Independent of the hash: the capture must be the file's own bytes, at
	// the offset where the `info` value starts. The key appears once, so the
	// start is findable without parsing; asserting the whole slice matches
	// there pins the end too.
	start := bytes.Index(fileBytes, []byte("4:info"))
	require.GreaterOrEqual(t, start, 0)
	assert.Equal(t, 1, bytes.Count(fileBytes, []byte("4:info")),
		"ambiguous fixture: the offset below would not identify the info value")
	start += len("4:info")
	require.LessOrEqual(t, start+len(got.Info), len(fileBytes))
	assert.True(t, bytes.Equal(fileBytes[start:start+len(got.Info)], got.Info))

	// SPEC §9.2.1: the capture is still a bencode value, so it decodes on
	// demand — and to the same thing an in-place decode produces.
	var viaRaw torrentInfo
	require.NoError(t, Unmarshal(got.Info, &viaRaw))

	var inPlace torrentMeta
	require.NoError(t, NewDecoder(bytes.NewReader(fileBytes)).Decode(&inPlace))
	assert.Equal(t, inPlace.Info, viaRaw)
	assert.Equal(t, inPlace.Announce, got.Announce)
}

// SPEC §9.7: capturing a key raw and decoding it into a typed field at the
// same time is not available — two fields tagged for one key are ambiguous
// under §3.3.1 rule 2, and bind to neither. This is the reason the documented
// usage is a two-step, so it is worth a standing test rather than a comment.
func TestSpec9_7_CaptureAndTypedFieldForOneKeyIsAmbiguous(t *testing.T) {
	t.Parallel()

	type both struct {
		Raw   RawMessage `bencode:"info"`
		Typed simple     `bencode:"info"`
	}

	var got both
	require.NoError(t, decode(t, "d4:infod1:s4:spamee", &got))
	assert.Empty(t, got.Raw)
	assert.Equal(t, simple{}, got.Typed)
}
