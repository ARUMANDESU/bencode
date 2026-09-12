package bencode

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	bencodeast "github.com/arumandesu/bencode/pkg/bencode_ast"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// SPEC §11.1 — concurrency
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

	matches, err := filepath.Glob(filepath.Join("testdata", "*.torrent"))
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
