package bencoderef

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
)

const (
	tagName            = "bencode"
	MaxStringLenDigits = 7   // TODO: it needs to be adjusted
	MaxIntDigits       = 20  // TODO: it needs to be adjusted
	MaxRecurtionDepth  = 128 // TODO: it needs to be adjusted
)

var (
	ErrUnsupportedType = errors.New("unsupported type")
	ErrSyntax          = errors.New("syntax error")
	ErrLeadingZero     = errors.New("leading zero")
	ErrNegativeZero    = errors.New("negative zero")
	ErrEmpty           = errors.New("empty")
	ErrExceedsMax      = errors.New("exceeds max")

	ErrTypeMismatch = errors.New("type mismatch")
	// ErrOverflow: the value parsed, but does not fit the destination's width.
	ErrOverflow = errors.New("value overflows destination")
	// ErrInvalidDestination: the destination itself is unusable — non-pointer,
	// nil pointer, or an unsettable reflect.Value.
	ErrInvalidDestination = errors.New("invalid destination")
	// ErrMaxDepth: nesting exceeded the decoder's recursion limit.
	ErrMaxDepth = errors.New("max nesting depth exceeded")
)

type Decoder struct {
	br    *bufio.Reader
	depth uint
}

func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{br: bufio.NewReader(r)}
}

func (d *Decoder) Decode(a any) error {
	v := reflect.ValueOf(a)
	if v.Kind() != reflect.Pointer {
		return ErrInvalidDestination
	}
	return d.decode(v.Elem())
}

func (d *Decoder) decode(v reflect.Value) error {
	d.depth++
	defer func() { d.depth-- }()
	if d.depth >= MaxRecurtionDepth {
		return ErrMaxDepth
	}

	b, err := d.br.ReadByte()
	if err != nil {
		return err
	}

	switch {
	case b == 'd':
		return d.decodeDict(v)
	case b == 'l':
		return d.decodeList(v)
	case b == 'i':
		return d.decodeInt(v)
	case b >= '0' && b <= '9':
		return d.decodeString(v)
	default:
		return fmt.Errorf("%w: unexpected %q", ErrSyntax, b)
	}
}

func (d *Decoder) decodeDict(v reflect.Value) error {
	switch k := v.Kind(); k {
	case reflect.Struct:
		t := v.Type()
		tags := make(map[string]reflect.Value, t.NumField())
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			tag := field.Tag.Get(tagName)
			if len(tag) == 0 {
				tag = field.Name
			}
			if tag == "-" || !field.IsExported() {
				continue
			}
			tags[tag] = v.Field(i)
		}

		for {
			db, err := d.br.ReadByte()
			if err != nil {
				return err
			}
			if db == 'e' {
				break
			}
			err = d.br.UnreadByte()
			if err != nil {
				return err
			}

			var key string
			keyDst := reflect.ValueOf(&key).Elem()
			if err := d.decode(keyDst); err != nil {
				return err
			}

			fieldDst, ok := tags[key]
			if !ok {
				err = d.skipValue()
				if err != nil {
					return err
				}
				continue
			}

			if err := d.decode(fieldDst); err != nil {
				return err
			}
		}
	case reflect.Map, reflect.Interface:
		t := v.Type()
		if k == reflect.Interface {
			t = reflect.TypeFor[map[string]any]()
		}
		m := reflect.MakeMap(t)
		for {
			db, err := d.br.ReadByte()
			if err != nil {
				return err
			}
			if db == 'e' {
				break
			}
			err = d.br.UnreadByte()
			if err != nil {
				return err
			}

			var key string
			keyDst := reflect.ValueOf(&key).Elem()
			if err := d.decode(keyDst); err != nil {
				return err
			}
			if reflect.ValueOf(key).Kind() != reflect.String {
				return ErrTypeMismatch
			}

			fieldDst := reflect.New(t.Elem())
			if err := d.decode(fieldDst.Elem()); err != nil {
				return err
			}

			m.SetMapIndex(reflect.ValueOf(key), fieldDst.Elem())
		}
		v.Set(m)
	default:
		err := d.br.UnreadByte()
		if err != nil {
			return err
		}
		err = d.skipValue()
		if err != nil {
			return errors.Join(ErrTypeMismatch, err)
		}
		return ErrTypeMismatch
	}

	return nil
}
func (d *Decoder) decodeList(v reflect.Value) error {
	if !v.CanSet() {
		return ErrInvalidDestination
	}
	switch k := v.Kind(); k {
	case reflect.Slice, reflect.Interface:
		t := v.Type()
		if k == reflect.Interface {
			t = reflect.TypeFor[[]any]()
		}

		s := reflect.MakeSlice(t, 0, 1)

		for {
			lb, err := d.br.ReadByte()
			if err != nil {
				return err
			}
			if lb == 'e' {
				break
			}

			err = d.br.UnreadByte()
			if err != nil {
				return err
			}

			elem := reflect.New(t.Elem())
			err = d.decode(elem.Elem())
			if err != nil {
				return err
			}

			s = reflect.Append(s, elem.Elem())
		}

		v.Set(s)
	case reflect.Array:
		s := reflect.New(reflect.ArrayOf(v.Len(), v.Type().Elem())).Elem()

		i := 0
		for {
			lb, err := d.br.ReadByte()
			if err != nil {
				return err
			}
			if lb == 'e' {
				break
			}
			err = d.br.UnreadByte()
			if err != nil {
				return err
			}
			if i >= v.Len() {
				err = d.skipValue()
				if err != nil {
					return err
				}
				continue
			}

			elem := reflect.New(v.Type().Elem())
			err = d.decode(elem.Elem())
			if err != nil {
				return err
			}

			s.Index(i).Set(elem.Elem())
			i++
		}

		v.Set(s)
	default:
		err := d.br.UnreadByte()
		if err != nil {
			return err
		}
		err = d.skipValue()
		if err != nil {
			return errors.Join(ErrTypeMismatch, err)
		}
		return ErrTypeMismatch
	}
	return nil
}
func (d *Decoder) decodeInt(v reflect.Value) error {
	if !v.CanSet() {
		return ErrInvalidDestination
	}

	iStr, err := d.readInt()
	if err != nil {
		return err
	}

	switch k := v.Kind(); k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		iInt, err := strconv.ParseInt(iStr, 10, 64)
		if err != nil {
			return ErrOverflow
		}
		if v.OverflowInt(iInt) {
			return ErrOverflow
		}
		v.SetInt(int64(iInt))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		iUint, err := strconv.ParseUint(iStr, 10, 64)
		if err != nil {
			return ErrOverflow
		}
		if v.OverflowUint(iUint) {
			return ErrOverflow
		}
		v.SetUint(uint64(iUint))
	case reflect.Float32, reflect.Float64:
		iFloat, err := strconv.ParseFloat(iStr, 64)
		if err != nil {
			return ErrOverflow
		}
		if v.OverflowFloat(iFloat) {
			return ErrOverflow
		}
		v.SetFloat(iFloat)
	case reflect.Interface:
		iInt, err := strconv.ParseInt(iStr, 10, 64)
		if err != nil {
			return ErrOverflow
		}
		v.Set(reflect.ValueOf(int64(iInt)))
	default:
		return ErrTypeMismatch
	}
	return nil
}
func (d *Decoder) decodeString(v reflect.Value) error {
	err := d.br.UnreadByte()
	if err != nil {
		return err
	}

	if !v.CanSet() {
		return ErrInvalidDestination
	}

	lengthInt, err := d.readStrLen()
	if err != nil {
		return err
	}

	str := make([]byte, lengthInt)
	_, err = io.ReadFull(d.br, str)
	if err != nil {
		return err
	}

	switch k := v.Kind(); {
	case k == reflect.String:
		v.SetString(string(str))
	case k == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8:
		v.SetBytes(str)
	case k == reflect.Interface:
		v.Set(reflect.ValueOf(string(str)))
	default:
		return ErrTypeMismatch
	}

	return nil
}

func (d *Decoder) skipValue() error {
	d.depth++
	defer func() { d.depth-- }()
	if d.depth >= MaxRecurtionDepth {
		return ErrMaxDepth
	}

	b, err := d.br.ReadByte()
	if err != nil {
		return err
	}

	switch {
	case b == 'd':
		return d.skipDictList()
	case b == 'l':
		return d.skipDictList()
	case b == 'i':
		return d.skipInt()
	case b >= '0' && b <= '9':
		return d.skipString()
	default:
		return fmt.Errorf("%w: unexpected %q", ErrSyntax, b)
	}
}

func (d *Decoder) skipDictList() error {
	for {
		lb, err := d.br.ReadByte()
		if err != nil {
			return err
		}
		if lb == 'e' {
			break
		}

		err = d.br.UnreadByte()
		if err != nil {
			return err
		}

		err = d.skipValue()
		if err != nil {
			return err
		}
	}

	return nil
}

func (d *Decoder) skipInt() error {
	_, err := d.br.ReadString('e')
	if err != nil {
		return err
	}

	return nil
}

func (d *Decoder) skipString() error {
	err := d.br.UnreadByte()
	if err != nil {
		return err
	}

	lengthInt, err := d.readStrLen()
	if err != nil {
		return err
	}

	_, err = d.br.Discard(lengthInt)
	if err != nil {
		return err
	}

	return nil
}

func (d *Decoder) readSlice(delim byte, limit int) ([]byte, error) {
	var buf []byte
	n := 0
	for {
		b, err := d.br.ReadByte()
		if err != nil {
			return nil, err
		}
		if b == delim {
			break
		}
		if n == limit {
			return nil, ErrExceedsMax
		}
		buf = append(buf, b)
		n++
	}
	if n == 0 {
		return nil, ErrEmpty
	}

	return buf, nil
}

func (d *Decoder) readStrLen() (int, error) {
	buf, err := d.readSlice(':', MaxStringLenDigits)
	if err != nil {
		return 0, err
	}
	if buf[0] == '0' && len(buf) > 1 {
		return 0, ErrLeadingZero
	}
	return strconv.Atoi(string(buf))
}

func (d *Decoder) readInt() (string, error) {
	buf, err := d.readSlice('e', MaxIntDigits)
	if err != nil {
		return "", err
	}
	bufLen := len(buf)

	if bufLen == 0 {
		return "", ErrEmpty
	}
	if buf[0] == '0' && bufLen > 1 {
		return "", ErrLeadingZero
	}
	if buf[0] == '-' && bufLen > 1 && buf[1] == '0' {
		return "", ErrNegativeZero
	}
	return string(buf), nil
}
