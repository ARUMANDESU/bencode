package bencode

import (
	"testing"

	bencodeast "github.com/arumandesu/bencode/pkg/bencode_ast"
	"github.com/stretchr/testify/require"
)

// repeatedString returns a bencoded string of exactly n bytes of payload.
func repeatedString(t testing.TB, n int) []byte {
	return []byte(mustEncode(t, bencodeast.Str(repeat("a", n))))
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
				err := Unmarshal(tt.data, &got)
				require.NoError(b, err)
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
	}

	b.ResetTimer()
	for _, tt := range tests {
		b.Run(tt.name, func(b *testing.B) {
			b.SetBytes(int64(len(tt.data)))
			b.ReportAllocs()
			for b.Loop() {
				var got string
				err := Unmarshal(tt.data, &got)
				require.NoError(b, err)
			}
		})
	}
}
