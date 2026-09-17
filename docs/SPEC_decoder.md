# bencode_ref — decoder spec

Status: draft 3. This document is the authority. Tests assert what is written
here; when behaviour and spec disagree, the spec wins and the code is the bug.
Anything not stated here is undefined and must not be relied on.

Appendix A lists what changed from draft 1 and why; Appendix B does the same
for draft 2 → 3, whose single subject is embedded field lifting (§3.3.1).

---

## 1. Scope

A streaming bencode **decoder** that maps values onto Go destinations via
reflection. One `Decoder` reads successive values from one `io.Reader`.

Also in scope: `Unmarshal([]byte, any) error`, a one-shot helper over the same
machinery. It differs from `Decoder` in exactly one way — it **rejects trailing
bytes** after the value (§2), because a caller who handed over a finite byte
slice meant all of it. See §6.4.

Also in scope: two escape hatches for types the reflection rules cannot
express — a custom `Unmarshaler` interface and `encoding.TextUnmarshaler`,
including as map keys. See §10.

Non-goals for v1 (listed so their absence is a decision, not an oversight):

- encoding
- strict canonical-form validation
- case-insensitive field matching
- recovery from type errors (§6.2 explains why this is architectural, not
  merely unimplemented)

---

## 2. Accepted grammar

| type | form | rules |
|---|---|---|
| integer | `i<sign?><digits>e` | optional leading `-`; digits only; no leading zero (`i03e` invalid); no `-0`; `i0e` valid |
| string | `<len>:<bytes>` | `len` is digits only, no leading zero unless it is exactly `0`; `0:` valid; bytes are arbitrary, including NUL and invalid UTF-8 |
| list | `l<value>*e` | values are any bencode type |
| dict | `d(<string><value>)*e` | keys **must** be bencode strings |

A non-string dict key is malformed input, not a destination problem →
`ErrSyntax`. The decoder detects this from the first byte of the key, before
reading whatever that key turned out to be.

### 2.1 Leniency

The decoder is **lenient** about canonical form, because real `.torrent` files
in the wild violate it and a decoder that cannot open them is useless:

| input | behaviour |
|---|---|
| `d1:b1:x1:a1:ye` (keys unsorted) | accepted |
| `d1:a1:x1:a1:ye` (duplicate key) | accepted, last occurrence wins |
| `i42egarbage` (trailing bytes) | accepted by `Decoder`, `42` returned; trailing bytes remain for the next `Decode`. Rejected by `Unmarshal` (§6.4) |

Corollary: an accepted input is **not** guaranteed to re-encode
byte-identically. This is why §9 exists.

---

## 3. Destinations

### 3.1 Entry point

`Decode(v any)` requires `v` to be a **non-nil pointer**. Anything else —
non-pointer, typed nil pointer, untyped nil — is `ErrInvalidDestination`,
returned **before any byte is read**.

`ErrInvalidDestination` is reachable **only** from this check. Inside the
decoder every destination is, by construction, addressable and settable, so
internal `CanSet` guards are assertions, not error paths.

> **This invariant is not self-standing.** It holds only because §3.3 rule 1
> keeps every unexported field out of the field map *as a destination*, and
> because §3.3.1 prunes the one traversal that would reach a read-only value
> anyway. Binding a key to an unexported field puts a read-only
> `reflect.Value` into the decode path, turns those assertions into reachable
> code, and produces a panic in the container paths that call `Set` on the
> whole value (§8).
>
> Since draft 3 the decoder *walks through* unexported embedded structs to
> reach the exported fields inside them (§3.3.1), so "no unexported field is
> ever touched" is no longer the mechanism. The precise mechanism is
> `reflect`'s own distinction: the read-only marker an unexported **embedded**
> field carries is not inherited by its fields, while the one an unexported
> **named** field carries is. Only anonymous fields are ever descended into,
> so every value the decoder binds is still settable. §3.3.1's prune list, §8's
> table and this paragraph must be changed together or not at all.

### 3.2 Pointers

A pointer destination at **any** depth is allocated on demand and decoded
through. This applies recursively (`**T` works).

Since bencode has no null, the rule is exact:

> a pointer field is non-nil **iff** the key was present in the input **and
> bound to that field**.

A key that is present but ambiguous (§3.3) binds to nothing and therefore
allocates nothing. An absent key leaves the pointer untouched (nil in a fresh
struct).

**Embedded pointers are the one place a pointer becomes non-nil without a key
of its own.** An embedded `*Base` is allocated when — and only when — some key
binds to a field promoted out of it (§3.3.1). The rule above still holds if
read as "the key that reached this pointer", one level down: no promoted key
present means the embedded pointer stays nil.

### 3.3 Struct field mapping

A struct type resolves to a map from **key** to **index path** — a `[]int`
naming a field, possibly reached through one or more embedded structs (§3.3.1).
For a plain struct every path has length 1.

Per-field key resolution, in order:

1. unexported → skipped as a destination, tag or no tag. Unexported **embedded
   struct** fields are the sole exception and are not destinations either: they
   are *walked through* by §3.3.1, never bound to
2. tag `bencode:"-"` (no comma) → skipped, and if the field is embedded its
   contents are not lifted
3. tag with a name → that name is the key, and the field counts as **tagged**.
   This applies to embedded fields too, and a tagged embedded field is bound as
   an ordinary field rather than lifted (§3.3.1)
4. tag with an empty name (`bencode:",omitempty"`) → **Go field name**, exact,
   and the field counts as **untagged** for §3.3.2
5. no tag at all → **Go field name**, exact, untagged

An embedded field's "Go field name" is its type's name without package
qualifier: embedded `bencode.RawMessage` is named `RawMessage`.

Matching is **case-sensitive**, exact bytes. `"ip"` does not match field `IP`.

Tag grammar is `bencode:"name,opt1,opt2"`. The name is everything before the
first comma. `bencode:"-,"` means the literal key `-`.

Options are parsed and **ignored** by the decoder. `omitempty` in particular is
an encoder concern; it is accepted in the tag so struct definitions can be
shared with a future encoder, and it has no effect on decoding. Unknown options
are ignored silently.

Note the consequence of rules 3 and 4 together: supplying a tag does not by
itself make a field tagged. Only a tag that supplies a *name* does. This
matches `encoding/json`, where the dominance test is on the resolved name being
non-empty, not on the tag's presence.

#### 3.3.1 Embedded fields are lifted

An **untagged embedded struct field is not a destination**; its fields are
promoted into the parent's key space as if they had been declared there.

```go
type Common struct {
    Announce string `bencode:"announce"`
}

type Torrent struct {
    Common                  // lifted
    Comment string `bencode:"comment"`
}
// keys: "announce" -> {0, 0}, "comment" -> {1}
```

This is what embedding means in Go — `t.Announce` compiles — and a decoder
whose key space disagrees with the selector space makes shared field sets
useless, which is the entire reason to embed.

**What is lifted.** A field is walked into when it is anonymous, its type is a
struct or a pointer to a struct, and it resolved to no explicit tag name
(§3.3 rules 2–3). Everything else anonymous is an ordinary field, keyed and
competing per §3.3:

| anonymous field | treatment |
|---|---|
| `Common` (exported struct) | lifted |
| `*Common` (exported pointer to struct) | lifted; allocated on demand (§3.2) |
| `common` (unexported struct) | lifted — see below |
| `*common` (unexported pointer to struct) | **pruned**: neither lifted nor bound |
| `Common` with `bencode:"c"` | ordinary field, key `c`, tagged, **not** lifted |
| `Common` with `bencode:"-"` | skipped entirely |
| `MyInt` (exported, non-struct) | ordinary field, key `MyInt` |
| `myInt` (unexported, non-struct) | skipped (§3.3 rule 1) |

An untagged embedded struct therefore loses the key it had in draft 2 — `Common`
above is no longer reachable under the key `Common`. Tag it to get that back.

A **tagged unexported** embedded struct (`common` with `bencode:"c"`) is
invisible: the tag asks for it to be bound, rule 1 forbids binding it, and
asking for a key is not a request to lift. Nothing under it is reachable.
Remove the tag to lift it, or export the type to bind it.

**Unexported embedded structs are walked, not bound.** `reflect` marks the
embedded field itself read-only but does **not** propagate that marker to its
fields, so `T.common.Name` is settable even though `T.common` is not. Lifting
out of an unexported struct is the standard mixin shape and `encoding/json`
supports it, so it is supported here.

`*common` is pruned for the mirror-image reason: reaching a field under it
means allocating the pointer, and the pointer *is* read-only, so the `Set`
would panic. The prune is done when the field map is built, not when a key
arrives, which makes it a static property of the type rather than an error
path that depends on the input. The keys underneath simply bind to nothing,
exactly as an ambiguous key does (§3.3.2) — the same treatment for the same
reason: the destination type is a programming error the decoder does not
report. (`encoding/json` instead defers this to decode time and returns an
error there; a build-time prune is cheaper, cacheable, and cannot desync a
stream.)

**The walk.** Keys are gathered breadth-first: all of depth 0, then all of
depth 1, and so on. Breadth-first is what makes "shallowest wins" (§3.3.2)
decidable without comparing every pair. A struct type is visited at most once
per depth level, so recursive shapes (`type Node struct { *Node }`) terminate.

The field-tree walk is a property of the destination *type*, not of the input.
It is unrelated to `MaxDepth` (§7), which bounds nesting in the byte stream; no
input can make this walk deeper.

**Decoding through a path.** When a key binds to a path of length > 1, the
decoder walks the path field by field, allocating any nil embedded pointer it
passes through, then decodes into the leaf. Consequences:

- a non-nil embedded pointer is reused, not replaced (§3.4)
- allocation happens on the way to a value, so a value that then fails to
  decode leaves the embedded pointer allocated. The decoder is poisoned
  (§6.1), so this is not observable through any further `Decode`
- the promoted field is the destination for §4 in every respect; embedding
  changes which `reflect.Value` is reached, nothing about what may be stored
  in it

#### 3.3.2 Competing fields

When two or more fields resolve to the same key, the winner is decided by these
rules, in order — never by declaration order:

1. **Shallowest wins.** A candidate with a shorter index path beats every
   deeper one outright, tagged or not. A field declared on the struct itself
   always beats one promoted out of an embedded struct — which is exactly what
   Go does with the selector `t.Name`, and the reason an outer field is said to
   *shadow* an inner one.
2. Among the candidates tied at that shallowest depth: if **exactly one** is
   tagged, it wins, regardless of how many untagged candidates it beats. Given
   `A string` tagged `"B"` alongside an untagged field `B`, the key `B` binds
   to `A`.
3. Otherwise — zero tagged candidates at that depth, or two or more — the key
   is **ambiguous and binds to nothing**. No candidate is populated; the key is
   skipped exactly as an unknown key is.

Two embedded structs both exposing `Name` are tied at depth 1 with no tag, so
`Name` binds to neither — the decoder's echo of the compile error Go gives for
the same selector. Tagging one of them resolves it, under rule 2.

These are `encoding/json`'s `dominantField` rules, now including the depth rule
draft 2 had to omit.

Picking a winner by position was rejected. Go resolves ambiguity by declaration
order nowhere: two embedded structs exposing the same field make the selector a
*compile error*. Order-based rules also fail quietly in the direction people
actually hit — under last-wins, appending a field silently steals a key from
one above it, and the older field just goes dark. An ambiguous key that binds
to nothing is order-independent, so reordering fields can never change
behaviour, and a permanently zero field is easier to notice than a silently
rebound one.

An ambiguous key is a programming error the decoder does not report.

#### 3.3.3 Field map caching

The field map is a pure function of `reflect.Type` and is cached process-wide
for the life of the program, keyed by type. A cached map is published
immutable and never edited in place; this is what makes it safe to share
across goroutines without locking (§11.1).

Immutability now covers the index paths as well: a `[]int` stored in the map is
never appended to or reused as scratch during a later walk. A path slice handed
out to a decode is shared by every goroutine decoding that type.

### 3.4 Reuse

Decoding into a non-zero destination:

| destination | behaviour |
|---|---|
| slice | replaced, not appended |
| array | replaced wholesale; §4 requires an exact length match, so every element is overwritten and no stale tail can survive |
| map | replaced, not merged |
| struct | fields present in the input are overwritten; **fields absent from the input are left as they were** |
| embedded pointer | reused if non-nil; allocated only if some promoted key arrives (§3.2, §3.3.1) |

The struct rule means a reused destination can return a mix of new and stale
data. Callers who care must decode into a zero value.

The embedded-pointer row follows from the struct row one level down: a
promoted field is an ordinary field of the embedded struct, so decoding into a
reused destination overwrites exactly the promoted keys the input carried and
leaves the rest of the embedded struct alone.

---

## 4. Type mapping

`ErrTypeMismatch` for every combination not listed. Non-empty interface
destinations are always `ErrTypeMismatch`.

A destination implementing one of §10's interfaces is handled there and never
reaches these tables. The tables are what a type gets when it stays silent.

### bencode integer →

| destination | behaviour |
|---|---|
| `int`, `int8`…`int64` | set if it fits, else `ErrOverflow` |
| `uint`, `uint8`…`uint64` | negative → `ErrOverflow`; set if it fits, else `ErrOverflow` |
| `float32`, `float64` | set (precision loss is accepted) |
| `bool` | `i0e` → false, any other value → true; the destination is **always assigned**, never left as it was |
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
| `[N]byte` | length must equal `N` **exactly**, else `ErrArrayLength` |
| `any` | `string` — note that binary blobs like `pieces` arrive as strings and need `[]byte(s)` |
| `*T` | allocate, recurse |
| `RawMessage` | verbatim bytes (§9) |

`[N]byte` matters: a SHA-1 is `[20]byte` and arrives as a bencode string, not a
list.

### bencode list →

| destination | behaviour |
|---|---|
| `[]T` | replaced; an empty list yields a **non-nil, empty** slice |
| `[N]T` | list length must equal `N` **exactly**, else `ErrArrayLength` |
| `any` | `[]any` |
| `*T` | allocate, recurse |
| `RawMessage` | verbatim bytes (§9) |

### bencode dict →

| destination | behaviour |
|---|---|
| struct | §3.3 |
| `map[string]T` | replaced; an empty dict yields a **non-nil, empty** map |
| `map[K]T` where `K` is a string **kind** | supported, **including named string types** (`type Key string`); keys are converted to `K` |
| `map[K]T` where `*K` implements `encoding.TextUnmarshaler` | supported whatever `K`'s kind; the key bytes go through `UnmarshalText` (§10.4) |
| `map[K]T`, `K` neither of the above | `ErrTypeMismatch`, checked **before** the map is allocated and before any entry is decoded — never a panic |
| `any` | `map[string]any` |
| `*T` | allocate, recurse |
| `RawMessage` | verbatim bytes (§9) |

### 4.1 Why arrays are strict

Both array cases — `[N]byte` from a string, `[N]T` from a list — demand an
exact length. This departs from `encoding/json`, which silently discards
surplus list elements and zeroes the tail of a short one.

An array destination is a statement that the value has exactly `N` elements.
For a string it is almost always a fixed-width identifier — a SHA-1, a peer ID,
an infohash. A partially filled one is not a degraded identifier, it is a
*different* identifier that will compare unequal to the right one and produce a
failure far from its cause.

The same reasoning carries to lists. A caller who wants "at most N of these"
has `[]T` and can check `len` themselves; a caller who reaches for `[N]T` is
asserting a shape, and a mismatch means the input is not what they think it is.
Silent truncation converts that into a bug that surfaces later, somewhere else.
Rejecting keeps the failure at the point where the assumption broke.

---

## 5. Errors

### 5.1 Sentinels

| sentinel | meaning |
|---|---|
| `ErrSyntax` | the bytes are not valid bencode |
| `ErrTypeMismatch` | the value is well-formed; the destination cannot hold it |
| `ErrOverflow` | the value parsed but does not fit the destination's width |
| `ErrInvalidDestination` | the argument to `Decode` is unusable (§3.1) |
| `ErrMaxDepth` | nesting exceeded the limit |
| `ErrExceedsMax` | a configured size limit was exceeded |

Sub-sentinels describe a more specific cause and wrap a sentinel above:

| sub-sentinel | wraps | meaning |
|---|---|---|
| `ErrLeadingZero` | `ErrSyntax` | `i03e`, `03:abc` |
| `ErrNegativeZero` | `ErrSyntax` | `i-0e` |
| `ErrEmpty` | `ErrSyntax` | `ie`, `:abc` |
| `ErrArrayLength` | `ErrTypeMismatch` | string length ≠ `[N]byte` length; list length ≠ `[N]T` length |

So both of these hold:

```
errors.Is(err, ErrLeadingZero) == true
errors.Is(err, ErrSyntax)      == true
```

Callers classify on the sentinel; tests may assert the sub-sentinel.

`strconv` errors, `reflect` errors and any other implementation detail must
**never** reach the caller unwrapped. Every returned error satisfies
`errors.Is` against exactly one sentinel above, or is an EOF per §5.3.

### 5.2 Structured errors

Sentinels classify; they do not locate. Every error the decoder returns —
other than `ErrInvalidDestination`, which is raised before reading — is a
**pointer to a struct carrying position and context**, whose `Unwrap` returns
the most specific applicable sentinel.

```go
type SyntaxError struct {
    Offset int64  // byte offset of the byte that could not be parsed
    msg    string
    cause  error  // ErrSyntax or a sub-sentinel of it
}

type TypeError struct {
    Offset int64        // byte offset of the first byte of the offending value
    Value  string       // "integer", "string", "list", "dict"
    Type   reflect.Type // destination type
    Struct string       // struct type name, if the value was a struct field
    Field  string       // field key, if the value was a struct field
    cause  error        // ErrTypeMismatch, ErrArrayLength or ErrOverflow
}

type LimitError struct {
    Offset int64  // byte offset at which the limit was exceeded
    Limit  string // "MaxStringBytes", "MaxDepth", "MaxValueBytes", ...
    Value  int64  // the value that exceeded it, where meaningful
    cause  error  // ErrExceedsMax or ErrMaxDepth
}
```

Each implements `Error() string` and `Unwrap() error`. Classification is
unchanged — `errors.Is(err, ErrSyntax)` works exactly as in §5.1 — and
`errors.As` now yields the detail. This is the current idiomatic split:
sentinels for control flow, types for diagnosis. `encoding/json` does the same
with `SyntaxError` and `UnmarshalTypeError`.

**Offsets are counted from the first byte the `Decoder` ever read**, not from
the start of the current value, and are zero-based. They come free from the
consumed-offset counter of §9.5, which is maintained unconditionally.

`Struct` and `Field` are best-effort: populated when the failing value was
being decoded into a struct field, empty otherwise.

For a **promoted** field (§3.3.1) `Struct` names the type that *declares* the
field, not the outer type being decoded, and `Field` is the declared Go field
name. `Common.Announce`, not `Torrent.Announce`: the outer type is findable
from the call site, while the declaring type is the one the reader has to go
looking for.

### 5.3 EOF

| situation | error |
|---|---|
| stream ends cleanly at a value boundary, before `Decode` reads anything | `io.EOF` |
| stream ends part-way through a value | `io.ErrUnexpectedEOF` |

`io.EOF` is the only non-error error: it means "no more values", not "broken
input". It is returned bare — not wrapped in a `SyntaxError` — and is
**sticky**: once a `Decoder` has returned `io.EOF` it returns `io.EOF` from
every later `Decode`. This is what makes the standard loop terminate:

```go
for {
    var v any
    err := d.Decode(&v)
    if err == io.EOF {
        break
    }
    if err != nil {
        return err
    }
    use(v)
}
```

Every truncation case (`i42`, `4`, `10:abc`, `l`, `li1e`, `d`, `d3:foo`) is
`io.ErrUnexpectedEOF`, wrapped in a `SyntaxError`, and poisons the decoder per
§6.

---

## 6. Stream contract

This is the invariant that keeps a decoder honest: a desynced decoder corrupts
every value after its first mistake.

### 6.1 The contract

| outcome | stream position | decoder afterwards |
|---|---|---|
| success | exactly after the value's last byte | usable |
| `ErrInvalidDestination` | unmoved, zero bytes read | usable |
| `io.EOF` | at end of stream | exhausted; returns `io.EOF` forever |
| **any other error** | **undefined** | **poisoned** |

**Poisoned** means sticky: the decoder stores the error and every later
`Decode` returns that same error without reading a byte. There is no `Reset`,
no `Sync`, no way back.

`ErrInvalidDestination` is the sole exception because it is raised before the
reader is touched — the caller's argument was bad, the stream is untouched, and
the obvious fix is to call again with a usable destination.

### 6.2 Why type errors poison too, unlike `encoding/json`

This is a deliberate divergence, and it follows from architecture rather than
taste.

`json.Decoder` is **two-phase**. `readValue` runs a scanner over the input to
find the extent of one complete value and buffers it; the scanner works purely
from the grammar and knows nothing about the destination. Only then does
`decodeState` walk that buffer into the destination. An `UnmarshalTypeError`
is therefore raised at a moment when the stream has *already* advanced past the
whole value. `json` goes further still: it stores the error in `savedError`,
keeps populating the remaining fields, and returns the error at the end.

This decoder is **single-pass**: parsing and destination-filling happen in the
same walk. A type error on the third pair of a ten-pair dict leaves seven pairs
and a terminator unread. The draft 1 promise that "mismatch consumes exactly
one value" was only true when the mismatch occurred at the top level of a
value; nested mismatches left the stream desynced while claiming otherwise.

Recovery is implementable — every container level would catch a recoverable
error from a child, drain its own remainder with `skipValue`, and only then
propagate — but that is a branch and a test class on every container iteration,
paid on the success path to serve an error path. For a decoder whose main job
is parsing whole `.torrent` files and framed peer messages, it is not worth it.

Consequence for the implementation: the mismatch branches in `decodeDict` and
`decodeList` must **not** call `skipValue` to realign. That call existed only
to honour a guarantee that no longer exists, and keeping it would suggest a
recovery that is not offered. Likewise, no returned error may be a
`errors.Join` of two sentinels — §5.1 requires exactly one.

### 6.3 What this costs the caller

A caller who genuinely wants to skip a badly-typed value can still do it, by
decoding into `any` or `RawMessage` first and inspecting the result. That path
never produces a type error, so it never poisons.

### 6.4 `Unmarshal`

```go
func Unmarshal(data []byte, v any) error
```

Decodes exactly one value from `data` into `v`. After the value, any remaining
byte — including whitespace, which bencode does not permit anywhere — is
`ErrSyntax`, with the offset of the first trailing byte.

This is the one place the leniency of §2.1 is withdrawn, because the input is
finite and fully known: a caller passing a byte slice asserted that the slice
is the message.

---

## 7. Limits

Hostile input is the normal case: a torrent client parses files and peer
messages from strangers. "It errors eventually" is not good enough; it must
error without spending the machine's memory or stack first.

Limits are **byte counts**, not digit counts. A digit cap is a proxy that lies
— `0000004:spam` and `999999:x` have wildly different meanings for the same
digit count.

### 7.1 Configuration

```go
type Limits struct {
    MaxStringBytes  int64 // single bencode string
    MaxValueBytes   int64 // one whole top-level value
    MaxCaptureBytes int64 // §9.6 capture span
    MaxDepth        uint  // nesting
}

type Decoder struct {
    Limits Limits // zero fields mean the defaults below
    // ...
}
```

`Limits` is read at the **start of each `Decode`**. Changing it between calls
is supported and well-defined; changing it during a call is not possible from
a correct program (§11.1). A zero field means "use the default", so a partial
override needs no constructor variant.

`MaxDepth` is a `uint` because a negative nesting limit has no meaning: the
only values it could carry are ones the decoder would have to reject, and a
type that cannot express them is cheaper than a check that has to.

| field | default | enforced |
|---|---|---|
| `MaxStringBytes` | 8 MiB | against the **declared** length, before any allocation |
| `MaxValueBytes` | 16 MiB | total bytes consumed by the current top-level value, checked as it grows |
| `MaxDepth` | 128 | on entry to each nested container |
| `MaxCaptureBytes` | 8 MiB | on `consumed - captureStart`, at the same chokepoints as `MaxValueBytes` (§9.6) |
| `maxIntBytes` (internal, not configurable) | 64 | on the scan for the `e` terminator |

### 7.2 Rules

1. No allocation sized by input until the declared size passes its limit check.
2. Scans for a delimiter (`e`, `:`) are bounded. A scan that hits its bound
   returns `ErrExceedsMax`; it must never buffer to EOF looking for a byte that
   is not there.
3. **Every limit applies to the skip path exactly as to the decode path.**
   Skipping is where limits get forgotten, and a limit that only applies to
   values you keep is not a limit.
4. Skipping never materialises the skipped value. An unknown key holding a
   large value must cost no allocation proportional to that value.
5. A value within `maxIntBytes` but outside `int64` is `ErrOverflow`; one
   beyond `maxIntBytes` is `ErrExceedsMax`. Both poison the decoder (§6.1);
   the distinction is diagnostic, not behavioural.

### 7.3 Why `MaxValueBytes` exists

Depth and string length together do not bound anything that grows by
*repetition*. The input `l` followed by a million `i0e` passes every other
check: depth 2, every integer three bytes long, every string absent. The
destination slice still grows linearly with the input, and for a socket source
the input has no end.

`MaxValueBytes` closes this with one counter, and the counter already exists —
it is the `consumed` offset of §9.5. It bounds element counts, node counts in
deeply-repetitive trees, and anything else proportional to input length,
without a separate limit for each shape.

### 7.4 Memory amplification is bounded, not eliminated

`MaxValueBytes` bounds the *input*, not the memory the destination occupies,
and the ratio is not 1. Decoding `li0ei0e…e` into `[]any` costs roughly 24
bytes of heap per 3 bytes of input: 16 for the slice element, 8 for the boxed
`int64`. At the default that is on the order of 128 MiB of live memory for
16 MiB of input.

This is a constant factor, so the input limit does its job. But callers reading
from untrusted sockets should lower `MaxValueBytes` to something matched to
their protocol's real messages rather than trusting the default, and this
paragraph exists so that choice is informed.

---

## 8. Panics

The decoder never panics on any input, for any destination type. A panic is
not a rejection: callers cannot recover from one and a fuzz corpus cannot
distinguish it from a crash. Every reflect operation that can panic —
`SetMapIndex` with a mismatched key type, `Set` with an unassignable value,
`Set` on a value obtained through an unexported field — must be guarded by a
check that returns an error first.

The known panic sources, each closed by a rule elsewhere in this document, are
recorded here because they are the ones that will come back:

| panic | closed by |
|---|---|
| `SetMapIndex` with `string` into a named-string-keyed map | §4, conversion to `K` |
| `Set` on a value reached through an unexported **named** field | §3.3 rule 1 — only anonymous fields are descended into |
| `Set` on an embedded pointer to an unexported struct (`*common`) | §3.3.1, pruned when the field map is built |
| unbounded recursion building the field map for a recursive type | §3.3.1, one visit per type per depth level |
| single-value interface assert when probing for §10's interfaces | §10.2 — the probe asserts to the interface the call site asked for, comma-ok, and never force-casts the result of one probe into the other interface |
| `UnmarshalText` on a map key obtained by conversion | §10.4, the key is allocated with `reflect.New` so it is addressable |

The second row is the one that changed in draft 3 and the one most likely to be
re-broken. Lifting out of an unexported embedded struct is safe, and looks
almost identical in code to binding an unexported field, which is not. The
distinction — an unexported *embedded* field's read-only marker is not
inherited by its fields, an unexported *named* field's is — lives in `reflect`,
not in this decoder, so a test must hold it: decoding into a struct with an
unexported embedded struct must populate the promoted field, and a struct with
an unexported named struct field must leave it untouched.

A fuzz target that decodes arbitrary bytes into a fixture struct containing
every destination shape in §4 is the standing test for this section. Draft 3
extends the fixture with each row of the §3.3.1 table, including `*common` and
a self-referential embedded pointer.

---

## 9. RawMessage

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
2. **Byte exactness.** `raw` is the input's bytes, not a re-encoding. §2.1
   leniency means key order and non-canonical encodings are *not* recoverable
   by re-encoding, which is the whole reason this type exists — a torrent's
   infohash is `sha1(raw bytes of the info dict)`.
3. **Ownership.** `RawMessage` never aliases the decoder's buffers. The caller
   may retain and mutate it freely.

### 9.3 Capture is skipping, with recording on

A list or dict carries no length prefix, so its extent is only knowable by
parsing it. The decoder already owns the machine that does this: `skipValue`
walks exactly one complete value and stops on its matching terminator, at any
nesting depth.

> **`RawMessage` = `skipValue()` with recording turned on.**

One code path for all four types. Lists and dicts are not special-cased; the
recursive walk finds the end for free.

### 9.4 Mechanism: offset-corrected tee

The decoder is a **stream** decoder (it must serve the peer wire protocol, not
only whole `.torrent` files), so buffering the entire input and subslicing it
is not available.

**Rejected: teeing the `io.Reader` and taking marks from it.** A recorder
wrapped around the source runs on the *refill* clock — bytes pulled into
`bufio`'s buffer — while the parser runs on the *consume* clock. `bufio` reads
ahead, so the two are separated by an arbitrary gap that depends on buffer
size, source chunking and, on a socket, packet timing. Capture marks taken on
the refill clock are meaningless, and they fail non-deterministically: a
plausible-looking but wrong hash.

**Adopted: record at the source, convert the clock.** `bufio.Reader.Buffered()`
reports how many bytes are read but not yet consumed, so at any instant

```
consumed = bytesPulledFromSource - br.Buffered()
```

is the parser's exact stream offset. A capture is then a pair of `consumed`
offsets taken before and after the walk of §9.3, and the payload is that range
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

**The offset clock is always on.** `consumed` is maintained on every read
whether or not a capture is active, because §5.2 needs it for error offsets and
§7 needs it for `MaxValueBytes`. Only *recording* — the retention of bytes — is
conditional.

### 9.5 The decoder owns its buffer

The mechanism above requires the recorder to sit **under** the `bufio.Reader`,
and requires access to that reader's `Buffered()`. Neither is possible if the
decoder adopts a `*bufio.Reader` handed in by the caller: the source is already
behind someone else's buffer, and bytes may already have been pulled into it.

Therefore:

```go
func NewDecoder(r io.Reader) *Decoder {
    rec := &recorder{src: r}
    return &Decoder{src: rec, br: bufio.NewReader(rec)}
}
```

**`NewDecoder` always wraps.** The draft 1 fast path — adopt the reader
directly when it already satisfies the internal `reader` interface — is
removed. It looked like a free optimisation and was in fact incompatible with
§9: it would have produced captures that are silently wrong for exactly the
inputs where correctness matters most.

The cost is one extra layer of buffering when the caller passes a
`*bufio.Reader`. That is a memory copy. A wrong infohash is a torrent that
never downloads and a bug that takes a day to find.

### 9.6 Requirements

- **Retention window.** Recorded bytes are kept only from the start of the
  outermost active capture; everything earlier is discarded. Peak retention is
  the size of the value being captured, and is subject to `MaxCaptureBytes`
  (§7.1, enforced as §9.6.1 describes).
- **Nesting is free.** Captures are `(start, end)` offset pairs into one
  window, not a stack of buffers, so an inner capture is a sub-range of the
  outer one and no byte can be counted twice. Note that the §9.3 walk never
  decodes, so a capture cannot currently open inside another one: captures are
  sequential in practice, and this bullet is a property of the design rather
  than a case the decoder reaches.
- **Recording is off by default.** With no `RawMessage` in the destination,
  nothing is retained. The offset counter still runs (§9.4).
- **Single-reader invariant.** The correction in §9.4 holds only while the
  decoder consumes exclusively through the one `bufio.Reader` it constructed.
  Wrapping, replacing or reading around that reader breaks capture. §9.5 is
  what makes this invariant enforceable rather than merely requested.
- **Errors.** A capture interrupted by an error is abandoned; there is nothing
  to unwind, because per §6.1 the decoder is poisoned and will produce no
  further values. The walk's error is returned **as it stands** — it is not
  re-wrapped as a `TypeError` against `RawMessage`. Per §9.1 every value is a
  valid capture, so "this value does not fit this destination" is a category
  that cannot arise here; a `TypeError` would name the destination for a fault
  the destination did not have, carry an empty `Value` because there is no
  offending value, and hide the `*SyntaxError` that holds the offending byte
  and its offset (§5.2).
- **Capture does not get its own EOF rules.** §5.3 is classified by position in
  the input, not by destination: a stream that ends at a value boundary is
  `io.EOF`, one that ends inside a container is `io.ErrUnexpectedEOF`. The
  capture branch is entered *before* the type byte is read, so it bypasses the
  classification the normal path applies and has to repeat it. Getting this
  wrong reports a truncated file as a clean end of stream, and the caller's
  drain loop (§5.3) then `break`s on corrupt input and swallows the error.

### 9.6.1 Enforcing `MaxCaptureBytes`

The limit is checked on the **offset span** of the active capture,
`consumed - captureStart`, at the two chokepoints where `MaxValueBytes` is
already checked: on entry to each value of the §9.3 walk, and — against the
*declared* length, before any byte is pulled — in the string case. It is the
same kind of check as `MaxValueBytes` with a different origin, and it is
enforced entirely in the parser.

**Rejected: returning the limit error from the recorder's `Read`.** Refusing to
append past the limit and returning an error from the recorder looks like the
tightest possible bound — the byte that would breach the window is the byte
that fails. It does not work, for the same reason as §9.4: `bufio` sits between
the recorder and the parser and owns that error, storing it and surfacing it
only when its own buffer drains. A capture that completes from bytes already
buffered never sees it. The failure is then not even a limit error: the parser
finishes the walk, asks for a window the recorder declined to record, and gets
an out-of-bounds internal error instead. Like §9.4, it fails as a function of
source chunking, so it passes for byte-at-a-time inputs and fails for whole
ones.

The consequence of checking at chokepoints rather than at every append is
**bounded slack**: between two checks the window can overrun the limit by at
most the parser's read-ahead — one `bufio` refill — plus the prefix of one
value. That overshoot is a constant, not a function of input length, which is
all a DoS bound requires. The slack is deliberate, not an oversight: exactness
would take an append-time check, and the paragraph above is why the recorder
cannot be the thing that reports. Tightening the bound is not worth
reintroducing an error path that fails by chunking.

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

The two-step is not a stylistic preference. A struct cannot capture `info` raw
*and* decode it into a typed field at the same time: two fields tagged
`bencode:"info"` are two tagged candidates for one key, which §3.3.2 rule 3
makes **ambiguous**, binding the key to neither. Both fields would come back
zero. Capture once, decode from the bytes.

Embedding does not provide a way around this either: two tagged candidates at
*different* depths are resolved by §3.3.2 rule 1, so the shallower one simply
shadows the deeper, and only one of the two fields is ever populated.

---

## 10. Custom unmarshaling

```go
type Unmarshaler interface {
	UnmarshalBencode([]byte) error
}
```

together with `encoding.TextUnmarshaler` from the standard library.

### 10.1 The two interfaces are not the same interface

| interface | receives | fires on |
|---|---|---|
| `Unmarshaler` | the **verbatim bytes of the complete value**, §9.1 semantics: type prefix, length prefix and terminator included | any bencode type |
| `encoding.TextUnmarshaler` | the string's **content**, length prefix stripped | bencode **string** only |

The division of labour is §9.1's, one level up:

> `TextUnmarshaler` gets the **content**. `Unmarshaler` gets the **value**.

Both are kept because each covers what the other cannot. Only `Unmarshaler`
can bind a non-string: a `time.Duration` from `i1234567890e`, a `pieces` string
split into `[][20]byte`, a DHT `nodes` list turned into `[]net.Addr`. Only
`TextUnmarshaler` gets the types that already implement it — `net.IP`,
`netip.Addr`, `time.Time`, `big.Int` — working with no method written in this
package's terms at all, and it is the only route to a non-string map key
(§10.4).

A `TextUnmarshaler` destination facing a non-string value is **not** special
cased: §4 applies unchanged. An integer or a list into a `netip.Addr` is
`ErrTypeMismatch` — the contract is text, and a list is not text.

A **dict** is the exception, and not a deliberate one. §4 maps a dict onto a
struct destination by §3.3, and `netip.Addr`, `time.Time` and `big.Int` are all
structs, so a dict binds against their fields, matches nothing exported, and
reports success while leaving the destination zero. §12 records this.

bencode strings are arbitrary byte strings and need not be UTF-8 (§2). They are
handed to `UnmarshalText` unchanged rather than validated or replaced. The
interface is named for text and this decoder feeds it bytes; `encoding/json`
has the same wart and it is not worth a second interface to fix.

### 10.2 Precedence, and how the method is found

Checked in order, first match wins:

1. `RawMessage` (§9)
2. `Unmarshaler`
3. `encoding.TextUnmarshaler` — only once the value is known to be a string
4. the type mapping of §4

`RawMessage` declares neither method, so its position is an ordering statement
rather than a conflict rule. `Unmarshaler` outranks `TextUnmarshaler` because
it is the strictly more informed of the two: it sees the value's type, and a
type that implements both has said it can handle every case.

Positions 2 and 3 differ in *where* they are checked. `Unmarshaler` is checked
on entry to a value, before the first byte is read, because it consumes the
value whole. `TextUnmarshaler` is checked inside the string path, after the
length prefix and content have been read.

**Lookup uses the addressable form of the destination.** Both methods must
mutate their receiver, so both are declared on `*T` far more often than on `T`,
and `T`'s method set does not contain them. The decoder takes the destination's
address before probing whenever it is not already a pointer. §3.1 guarantees
every destination inside the decoder is addressable, so this always succeeds; a
value receiver is found either way.

**The probe asserts to the interface the call site asked for, comma-ok.** It
must not probe one interface and cast the result into the other: a type
implementing exactly one of the two — which is every type named in §10.1 —
then panics out of `Decode`. §8 records this.

### 10.3 The bytes are given, not borrowed

The slice passed to `UnmarshalBencode` never aliases the decoder's capture
buffer. The implementation may retain and mutate it freely, and it stays valid
after the call returns and after the decoder that produced it is reused or
discarded.

This extends §9.2 rule 3's ownership guarantee to the whole custom-unmarshaling
surface, so there is no rule to learn: nothing the decoder hands out points into
memory it will reuse. `encoding/json` draws the opposite line for
`UnmarshalJSON` and documents the slice as borrowed, which is a defensible trade
when the common implementation parses and discards — but it is a trade that
buys a copy elided in the common case at the price of a silent, undiagnosable
corruption in the uncommon one. A retained borrow produces no error, no panic
and no test failure; it produces wrong bytes much later. Bencode's capture path
already allocates, so the copy is one `slices.Clone` on a value the caller
explicitly asked to see verbatim, and the failure mode it buys off is the worst
kind this decoder has.

The guarantee is unconditional. It is not "the decoder currently copies" — an
implementation that stopped copying would be violating this section, not
optimizing it.

Capture runs on §9.3's machinery, so `MaxCaptureBytes` (§7.1) bounds an
`Unmarshaler` value exactly as it bounds a `RawMessage`, and recording is
switched on for the span and off again afterwards (§9.6).

### 10.4 Map keys

`map[K]T` is accepted when `*K` implements `encoding.TextUnmarshaler`,
whatever `K`'s kind — this is what makes `map[netip.Addr]Peer` decodable. The
check is computed once from the map type, **before the map is allocated and
before any entry is read**, which is the same point §4 requires for the
string-kind check and what keeps §8's no-panic rule intact.

Each key is allocated with `reflect.New(K)` and unmarshaled through the
resulting pointer. Converting the key bytes instead
(`reflect.ValueOf(key).Convert(K)`) is not equivalent and is wrong twice over:
the result is not addressable, so a pointer receiver is invisible, and a value
receiver would mutate a copy that is then discarded. `Convert` also panics
outright for a `K` that is not string-kinded, which is precisely the case this
rule exists to serve.

`Unmarshaler` is **not** consulted for map keys. A dict key is always a bencode
string (§2), so the whole-value form would only add a second spelling of what
`UnmarshalText` already does. `encoding/json` draws the line in the same place.

### 10.5 Errors

An error returned by either method is wrapped in a `*TypeError` carrying the
offset of the value and the destination type, with the returned error as the
cause. `errors.Is` and `errors.As` reach through it to whatever the method
returned.

This departs from `encoding/json`, which returns the user's error verbatim. The
offset is the reason: a custom unmarshaler that fails four megabytes into a
stream is unlocatable without one, and §6.1 has already poisoned the decoder by
then, so the caller has no other way to recover the position.

The consequence is that such a `*TypeError` does **not** wrap `ErrTypeMismatch`
— a caller matching on that sentinel will not match it. That is correct: the
failure belongs to the type, and the decoder has not classified anything as a
mismatch.

### 10.6 Requirements

1. **Both receiver forms.** A pointer-receiver `UnmarshalBencode` must be found
   on a plain field, a pointer field, a slice element, a map value and the
   top-level destination. The pointer-receiver case is the one that breaks
   silently, because failing to find the method falls through to §4 and looks
   like an ordinary type error.
2. **Exactly one interface.** A type implementing only `UnmarshalBencode` and a
   type implementing only `UnmarshalText` must each decode without panicking,
   in a value position and in a map-key position. This is §10.2's last
   paragraph and it has already been broken once.
3. **Both interfaces.** A type implementing both must take the `Unmarshaler`
   path for every bencode type, including strings.
4. **Every bencode type.** `UnmarshalBencode` must receive `i42e`, `4:spam`,
   `li1ee` and `d1:ai1ee` verbatim, terminators included.
5. **Text against a non-string.** An integer, list or dict into a
   `TextUnmarshaler`-only destination is `ErrTypeMismatch`, not a call with
   improvised bytes.
6. **Non-UTF-8.** A key and a value containing invalid UTF-8 and NUL reach
   `UnmarshalText` byte-identical.
7. **Limits and capture.** An `Unmarshaler` value longer than
   `MaxCaptureBytes` is a `LimitError`, and recording is off again after the
   call — a second `RawMessage`-free decode on the same `Decoder` must retain
   nothing.
8. **Errors.** A method returning a sentinel yields a `*TypeError` whose
   `Offset` is the value's start and which `errors.Is`-matches that sentinel
   but not `ErrTypeMismatch`.

---

## 11. Concurrency and compatibility

### 11.1 Concurrency

A `Decoder` is **not safe for concurrent use**. It owns a read position, a
sticky error and a capture window, none of which are synchronised. One decoder
per goroutine, or external locking.

The field map cache (§3.3.3) **is** safe for concurrent use and is shared
across all decoders in the process. Its entries are published immutable, so
readers need no synchronisation once an entry exists.

### 11.2 What is covered by compatibility

Committed, will not change without a major version:

- the exported sentinel and sub-sentinel values of §5.1, and the sentinel each
  sub-sentinel wraps
- the structured error types of §5.2, their exported fields, and the sentinel
  each `Unwrap`s to
- the type mappings of §4
- the field resolution rules of §3.3
- the stream contract of §6.1
- the `Unmarshaler` interface, and the precedence of §10.2

Not committed, may change in any release:

- the text of `Error()` messages
- the default values in §7.1
- `maxIntBytes` and any other internal limit
- whether a given `SyntaxError` reports a sub-sentinel or plain `ErrSyntax`
  (it may become *more* specific)

---

## 12. Open decisions

Recorded so they are not silently defaulted:

- **Floats.** bencode has no float type, and torrents contain none. Float
  destinations are supported today (§4) and harmless. Drop, or keep as a
  convenience?
- **Bool.** §4 says any nonzero integer is true, consistent with the lenient
  stance. The alternative — `i0e`/`i1e` only, everything else `ErrOverflow` —
  is defensible. `private` in a torrent is the only real user.
- **`MaxStringBytes` default.** 8 MiB is a guess. A torrent's `pieces` field is
  20 bytes per piece, so 8 MiB is ~400k pieces. Verify against real files
  before 1.0.
- **`MaxValueBytes` default.** 16 MiB is likewise a guess, chosen as twice
  `MaxStringBytes`. The right number for a peer connection is far smaller than
  the right number for a `.torrent` file; consider shipping two named presets
  rather than one default.
- **`TypeError.Struct` / `.Field`.** Populating them requires threading a
  small context through the decode path. Worth it, or is `Offset` + `Type`
  enough in practice?
- **Silent prunes (§3.3.1).** An embedded `*common`, an ambiguous key and a
  `bencode:"-"` field are all invisible: the destination is simply never
  written. A `FieldMapError` returned from the first `Decode` against a type
  would catch these at first use, at the cost of an error path for what is
  always a programming error. Deferred, not rejected — but if it is ever added
  it should cover all three, not just embedding.
- **A dict into a struct-kinded `TextUnmarshaler` (§10.1).** Today it binds by
  §3.3, matches no exported field and returns nil, leaving the destination
  zero — silent success on input the type cannot represent, which is the
  outcome §8 and the "silent prunes" entry below both exist to avoid. The fix
  is to make a non-string value `ErrTypeMismatch` whenever the destination
  implements `TextUnmarshaler`, moving the check off the string path and onto
  the value dispatch. The argument for leaving it is `encoding/json` parity:
  json binds a JSON object to a struct the same way, and only escapes the
  problem because its ecosystem types implement `json.Unmarshaler` as well.
  That is luck, not design, and bencode has no equivalent luck. Decide before
  v1; `decode_custom_test.go` holds a skipped test for the fixed behaviour.
- **`Unmarshaler` for map keys (§10.4).** Rejected on the grounds that a key is
  always a string. The counter-argument is uniformity: a type that implements
  only `UnmarshalBencode` works everywhere except as a key, which has to be
  learned rather than deduced.
- **Depth cap on the field-tree walk.** Cycle detection bounds the walk, but a
  pathological generated type could still produce a very wide map. No limit is
  specified; add one only if a real type ever needs it.

---

## Appendix A — changes from draft 1

Grouped by why they changed.

### Corrections — draft 1 stated something that was not true

- **§6 stream contract, rewritten.** Draft 1 promised that `ErrTypeMismatch`
  and `ErrOverflow` leave the stream aligned and the decoder usable, and that
  "mismatch consumes exactly one value". That guarantee held only for
  top-level mismatches; a mismatch inside a container left unread pairs and an
  unconsumed terminator. All errors except `ErrInvalidDestination` are now
  sticky. §6.2 records the architectural reason and the divergence from
  `encoding/json`.
- **§9.7 rationale, corrected.** Draft 1 justified the two-step with "§3.3
  makes the first field claiming a key the winner". §3.3 says no such thing —
  two tagged candidates are ambiguous and bind to *neither*. The conclusion
  survives; the reasoning was wrong and would have misled anyone extending the
  rules.
- **§3.2 pointer rule, tightened.** "non-nil iff the key was present" is false
  for an ambiguous key. Now "present **and bound**".
- **§4 `[N]byte` length mismatch** returned `ErrTypeMismatch`, though the type
  did match. Now `ErrArrayLength`, wrapping it.
- **§4 `[N]T` from a list** truncated to `min(len, N)` and discarded surplus,
  following `encoding/json`. Now strict like `[N]byte`: a length mismatch is
  `ErrArrayLength`. §4.1 records why the two array cases are now symmetric.

### Gaps — draft 1 was silent where it should not have been

- **`MaxValueBytes` added (§7.1, §7.3).** No draft 1 limit bounded repetition;
  a million-element list passed every check. §7.4 documents the memory
  amplification that remains.
- **§3.1 invariant made conditional (explicitly).** The "everything inside is
  settable" claim depends entirely on §3.3 rule 1 excluding unexported fields.
  That dependency is now written down, because breaking it produces a panic,
  not an error.
- **§3.3 rule 1 wording.** "A tagged field beats an untagged one" was
  ambiguous for two tagged fields. Now "exactly one tagged candidate".
- **§3.3 rules 3 and 4.** Draft 1 did not say whether `bencode:",omitempty"`
  makes a field tagged for competition purposes. It does not.
- **§3.4** did not cover arrays.
- **§4 bool** did not say the destination is always assigned, so `i0e` into a
  `true` field was undefined.
- **§4 map keys** did not mention named string types, which is precisely the
  case that panics without an explicit conversion.
- **§5.3** did not say whether `io.EOF` is sticky. It is.
- **§10.1** concurrency was unstated.
- **§10.2** API compatibility surface was unstated.
- **§4.1** added: the array policies differ between strings and lists, and
  draft 1 never reconciled them.

### Decisions made here that draft 1 left open or implied

Flagged because these are choices, not corrections — reverse any of them
freely.

- **Structured errors (§5.2) are now required for v1**, not deferred. The
  argument: the offset they need already exists for §9, so the marginal cost is
  small, and error-position reporting is the most visible gap against stdlib.
  If this is too much surface for a first release, cut `LimitError` and keep
  `SyntaxError` + `TypeError`.
- **`NewDecoder` always wraps (§9.5).** Draft 1's fast path for an existing
  `*bufio.Reader` is removed as incompatible with capture. Alternative if the
  copy ever matters: keep the fast path and disable `RawMessage` when it is
  taken — rejected here because a decoder whose features depend on the dynamic
  type of its argument is a bad contract.
- **`Unmarshal` moved into scope (§1, §6.4)** with an explicit trailing-byte
  rule, rather than staying an open question. It is the first thing callers
  look for. Move it back to non-goals if v1 should stay minimal.
- **`Limits` as a struct field with zero-means-default (§7.1)** rather than
  package-level constants or setter methods. Constants were untunable; setters
  would be four methods for four numbers.

---

## Appendix B — changes from draft 2

One subject: **embedded fields are now lifted** (§3.3.1). Draft 2 listed
flattening as a non-goal and treated an anonymous field as an ordinary one
keyed by its type name.

### Behaviour changes

- **§1** — "embedded / anonymous struct flattening" removed from non-goals.
- **§3.3** — a struct now resolves to key → **index path** (`[]int`), not
  key → field index. Every path in a struct with no embedding has length 1, so
  nothing about a flat struct changes.
- **§3.3.1 added.** An untagged embedded struct or pointer-to-struct is walked
  through and its fields promoted. Tagged, `-`-tagged and non-struct anonymous
  fields stay ordinary fields.
- **Breaking: an untagged embedded struct no longer has a key of its own.**
  Under draft 2, `Common` embedded in `Torrent` bound the key `Common`; now the
  key `Common` binds to nothing and `Common`'s fields are reachable under their
  own keys. Tagging the embedded field restores the old behaviour.
- **§3.3.2 gained a first rule** — shallowest depth wins — which draft 2
  explicitly deferred ("Add that rule ahead of these two if flattening is ever
  adopted"). The tagged/ambiguous rules are unchanged and now apply only among
  candidates tied at the shallowest depth.
- **§3.2, §3.4** — embedded pointers are allocated on demand when a promoted
  key arrives and reused when already non-nil. This is the one way a pointer
  becomes non-nil without a key naming it.
- **§5.2** — `Struct`/`Field` on a promoted field name the *declaring* type.

### Invariants that had to move

- **§3.1** — the settability invariant no longer rests on "no unexported field
  is ever touched", because unexported embedded structs are now walked through.
  It rests on `reflect`'s distinction between an unexported embedded field's
  read-only marker (not inherited by its fields) and an unexported named
  field's (inherited). Rewritten to say so.
- **§8** — panic table extended: the unexported-embedded row was re-stated in
  terms of *named* fields, and two new rows added (embedded pointer to an
  unexported struct, recursive type in the field-map walk).

### Decisions taken here

Reversible; recorded so they are not mistaken for consequences.

- **Unexported embedded structs are lifted** (`encoding/json`'s behaviour)
  rather than skipped. Skipping would have kept §3.1's invariant literally
  true, at the cost of the standard unexported-mixin shape.
- **An embedded `*common` is pruned when the field map is built**, so the keys
  under it bind to nothing. `encoding/json` instead reaches decode time and
  returns an error. A static prune cannot desync a stream, costs nothing per
  decode, and matches how this spec already treats an ambiguous key.
- **Cycle detection is one visit per type per depth level**, which is what
  makes the breadth-first walk terminate on `type Node struct { *Node }`. A
  depth cap was considered and left out (§11).
