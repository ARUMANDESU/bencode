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
)

type Decoder struct {
	br *bufio.Reader
}

func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{br: bufio.NewReader(r)}
}

func (d *Decoder) Decode(a any) error {
	v := reflect.ValueOf(a)
	if v.Kind() != reflect.Ptr {
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
		d.br.UnreadByte()
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
			tags[t.Field(i).Tag.Get(tagName)] = v.Field(i)
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
				err = d.Skip()
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
		s := reflect.New(reflect.ArrayOf(v.Len(), v.Type().Elem()))

		i := 0
		for {
			lb, err := d.br.ReadByte()
			if err != nil {
				return err
			}
			if lb == 'e' {
				break
			}
			if i >= v.Len() {
				err = d.Skip()
				if err != nil {
					return err
				}
				continue
			}

			err = d.br.UnreadByte()
			if err != nil {
				return err
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

	iInt, err := strconv.Atoi(iStr)
	if err != nil {
		return err
	}

	switch k := v.Kind(); k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(iInt))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(iInt))
	case reflect.Interface:
		v.Set(reflect.ValueOf(int64(iInt)))
	default:
		return fmt.Errorf("TODO")
	}
	return nil
}
func (d *Decoder) decodeString(v reflect.Value) error {
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

func (d *Decoder) Skip() error {
	var discard any
	if err := d.decode(reflect.ValueOf(&discard).Elem()); err != nil {
		return err
	}
	return nil
}
