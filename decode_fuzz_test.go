package bencode

import (
	"bytes"
	"reflect"
	"testing"

	bencodeast "github.com/arumandesu/bencode/pkg/bencode_ast"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// fuzz
//
// The tables elsewhere in this suite assert what the spec says. Fuzzing
// asserts what a table can never cover: that no input panics (SPEC §8), that
// decoding is self-consistent, and that error offsets stay inside the input.
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
