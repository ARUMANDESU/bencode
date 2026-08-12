package bencode

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		input       string
		expected    Value
		expectedErr error
	}{
		{
			name:     "integer",
			input:    "i4e",
			expected: Int(4),
		},
		{
			name:     "integer with negative value",
			input:    "i-3e",
			expected: Int(-3),
		},
		{
			name:     "integer zero",
			input:    "i0e",
			expected: Int(0),
		},
		{
			name:        "integer with leading zero",
			input:       "i04e",
			expectedErr: ErrLeadingZero,
		},
		{
			name:        "integer with negative zero",
			input:       "i-0e",
			expectedErr: ErrNegativeZero,
		},
		{
			name:        "integer with negative zero and multiple zeroes",
			input:       "i-000e",
			expectedErr: ErrNegativeZero,
		},
		{
			name:        "integer empty",
			input:       "ie",
			expectedErr: ErrEmpty,
		},
		{
			name:     "string",
			input:    "5:hello",
			expected: Str("hello"),
		},
		{
			name:     "string lol",
			input:    "3:lol",
			expected: Str("lol"),
		},
		{
			name:     "string empty",
			input:    "0:",
			expected: Str(""),
		},
		{
			name:        "string length exceeds max digits",
			input:       "1234567:",
			expectedErr: ErrMaxStringLenDigits,
		},
		{
			name:        "string truncated",
			input:       "5:abc",
			expectedErr: io.ErrUnexpectedEOF,
		},
		{
			name:     "list empty",
			input:    "le",
			expected: List{},
		},
		{
			name:     "list of strings",
			input:    "l4:spam3:lole",
			expected: List{Str("spam"), Str("lol")},
		},
		{
			name:     "list of mixed types",
			input:    "l3:keki-1e3:bane",
			expected: List{Str("kek"), Int(-1), Str("ban")},
		},
		{
			name:     "list nested",
			input:    "l4:spami42el4:nestee",
			expected: List{Str("spam"), Int(42), List{Str("nest")}},
		},
		{
			name:        "list unterminated",
			input:       "l4:spam",
			expectedErr: io.EOF,
		},
		{
			name:     "dict empty",
			input:    "de",
			expected: Dict{},
		},
		{
			name:     "dict string value",
			input:    "d3:lol3:keke",
			expected: Dict{"lol": Str("kek")},
		},
		{
			name:     "dict int value",
			input:    "d3:keki4ee",
			expected: Dict{"kek": Int(4)},
		},
		{
			name:     "dict multiple keys",
			input:    "d3:cow3:moo4:spam4:eggse",
			expected: Dict{"cow": Str("moo"), "spam": Str("eggs")},
		},
		{
			name:        "dict unterminated",
			input:       "d3:lol3:kek",
			expectedErr: io.EOF,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			v, err := Decode(bytes.NewReader([]byte(tt.input)))
			if tt.expectedErr != nil {
				require.Error(t, err)
				assert.ErrorIs(t, err, tt.expectedErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.expected, v)
		})
	}
}
