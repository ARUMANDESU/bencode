package bencoderef

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
)

const tagName = "bencode"
const MaxStringLenDigits = 6

var (
	ErrUnsupportedType    = errors.New("unsupported type")
	ErrSyntax             = errors.New("syntax error")
	ErrLeadingZero        = errors.New("leading zero")
	ErrNegativeZero       = errors.New("negative zero")
	ErrEmpty              = errors.New("empty")
	ErrMaxStringLenDigits = errors.New("string length digits exceeds max")

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
	br *bufio.Reader
}

func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{br: bufio.NewReader(r)}
}

func (d *Decoder) Decode(a any) error {
	v := reflect.ValueOf(a)
	if v.Kind() != reflect.Pointer {
		return fmt.Errorf("non-pointer destionation TODO")
	}
	return d.decode(v.Elem())
}

func (d *Decoder) decode(v reflect.Value) error {
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
			if len(tag) == 0 || tag == "-" || !field.IsExported() {
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
				err = d.skip()
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
		err = d.skip()
		if err != nil {
			return errors.Join(ErrSyntax, err)
		}
		return ErrSyntax
	}

	return nil
}
func (d *Decoder) decodeList(v reflect.Value) error {
	if !v.CanSet() {
		return fmt.Errorf("TODO")
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
				err = d.skip()
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
		err = d.skip()
		if err != nil {
			return errors.Join(ErrSyntax, err)
		}
		return fmt.Errorf("TODO")
	}
	return nil
}
func (d *Decoder) decodeInt(v reflect.Value) error {
	if !v.CanSet() {
		return fmt.Errorf("TODO")
	}

	iStr, err := d.br.ReadString('e')
	if err != nil {
		return err
	}
	iStr = iStr[:len(iStr)-1]

	if len(iStr) == 0 {
		return ErrEmpty
	}
	if iStr[0] == '0' && len(iStr) > 1 {
		return ErrLeadingZero
	}
	if iStr[0] == '-' && len(iStr) > 1 && iStr[1] == '0' {
		return ErrNegativeZero
	}

	switch k := v.Kind(); k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		iInt, err := strconv.ParseInt(iStr, 10, 64)
		if err != nil {
			return err
		}
		if v.OverflowInt(iInt) {
			return fmt.Errorf("TODO")
		}
		v.SetInt(int64(iInt))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		iUint, err := strconv.ParseUint(iStr, 10, 64)
		if err != nil {
			return err
		}
		if v.OverflowUint(iUint) {
			return fmt.Errorf("TODO")
		}
		v.SetUint(uint64(iUint))
	case reflect.Float32, reflect.Float64:
		iFloat, err := strconv.ParseFloat(iStr, 64)
		if err != nil {
			return err
		}
		if v.OverflowFloat(iFloat) {
			return fmt.Errorf("TODO")
		}
		v.SetFloat(iFloat)
	case reflect.Interface:
		iInt, err := strconv.ParseInt(iStr, 10, 64)
		if err != nil {
			return err
		}
		v.Set(reflect.ValueOf(int64(iInt)))
	default:
		err := d.br.UnreadByte()
		if err != nil {
			return err
		}
		err = d.skip()
		if err != nil {
			return errors.Join(ErrSyntax, err)
		}
		return fmt.Errorf("TODO")
	}
	return nil
}
func (d *Decoder) decodeString(v reflect.Value) error {
	err := d.br.UnreadByte()
	if err != nil {
		return err
	}

	if !v.CanSet() {
		return fmt.Errorf("TODO")
	}

	lengthStr, err := d.br.ReadString(':')
	if err != nil {
		return err
	}

	lengthStr = lengthStr[:len(lengthStr)-1]

	if len(lengthStr) == 0 {
		return ErrEmpty
	}

	if lengthStr[0] == '0' && len(lengthStr) > 1 {
		return ErrLeadingZero
	}

	if len(lengthStr) > MaxStringLenDigits {
		return ErrMaxStringLenDigits
	}

	lengthInt, err := strconv.Atoi(lengthStr)
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
		return fmt.Errorf("TODO")
	}

	return nil
}

func (d *Decoder) skip() error {
	var discard any
	if err := d.decode(reflect.ValueOf(&discard).Elem()); err != nil {
		return err
	}
	return nil
}
