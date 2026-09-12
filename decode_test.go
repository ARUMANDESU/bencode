package bencode

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"

	bencodeast "github.com/arumandesu/bencode/pkg/bencode_ast"
	"github.com/stretchr/testify/require"
)

// This file asserts SPEC.md draft 3 and nothing else. Every test cites the
// section it enforces. When a test and the implementation disagree, the spec
// decides which one is wrong — a test that merely describes what the code
// happens to do today is worthless, because it can never fail for a reason
// worth knowing.
//
// This file holds the fixtures and helpers the suite shares. The assertions
// live next to the section they enforce:
//
//	decode_syntax_test.go       §2 grammar and leniency, §5 errors
//	decode_binding_test.go      §3 destination binding, §4 type mapping
//	decode_errors_test.go       §6 stream contract
//	decode_limits_test.go       §7 limits, §8 never panics
//	decode_raw_test.go          §9 RawMessage
//	decode_integration_test.go  §10.1 concurrency, real torrent files
//	decode_fuzz_test.go         fuzz targets

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

// ambiguous pins SPEC §3.3.2: two tagged fields claiming one key bind to
// nothing. Declaration order must not decide.
type ambiguous struct {
	A string `bencode:"dup"`
	B string `bencode:"dup"`
}

// taggedBeatsUntagged pins the one same-depth collision SPEC §3.3.2 resolves. The two
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
// supplies no name does NOT make the field tagged for §3.3.2. Both fields here
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

// The embedded fixtures below are one per row of the SPEC §3.3.1 table, plus
// the collision shapes §3.3.2 rule 1 exists for. An embedded field's name is
// its TYPE's name, so `hidden` embedded is an unexported field — which under
// draft 3 is walked through rather than skipped, while `*hidden` is pruned
// because allocating it would need a Set on a read-only value (§8).
type Exported struct {
	S string `bencode:"s"`
}

// embedsExported is the tagged embed: a tag asks for the field itself, so it
// is bound as an ordinary field under "emb" and NOT lifted.
type embedsExported struct {
	Exported `bencode:"emb"`
	Extra    string `bencode:"extra"`
}

// liftsExported is the same shape untagged, which is the draft 3 change: "s"
// reaches Exported.S and "Exported" reaches nothing.
type liftsExported struct {
	Exported
	Extra string `bencode:"extra"`
}

type liftsExportedPtr struct {
	*Exported
	Extra string `bencode:"extra"`
}

// skipsEmbedded pins §3.3 rule 2 against an embedded field: "-" skips the
// field AND its contents, so neither "s" nor "Exported" binds to anything.
type skipsEmbedded struct {
	Exported `bencode:"-"`
	Extra    string `bencode:"extra"`
}

// hidden is unexported with exported fields — the mixin shape. reflect marks
// the embedded field read-only but does not propagate that to its fields, so
// hidden.S is settable and must be reachable under "s".
type hidden struct {
	S string `bencode:"s"`
	N int64  `bencode:"n"`
}

type liftsUnexported struct {
	hidden
	Extra string `bencode:"extra"`
}

// liftsUnexportedPtr is the pruned row: the embedded pointer cannot be
// allocated, so every key under it binds to nothing — silently, like an
// ambiguous key, and never as a panic.
type liftsUnexportedPtr struct {
	*hidden
	Extra string `bencode:"extra"`
}

// taggedUnexportedEmbed is invisible per §3.3.1: the tag asks for a binding
// rule 1 forbids, and asking for a key is not a request to lift.
type taggedUnexportedEmbed struct {
	hidden `bencode:"h"`
	Extra  string `bencode:"extra"`
}

// Deep/Mid pin that lifting is transitive: "d" is reached at depth 2.
type Deep struct {
	D string `bencode:"d"`
}

type Mid struct {
	Deep
}

type liftsTransitively struct {
	Mid
	Extra string `bencode:"extra"`
}

// shadowsEmbedded pins §3.3.2 rule 1: depth 0 beats depth 1 outright, and the
// promoted field must be left zero rather than written twice.
type shadowsEmbedded struct {
	Exported
	S string `bencode:"s"`
}

// Left/Right collide at the same depth with no tag between them, which is the
// decoder's echo of the compile error `x.Name` would be.
type Left struct {
	Name string `bencode:"name"`
}

type Right struct {
	Name string `bencode:"name"`
}

type ambiguousEmbeds struct {
	Left
	Right
}

// PlainName/TaggedName collide at the same depth with exactly one tag, which
// §3.3.2 rule 2 resolves in favour of the tagged one.
type PlainName struct {
	Name string
}

type TaggedName struct {
	Other string `bencode:"Name"`
}

type embedTieBrokenByTag struct {
	PlainName
	TaggedName
}

// Diamond: both embeds reach Exported, so "s" has two depth-2 claimants and
// binds to neither — while "extra" at depth 0 is unaffected.
type LeftDiamond struct {
	Exported
}

type RightDiamond struct {
	Exported
}

type diamond struct {
	LeftDiamond
	RightDiamond
	Extra string `bencode:"extra"`
}

// Recursive makes the field-tree walk infinite unless it stops revisiting a
// type (§3.3.1). Building its field map must terminate, and "v" must resolve
// to the depth-0 field rather than to any of its promoted copies.
type Recursive struct {
	*Recursive
	V string `bencode:"v"`
}

// MyInt is a non-struct named type: embedded, it stays an ordinary field keyed
// by its type name, never lifted.
type MyInt int64

type embedsNamedInt struct {
	MyInt
	Extra string `bencode:"extra"`
}

type unexportedMap map[string]int

type unexportedSlice []string

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

// rawHolder is the SPEC §9.7 shape: one key is captured verbatim for hashing
// while its siblings decode normally.
type rawHolder struct {
	Announce string     `bencode:"announce"`
	Info     RawMessage `bencode:"info"`
}

// twoRaw exists for the §9.2.3 ownership tests: two captures in one Decode,
// where a returned slice that still aliases the recorder's window would be
// clobbered by the second capture.
type twoRaw struct {
	A RawMessage  `bencode:"a"`
	B RawMessage  `bencode:"b"`
	P *RawMessage `bencode:"p"`
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

// chunkyReader hands the decoder its input in pieces of the given size,
// independent of how much the decoder asks for.
//
// This is the whole point of SPEC §9.4: capture marks are taken on the
// *consume* clock, and a recorder that reports the *refill* clock instead
// drifts by however much bufio happened to read ahead. On a socket that gap
// depends on packet timing, so a capture that is right for one chunking and
// wrong for another is a bug that only shows up in production. Every capture
// assertion that can be run against a chunked source should be.
type chunkyReader struct {
	src  []byte
	size int
}

func (c *chunkyReader) Read(p []byte) (int, error) {
	if len(c.src) == 0 {
		return 0, io.EOF
	}
	n := min(c.size, len(c.src), len(p))
	copy(p, c.src[:n])
	c.src = c.src[n:]
	return n, nil
}

// chunkSizes spans the shapes that separate the two clocks: byte-at-a-time
// (bufio's buffer is never full), sizes that straddle value boundaries, and
// one large enough that the whole fixture arrives in a single Read.
var chunkSizes = []int{1, 2, 3, 7, 13, 64, 1 << 16}

// eachChunking runs fn against input delivered every way in chunkSizes, plus
// once through strings.Reader, which answers every Read in full.
func eachChunking(t *testing.T, input string, fn func(t *testing.T, r io.Reader)) {
	t.Helper()
	t.Run("whole", func(t *testing.T) {
		t.Parallel()
		fn(t, strings.NewReader(input))
	})
	for _, size := range chunkSizes {
		t.Run(fmt.Sprintf("chunks of %d", size), func(t *testing.T) {
			t.Parallel()
			fn(t, &chunkyReader{src: []byte(input), size: size})
		})
	}
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
	// The §3.3.1 rows whose reflect operations panic when unguarded: lifting
	// out of an unexported struct is legal, allocating a pointer to one is
	// not, and a recursive embed must not hang the field-map walk.
	{"embedded unexported struct", func() any { return new(liftsUnexported) }},
	{"embedded pointer to unexported struct", func() any { return new(liftsUnexportedPtr) }},
	{"embedded pointer", func() any { return new(liftsExportedPtr) }},
	{"recursive embed", func() any { return new(Recursive) }},
	{"diamond embed", func() any { return new(diamond) }},
	// SPEC §9: capture accepts every input shape, so it is the one
	// destination that must survive the whole corpus without erroring — which
	// makes it the one most likely to walk off the end of a truncated value.
	{"RawMessage", func() any { return new(RawMessage) }},
	{"struct with RawMessage field", func() any { return new(rawHolder) }},
	{"[]RawMessage", func() any { return new([]RawMessage) }},
	{"map[string]RawMessage", func() any { return new(map[string]RawMessage) }},
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
