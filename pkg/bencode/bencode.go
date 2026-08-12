package bencode

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
)

/*
# string
5:string -> "string"
3:lol -> "lol"

# integer
prefixed with `i` followed by base 10 number and followed by `e`
i4e -> 4
i-3e -> -3
i0e -> 0

# list
prefixed with `l`, followed by elements and followed by `e`
l4:spam3:lole -> ["spam", "lol"]
l3:keki-1e3:bane -> ["kek", -1, "ban"]d3:cow3:moo4:spam4:eggse → {"cow": "moo", "spam": "eggs"}
l4:spami42el4:nestee d3:key5:valueee -> ["spam", 42, ["nest"], {"key": "value"}]

# dictionary
prefixed with `d`<key><value>`e`
d3:lol3:keke -> {"lol": "kek"}
d3:keki4ee -> {"kek": 4}
d3:cow3:moo4:spam4:eggse → {"cow": "moo", "spam": "eggs"}
*/

const MaxStringLenDigits = 6

var (
	ErrDictKeyMustBeStr   = errors.New("dictionary key must be string")
	ErrMaxStringLenDigits = errors.New("string length digits exceeds max")
	ErrLeadingZero        = errors.New("leading zero")
	ErrNegativeZero       = errors.New("negative zero")
	ErrEmpty              = errors.New("empty")
)

type Value interface{ bencode() }

type Str []byte
type Int int64
type List []Value
type Dict map[string]Value

func (Str) bencode()  {}
func (Int) bencode()  {}
func (List) bencode() {}
func (Dict) bencode() {}

func encode(w io.Writer, v Value) {
	switch v := v.(type) {
	case Str:
		fmt.Fprintf(w, "%d:%s", len(v), v)
	case Int:
		fmt.Fprintf(w, "i%de", v)
	case List:
		io.WriteString(w, "l")
		for _, ll := range v {
			encode(w, ll)
		}
		io.WriteString(w, "e")
	case Dict:
		io.WriteString(w, "d")
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			encode(w, Str(k))
			encode(w, v[k])
		}
		io.WriteString(w, "e")
	default:
		panic(fmt.Sprintf("unknown bencode value %T", v))
	}
}

func decode(br *bufio.Reader) (Value, error) {
	b, err := br.ReadByte()
	if err != nil {
		return nil, err
	}

	switch b {
	case 'd':
		d := Dict{}
		var key string
		for {
			db, err := br.ReadByte()
			if err != nil {
				return nil, err
			}
			if db == 'e' {
				break
			}

			err = br.UnreadByte()
			if err != nil {
				return nil, err
			}

			v, err := decode(br)
			if err != nil {
				return nil, err
			}

			vk, ok := v.(Str)
			if key == "" && !ok {
				return nil, fmt.Errorf("%w, but got: %T", ErrDictKeyMustBeStr, v)
			} else if key == "" {
				key = string(vk)
			} else {
				d[key] = v
				key = ""
			}
		}

		return d, nil
	case 'l':
		l := List{}

		for {
			lb, err := br.ReadByte()
			if err != nil {
				return nil, err
			}
			if lb == 'e' {
				break
			}

			err = br.UnreadByte()
			if err != nil {
				return nil, err
			}

			v, err := decode(br)
			if err != nil {
				return nil, err
			}

			l = append(l, v)
		}
		return l, nil
	case 'i':
		iStr, err := br.ReadString('e')
		if err != nil {
			return nil, err
		}
		iStr = iStr[:len(iStr)-1]

		if len(iStr) == 0 {
			return nil, ErrEmpty
		}
		if iStr[0] == '0' && len(iStr) > 1 {
			return nil, ErrLeadingZero
		}
		if iStr[0] == '-' && len(iStr) > 1 && iStr[1] == '0' {
			return nil, ErrNegativeZero
		}

		iInt, err := strconv.Atoi(iStr)
		if err != nil {
			return nil, err
		}

		return Int(iInt), nil
	default:
		lengthStr, err := br.ReadString(':')
		if err != nil {
			return nil, err
		}

		lengthStr = string(b) + lengthStr[:len(lengthStr)-1]

		if len(lengthStr) == 0 {
			return nil, ErrEmpty
		}

		if len(lengthStr) > MaxStringLenDigits {
			return nil, ErrMaxStringLenDigits
		}

		lengthInt, err := strconv.Atoi(lengthStr)
		if err != nil {
			return nil, err
		}

		str := make([]byte, lengthInt)
		n, err := io.ReadFull(br, str)
		if err != nil {
			return nil, err
		}
		if n != lengthInt {
			return nil, fmt.Errorf("read length and string length are not the same")
		}

		return Str(str), nil
	}
}

type errWriter struct {
	err error
	w   io.Writer
}

func (e *errWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	var n int
	n, e.err = e.w.Write(p)
	return n, e.err
}

func Encode(w io.Writer, v Value) error {
	ew := &errWriter{w: w}
	encode(ew, v)
	return ew.err
}

func Decode(r io.Reader) (Value, error) {
	return decode(bufio.NewReader(r))

}
