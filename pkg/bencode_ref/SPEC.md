# bencode_ref — decoder spec

Status: draft 1. This document is the authority. Tests assert what is written
here; when behaviour and spec disagree, the spec wins and the code is the bug.
Anything not stated here is undefined and must not be relied on.

---

## 1. Scope

A streaming bencode **decoder** that maps values onto Go destinations via
reflection. One `Decoder` reads successive values from one `io.Reader`.

Non-goals for v1 (listed so their absence is a decision, not an oversight):

- encoding
- embedded / anonymous struct flattening
- a custom `Unmarshaler` interface
- `encoding.TextUnmarshaler` map keys
- strict canonical-form validation
- case-insensitive field matching

---

## 2. Accepted grammar

| type | form | rules |
|---|---|---|
| integer | `i<sign?><digits>e` | optional leading `-`; digits only; no leading zero (`i03e` invalid); no `-0`; `i0e` valid |
| string | `<len>:<bytes>` | `len` is digits only, no leading zero unless it is exactly `0`; `0:` valid; bytes are arbitrary, including NUL and invalid UTF-8 |
| list | `l<value>*e` | values are any bencode type |
| dict | `d(<string><value>)*e` | keys **must** be bencode strings |

A non-string dict key is malformed input, not a destination problem →
`ErrSyntax`.

### Leniency

The decoder is **lenient** about canonical form, because real `.torrent` files
in the wild violate it and a decoder that cannot open them is useless:

| input | behaviour |
|---|---|
| `d1:b1:x1:a1:ye` (keys unsorted) | accepted |
| `d1:a1:x1:a1:ye` (duplicate key) | accepted, last occurrence wins |
| `i42egarbage` (trailing bytes) | accepted, `42` returned; trailing bytes stay in the buffer for the next `Decode` |

Corollary: an accepted input is **not** guaranteed to re-encode
byte-identically. This is why §9 exists.

---

## 3. Destinations

### 3.1 Entry point

`Decode(v any)` requires `v` to be a **non-nil pointer**. Anything else —
non-pointer, typed nil pointer, untyped nil — is `ErrInvalidDestination`,
returned **before any byte is read**.

`ErrInvalidDestination` is reachable **only** from this check. Inside the
decoder every destination is, by construction, addressable and settable; an
unsettable value there is an internal invariant violation, not a caller error.
Internal `CanSet` guards are therefore assertions, not error paths.

### 3.2 Pointers

A pointer destination at **any** depth is allocated on demand and decoded
through. This applies recursively (`**T` works).

Since bencode has no null, the rule is exact:

> a pointer field is non-nil **iff** the key was present in the input.

An absent key leaves the pointer untouched (nil in a fresh struct).

### 3.3 Struct field mapping

Field key resolution, in order:

1. unexported → always skipped, tag or no tag
2. tag `bencode:"-"` (no comma) → skipped
3. tag with a name → that name is the key
4. tag with an empty name (`bencode:",omitempty"`) → **Go field name**, exact
5. no tag at all → **Go field name**, exact

Matching is **case-sensitive**, exact bytes. `"ip"` does not match field `IP`.

Tag grammar is `bencode:"name,opt1,opt2"`. The name is everything before the
first comma. `bencode:"-,"` means the literal key `-`.

Options are parsed and **ignored** by the decoder. `omitempty` in particular is
an encoder concern; it is accepted in the tag so struct definitions can be
shared with a future encoder, and it has no effect on decoding. Unknown options
are ignored silently.

#### Competing fields

When two fields resolve to the same key, the winner is decided by these rules,
in order — never by declaration order:

1. A **tagged** field beats an untagged one. Given `A string` tagged `"B"`
   alongside an untagged field `B`, the key `B` binds to `A`. This collision is
   easy to create by accident now that untagged fields fall back to their Go
   name, so it is worth resolving rather than reporting.
2. Otherwise the key is **ambiguous and binds to nothing**. Neither field is
   populated; the key is skipped exactly as an unknown key is.

This follows `encoding/json`'s `dominantField`, minus its first rule
(shallower embedding depth wins), which cannot apply here because embedded
fields are not flattened — every field sits at depth 0. Add that rule first if
flattening is ever adopted.

Picking a winner by position was rejected. Go resolves ambiguity by declaration
order nowhere: two embedded structs exposing the same field make the selector a
*compile error*. Order-based rules also fail quietly in the direction people
actually hit — under last-wins, appending a field silently steals a key from
one above it, and the older field just goes dark. An ambiguous key that binds
to nothing is order-independent, so reordering fields can never change
behaviour, and a permanently zero field is easier to notice than a silently
rebound one.

An ambiguous key is a programming error the decoder does not report.

Anonymous (embedded) struct fields are treated as ordinary fields — matched by
type name or tag, **not** flattened into the parent.

### 3.4 Reuse

Decoding into a non-zero destination:

| destination | behaviour |
|---|---|
| slice | replaced, not appended |
| map | replaced, not merged |
| struct | fields present in the input are overwritten; **fields absent from the input are left as they were** |

The struct rule means a reused destination can return a mix of new and stale
data. Callers who care must decode into a zero value.

---

## 4. Type mapping

`ErrTypeMismatch` for every combination not listed. Non-empty interface
destinations are always `ErrTypeMismatch`.

### bencode integer →

| destination | behaviour |
|---|---|
| `int`, `int8`…`int64` | set if it fits, else `ErrOverflow` |
| `uint`, `uint8`…`uint64` | negative → `ErrOverflow`; set if it fits, else `ErrOverflow` |
| `float32`, `float64` | set (precision loss is accepted) |
| `bool` | `i0e` → false, any other value → true |
| `any` | `int64` |
| `*T` | allocate, recurse |
| `RawMessage` | verbatim bytes (§9) |

Integers larger than `int64` are legal bencode but unrepresentable here →
`ErrOverflow`, not `ErrSyntax`.

### bencode string →

| destination | behaviour |
|---|---|
| `string` | set |
| `[]byte` | **fresh copy**; decoded slices never alias each other or the read buffer |
| `[N]byte` | length must equal `N` **exactly**, else `ErrTypeMismatch`; a short string must never partially fill the array |
| `any` | `string` — note that binary blobs like `pieces` arrive as strings and need `[]byte(s)` |
| `*T` | allocate, recurse |
| `RawMessage` | verbatim bytes (§9) |

`[N]byte` matters: a SHA-1 is `[20]byte` and arrives as a bencode string, not a
list.

### bencode list →

| destination | behaviour |
|---|---|
| `[]T` | replaced; an empty list yields a **non-nil, empty** slice |
| `[N]T` | fills `min(len, N)`; surplus elements are consumed and discarded; the tail stays zeroed |
| `any` | `[]any` |
| `*T` | allocate, recurse |
| `RawMessage` | verbatim bytes (§9) |

### bencode dict →

| destination | behaviour |
|---|---|
| struct | §3.3 |
| `map[string]T` | replaced; an empty dict yields a **non-nil, empty** map |
| `map[K]T`, `K` not string-kinded | `ErrTypeMismatch`, checked **before** any entry is decoded — never a panic |
| `any` | `map[string]any` |
| `*T` | allocate, recurse |
| `RawMessage` | verbatim bytes (§9) |

---

## 5. Errors

| sentinel | meaning |
|---|---|
| `ErrSyntax` | the bytes are not valid bencode |
| `ErrTypeMismatch` | the value is well-formed; the destination cannot hold it |
| `ErrOverflow` | the value parsed but does not fit the destination's width |
| `ErrInvalidDestination` | the argument to `Decode` is unusable (§3.1) |
| `ErrMaxDepth` | nesting exceeded the limit |
| `ErrExceedsMax` | a configured size limit was exceeded |

`strconv` errors, `reflect` errors and any other implementation detail must
**never** reach the caller unwrapped. Every returned error satisfies
`errors.Is` against exactly one sentinel above, or is an EOF per §5.2.

### 5.1 Sub-sentinels

`ErrLeadingZero`, `ErrNegativeZero` and `ErrEmpty` describe *how* the bytes were
malformed. They wrap `ErrSyntax`, so both of these hold:

```
errors.Is(err, ErrLeadingZero) == true
errors.Is(err, ErrSyntax)      == true
```

Callers classify on `ErrSyntax`; tests may assert the specific one.

### 5.2 EOF

| situation | error |
|---|---|
| stream ends cleanly at a value boundary, before `Decode` reads anything | `io.EOF` |
| stream ends part-way through a value | `io.ErrUnexpectedEOF` |

`io.EOF` is the only non-error error: it means "no more values", not "broken
input". Every truncation case (`i42`, `4`, `10:abc`, `l`, `li1e`, `d`,
`d3:foo`) is `io.ErrUnexpectedEOF`.

---

## 6. Stream contract

This is the invariant that keeps a decoder honest: a desynced decoder corrupts
every value after its first mistake.

| outcome | stream position | decoder afterwards |
|---|---|---|
| success | exactly after the value's last byte | usable |
| `ErrTypeMismatch` | exactly after the offending value's last byte | **usable** |
| `ErrOverflow` | exactly after the offending value's last byte | **usable** |
| `ErrInvalidDestination` | unmoved, zero bytes read | usable |
| `ErrSyntax` | undefined | **poisoned** |
| `ErrMaxDepth` | undefined | **poisoned** |
| `ErrExceedsMax` | undefined | **poisoned** |
| `io.ErrUnexpectedEOF` | undefined | **poisoned** |

So:

```go
d := NewDecoder(strings.NewReader("li1ee" + "i42e"))
d.Decode(&anInt)   // ErrTypeMismatch — the whole list is consumed
d.Decode(&next)    // OK → 42
```

**Poisoned** means sticky: the decoder stores the error and every later
`Decode` returns it without reading. A syntax error leaves the read position
mid-value with no way to find the next boundary, so pretending to recover would
be a lie.

**Mismatch consumes exactly one value.** Every mismatch path must skip the
value it rejected — including the string path, where the payload has already
been read and skipping again would eat the *next* value.

---

## 7. Limits

Hostile input is the normal case: a torrent client parses files and peer
messages from strangers. "It errors eventually" is not good enough; it must
error without spending the machine's memory or stack first.

Limits are **byte counts on the `Decoder`**, not digit counts. A digit cap is a
proxy that lies — `0000004:spam` and `999999:x` have wildly different meanings
for the same digit count.

| field | default | enforced |
|---|---|---|
| `MaxStringBytes` | 8 MiB | against the **declared** length, before any allocation |
| `MaxDepth` | 128 | on entry to each nested container |
| `MaxIntBytes` (internal) | 64 | on the scan for the `e` terminator |
| `MaxCaptureBytes` | 8 MiB | on the §9.6 retention window, as it grows |

Rules:

1. No allocation sized by input until the declared size passes its limit check.
2. Scans for a delimiter (`e`, `:`) are bounded. A scan that hits its bound
   returns `ErrExceedsMax`; it must never buffer to EOF looking for a byte that
   is not there.
3. **Skip paths obey the same limits.** Skipping is where limits get forgotten,
   and a limit that only applies to values you keep is not a limit.
4. Skipping never materialises the skipped value. An unknown key holding a
   large value must cost no allocation proportional to that value.
5. A value within `MaxIntBytes` but outside `int64` is `ErrOverflow` (aligned,
   recoverable); one beyond `MaxIntBytes` is `ErrExceedsMax` (poisoned).

---

## 8. Panics

The decoder never panics on any input, for any destination type. A panic is
not a rejection: callers cannot recover from one and a fuzz corpus cannot
distinguish it from a crash. Every reflect operation that can panic —
`SetMapIndex` with a mismatched key type, `Set` with an unassignable value —
must be guarded by a type check that returns `ErrTypeMismatch` first.

---

## 9. RawMessage — specified, not yet implemented

```go
type RawMessage []byte
```

### 9.1 Semantics

Decoding any value into a `RawMessage` yields the **verbatim bytes of the
complete value**: type prefix, length prefix and terminator included.

| input | `RawMessage` receives |
|---|---|
| `i4e` | `i4e` |
| `6:string` | `6:string` |
| `li4ee` | `li4ee` |
| `d6:lengthi7ee` | `d6:lengthi7ee` |

Content-only was rejected. Whole-value is what both callers need:

- **re-hashing** wants exactly the bytes that were on the wire
- **deferred decoding** wants something that is still a bencode value, so it
  can be fed back through a `Decoder` later

Content-only breaks deferred decoding for every type — `string` is not a
bencode value, neither is `4`, neither are a list's innards — and for strings
it would merely duplicate `[]byte`. The division of labour is:

> `[]byte` gives you the **content**. `RawMessage` gives you the **value**.

This is the same choice `encoding/json` makes: a `json.RawMessage` holds
`{"a":1}` with its braces and `"foo"` with its quotes.

### 9.2 Guarantees

1. **Round trip.** For any value `v` captured as `raw`, feeding `raw` to a
   fresh `Decoder` produces a decode identical to decoding `v` in place.
2. **Byte exactness.** `raw` is the input's bytes, not a re-encoding. §2
   leniency means key order and non-canonical encodings are *not* recoverable
   by re-encoding, which is the whole reason this type exists — a torrent's
   infohash is `sha1(raw bytes of the info dict)`.
3. **Ownership.** `RawMessage` never aliases the decoder's buffers. The caller
   may retain and mutate it freely.

### 9.3 Why it is specified before it is built

Capture is not a feature you can bolt on: it constrains how the decoder is
allowed to read. Writing it down now costs nothing and prevents a read path
that cannot support it.

### 9.4 Capture is skipping, with recording on

A list or dict carries no length prefix, so its extent is only knowable by
parsing it. The decoder already owns the machine that does this: `skipValue`
walks exactly one complete value and stops on its matching terminator, at any
nesting depth.

> **`RawMessage` = `skipValue()` with recording turned on.**

One code path for all four types. Lists and dicts are not special-cased; the
recursive walk finds the end for free.

### 9.5 Mechanism: offset-corrected tee

The decoder is a **stream** decoder (it must serve the peer wire protocol, not
only whole `.torrent` files), so buffering the entire input and subslicing it
is not available.

**Rejected: teeing the `io.Reader`.** A recorder wrapped around the source runs
on the *refill* clock — bytes pulled into `bufio`'s buffer — while the parser
runs on the *consume* clock. `bufio` reads ahead, so the two are separated by
an arbitrary gap that depends on buffer size, source chunking and, on a socket,
packet timing. Capture marks taken on the refill clock are meaningless, and
they fail non-deterministically: a plausible-looking but wrong hash.

**Adopted: record at the source, convert the clock.** `bufio.Reader.Buffered()`
reports how many bytes are read but not yet consumed, so at any instant

```
consumed = bytesPulledFromSource - br.Buffered()
```

is the parser's exact stream offset. A capture is then a pair of `consumed`
offsets taken before and after the walk of §9.4, and the payload is that range
of the recorded bytes.

Worked example — input `d4:infod6:lengthi7ee8:announce3:abce`, 36 bytes, all of
which `bufio` pulls on the first `ReadByte`:

| moment | pulled | `Buffered()` | `consumed` |
|---|---|---|---|
| capture start | 36 | 29 | **7** |
| capture end | 36 | 16 | **20** |

`recorded[7:20]` is `d6:lengthi7ee`. The correction stays exact when the source
delivers in unpredictable chunks, which is the case that matters on a socket.

**Why not record at every consumption site.** Appending inside `readSlice`,
`decodeString`, `skipString`, `skipInt`, `Discard` and `ReadFull` also works,
but correctness then depends on six call sites remembering to participate, and
on every future one doing the same. A missed site drops bytes silently: no
error, no panic, just a wrong hash. The offset scheme has a single chokepoint
that a new read path cannot bypass.

### 9.6 Requirements

- **Retention window.** Recorded bytes are kept only from the start of the
  outermost active capture; everything earlier is discarded. Peak retention is
  the size of the value being captured, and is subject to a limit of its own
  (§7).
- **Nesting is free.** Captures are `(start, end)` offset pairs into one
  window, not a stack of buffers, so an inner capture is a sub-range of the
  outer one and no byte can be counted twice.
- **Off by default.** With no `RawMessage` in the destination, nothing is
  recorded and nothing is retained.
- **Single-reader invariant.** The correction in §9.5 holds only while the
  decoder consumes exclusively through the one `bufio.Reader` it was
  constructed with. Wrapping, replacing or reading around that reader breaks
  capture. This invariant is part of the decoder's internal contract.
- **Errors.** A capture interrupted by an error is abandoned; there is nothing
  to unwind. Per §6 the failing error class decides whether the decoder
  survives at all.

### 9.7 Usage

```go
type Torrent struct {
    Announce string     `bencode:"announce"`
    Info     RawMessage `bencode:"info"`
}

// infohash comes from the bytes, never from a re-encoding
sum := sha1.Sum(t.Info)

// the same bytes decode again on demand (§9.2.1)
var info InfoDict
err := NewDecoder(bytes.NewReader(t.Info)).Decode(&info)
```

Note the two-step: §3.3 makes the first field claiming a key the winner, so a
struct cannot capture `info` raw *and* decode it into a typed field at the same
time. Capture once, decode from the bytes.

---

## 10. Open decisions

Recorded so they are not silently defaulted:

- **Floats.** bencode has no float type, and torrents contain none. Float
  destinations are supported today (§4) and harmless. Drop, or keep as a
  convenience?
- **Bool.** §4 says any nonzero integer is true, consistent with the lenient
  stance. The alternative — `i0e`/`i1e` only, everything else `ErrOverflow` —
  is defensible. `private` in a torrent is the only real user.
- **`MaxStringBytes` default.** 8 MiB is a guess. A torrent's `pieces` field is
  20 bytes per piece, so 8 MiB is ~400k pieces. Verify against real files.
- **Trailing data.** §2 leaves it in the buffer, which is right for a stream
  decoder and surprising for a one-shot `Unmarshal([]byte, any)` helper. If
  that helper is ever added, it should reject trailing bytes.
