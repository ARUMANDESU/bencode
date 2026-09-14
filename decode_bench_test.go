package bencode

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	bencodeast "github.com/arumandesu/bencode/pkg/bencode_ast"
	"github.com/stretchr/testify/require"
)

// repeatedString returns a bencoded string of exactly n bytes of payload.
func repeatedString(t testing.TB, n int) []byte {
	return mustEncodeBytes(t, bencodeast.Str(repeat("a", n)))
}

func mapWithNumOfKeys(t testing.TB, n int) []byte {
	dict := make(bencodeast.Dict, n)
	for i := range n {
		dict[fmt.Sprintf("k%04d", i)] = bencodeast.Str(fmt.Sprintf("k%04d", i))
	}
	return mustEncodeBytes(t, dict)
}

func BenchmarkInt(b *testing.B) {

	tests := []struct {
		name string
		data []byte
	}{
		{"2bytes", []byte("i12e")},
		{"8bytes", []byte("i12345678e")},
		{"16bytes", []byte("i1234567890123456e")},
		{"19bytes", []byte("i1234567890123456789e")},
	}

	b.ResetTimer()
	for _, tt := range tests {
		b.Run(tt.name, func(b *testing.B) {
			b.SetBytes(int64(len(tt.data)))
			b.ReportAllocs()
			for b.Loop() {
				var got int
				mustUnmarshal(b, tt.data, &got)
			}
		})
	}
}

func BenchmarkString(b *testing.B) {
	tests := []struct {
		name string
		data []byte
	}{
		{"8B", repeatedString(b, 8)},
		{"1KB", repeatedString(b, 1<<10)},
		{"1MB", repeatedString(b, 1<<20)},
		{"7MB", repeatedString(b, 7<<20)},
	}

	b.ResetTimer()
	for _, tt := range tests {
		b.Run(tt.name, func(b *testing.B) {
			b.SetBytes(int64(len(tt.data)))
			b.ReportAllocs()
			for b.Loop() {
				var got string
				mustUnmarshal(b, tt.data, &got)
			}
		})
	}
}

func BenchmarkMap(b *testing.B) {
	tests := []struct {
		name string
		data []byte
	}{
		{"8keys", mapWithNumOfKeys(b, 8)},
		{"16keys", mapWithNumOfKeys(b, 16)},
		{"32keys", mapWithNumOfKeys(b, 32)},
		{"64keys", mapWithNumOfKeys(b, 64)},
		{"256keys", mapWithNumOfKeys(b, 256)},
		{"1024keys", mapWithNumOfKeys(b, 1024)},
	}

	b.ResetTimer()
	for _, tt := range tests {
		b.Run(tt.name, func(b *testing.B) {
			b.SetBytes(int64(len(tt.data)))
			b.ReportAllocs()
			for b.Loop() {
				var got map[string]any
				mustUnmarshal(b, tt.data, &got)
			}
		})
	}
}

func BenchmarkMapDestination(b *testing.B) {
	in := mapWithNumOfKeys(b, 64)
	b.ResetTimer()

	b.Run("map", func(b *testing.B) {
		b.SetBytes(int64(len(in)))
		b.ReportAllocs()
		for b.Loop() {
			var got map[string]any
			mustUnmarshal(b, in, &got)
		}
	})

	b.Run("struct", func(b *testing.B) {
		type dst struct {
			K0000 string `bencode:"k0000"`
			K0001 string `bencode:"k0001"`
			K0002 string `bencode:"k0002"`
		}
		b.SetBytes(int64(len(in)))
		b.ReportAllocs()
		for b.Loop() {
			var got dst
			mustUnmarshal(b, in, &got)
		}
	})

	b.Run("raw", func(b *testing.B) {
		b.SetBytes(int64(len(in)))
		b.ReportAllocs()
		for b.Loop() {
			var got RawMessage
			mustUnmarshal(b, in, &got)
		}
	})
}

func benchTorrent(b *testing.B) []byte {
	b.Helper()
	matches, err := filepath.Glob(filepath.Join("testdata", "*.torrent"))
	if err != nil || len(matches) == 0 {
		b.Skip("no .torrent fixture in testdata")
	}
	in, err := os.ReadFile(matches[0])
	if err != nil {
		b.Fatal(err)
	}
	return in
}

func BenchmarkUnmarshalTorrent(b *testing.B) {
	in := benchTorrent(b)
	b.SetBytes(int64(len(in)))
	b.ReportAllocs()
	for b.Loop() {
		var got torrentMeta
		if err := Unmarshal(in, &got); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecoderTorrent(b *testing.B) {
	in := benchTorrent(b)
	b.SetBytes(int64(len(in)))
	b.ReportAllocs()
	for b.Loop() {
		var got torrentMeta
		err := NewDecoder(bytes.NewReader(in)).Decode(&got)
		require.NoError(b, err)
	}
}
