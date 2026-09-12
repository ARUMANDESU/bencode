package bencode

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

	// With tinyLimits the scan bounds are tens of bytes, so this much junk is
	// far past every one of them. The assertion is that the decoder stops
	// early — not where exactly it stops.
	//
	// countingReader sees what bufio prefetched, not what the decoder
	// consumed, so allowed has to clear bufio's own read granularity: junk is
	// many buffers' worth, allowed is a couple of buffers. Reading to EOF and
	// stopping at the bound are then far apart.
	const junk = 64 << 10
	const allowed = 8192

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
		// 8192, not 4096: countingReader sees bufio's prefetch, and one buffer
		// fill is already 4096. The input is 15002 bytes, so stopping at the
		// limit and reading to EOF stay far apart.
		assert.Less(t, r.read, 8192,
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
