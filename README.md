# bencode

A streaming [bencode](https://en.wikipedia.org/wiki/Bencode) decoder for Go. It maps
bencode values onto Go types through reflection, the way `encoding/json` does, and reads
from an `io.Reader` instead of requiring the whole input in memory.

Requires Go 1.26 or newer.

```
go get github.com/ARUMANDESU/bencode
```

## Usage

```go
package main

import (
	"fmt"

	"github.com/ARUMANDESU/bencode"
)

type Torrent struct {
	Announce string `bencode:"announce"`
	Comment  string `bencode:"comment"`
}

func main() {
	data := []byte("d8:announce20:http://tracker/ann7:comment5:helloe")

	var t Torrent
	if err := bencode.Unmarshal(data, &t); err != nil {
		panic(err)
	}
	fmt.Println(t.Announce, t.Comment)
}
```

`Unmarshal` takes a byte slice and rejects trailing bytes after the value. For a stream
of values, or for a file you do not want to read into memory, use a `Decoder`:

```go
f, err := os.Open("debian.torrent")
if err != nil {
	return err
}
defer f.Close()

d := bencode.NewDecoder(f)

var t Torrent
if err := d.Decode(&t); err != nil {
	return err
}
```

Each call to `Decode` reads one value. Calling it again continues where the previous call
stopped, so a reader carrying several concatenated values can be drained in a loop. Once
a `Decode` fails, the decoder is poisoned and every later call returns the same error;
there is no recovery, because the read position after a failure is not meaningful.

## Struct tags

Fields are matched by tag first, then by field name:

```go
type Info struct {
	PieceLength int    `bencode:"piece length"`
	Pieces      []byte `bencode:"pieces"`
	Private     bool   // matches the key "Private"
	internal    string // unexported, never touched
	Scratch     int    `bencode:"-"` // never touched
}
```

Unknown keys in the input are skipped. Duplicate keys are allowed and the last one wins.
Keys do not have to be sorted. If two fields end up claiming the same key and neither one
resolves it with an explicit tag, both are dropped rather than one silently winning.

## Type mapping

| bencode | Go destinations |
|---|---|
| integer | `int`, `int8`...`int64`, `uint`...`uint64`, `float32`, `float64`, `bool`, `any` (as `int64`) |
| string | `string`, `[]byte` (fresh copy), `[N]byte` (length must match exactly), `any` (as `string`) |
| list | `[]T`, `[N]T` (length must match exactly), `any` (as `[]any`) |
| dict | struct, `map[string]T`, any map with a string-kinded key, `any` (as `map[string]any`) |

Pointers are allocated and followed. Values too large for the destination give
`ErrOverflow`, not a truncated result. Arrays are strict in both directions: a `[20]byte`
destination is a statement that the value is exactly 20 bytes, which is what you want for
a SHA-1 and a peer ID.

## RawMessage

`RawMessage` captures the verbatim bytes of a value instead of decoding it. This matters
for torrents, where the infohash has to be computed over the original `info` bytes and
re-encoding is not guaranteed to reproduce them:

```go
type Torrent struct {
	Announce string             `bencode:"announce"`
	Info     bencode.RawMessage `bencode:"info"`
}

sum := sha1.Sum(t.Info)

var info InfoDict
err := bencode.NewDecoder(bytes.NewReader(t.Info)).Decode(&info)
```

`[]byte` gives you the content of a bencode string. `RawMessage` gives you the encoded
value, whatever type it was. Captured bytes are always a copy and never alias the
decoder's buffers. Recording is off unless the destination actually contains a
`RawMessage`, so structs that do not use it pay nothing for the feature.

## Limits

Every `Decoder` carries limits so that hostile input cannot exhaust memory or blow the
stack. Set them on the decoder before the first `Decode`:

```go
d := bencode.NewDecoder(r)
d.Limits.MaxStringBytes = 1 << 20
```

| field | default | guards |
|---|---|---|
| `MaxStringBytes` | 8 MiB | a single string or dict key |
| `MaxValueBytes` | 16 MiB | total bytes consumed by one `Decode` call |
| `MaxCaptureBytes` | 8 MiB | bytes held for a `RawMessage` capture |
| `MaxDepth` | 128 | nesting depth of lists and dicts |

A zero value falls back to the default rather than meaning "unlimited".

## Errors

Errors carry a byte offset and a cause you can match with `errors.Is`:

- `*SyntaxError` for malformed input, wrapping `ErrSyntax` (and the more specific
  `ErrLeadingZero`, `ErrNegativeZero`, `ErrEmpty`)
- `*TypeError` for a value that cannot go into the destination, wrapping
  `ErrTypeMismatch`, `ErrArrayLength`, or `ErrOverflow`. It reports the struct and field
  name when the value came from a struct field
- `*LimitError` for a limit that was crossed, wrapping `ErrExceedsMax` or `ErrMaxDepth`,
  and naming which limit it was

```go
var te *bencode.TypeError
if errors.As(err, &te) {
	log.Printf("bad %s at offset %d: %v", te.Value, te.Offset, err)
}
```

The two ends of a stream are kept apart. A reader that runs out at a value boundary
gives `io.EOF`, which is how a drain loop terminates; one that runs out part-way
through a value gives `io.ErrUnexpectedEOF`, which is a broken file and not a
stopping condition:

```go
for {
	var t Torrent
	err := d.Decode(&t)
	if errors.Is(err, io.EOF) {
		break
	}
	if err != nil {
		return err
	}
	use(t)
}
```

Once the decoder returns `io.EOF` it returns it forever, so the loop cannot spin.

## Status

The decoder is done and covered by tests, including real `.torrent` files. Rough edges are
still being filed down, so the API may shift before a v1 tag. Encoding is not implemented
yet and is the next thing on the list. There are no benchmarks yet either.

`SPEC_decoder.md` is the authority on decoder behaviour. Where the code and the spec
disagree, the spec is right and the code has a bug.
