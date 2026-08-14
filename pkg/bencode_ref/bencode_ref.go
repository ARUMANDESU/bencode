package bencoderef

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"reflect"
)

const tagName = "bencode"

var ErrUnsupportedType = errors.New("unsupported type")
var ErrSyntax = errors.New("syntax error")

type Bytes []byte

type Decoder struct {
	r  io.Reader
	br *bufio.Reader
}

func NewDecoder(r io.Reader) Decoder {
	return Decoder{r: r, br: bufio.NewReader(r)}
}

func (d *Decoder) Decode(a any) error {
	return d.decode(reflect.ValueOf(a).Elem())
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
				// read and skip
				var discard any
				if err := d.decode(reflect.ValueOf(&discard).Elem()); err != nil {
					return err
				}
				continue
			}

			if err := d.decode(fieldDst); err != nil {
				return err
			}
		}
	case reflect.Map:
	default:
		return ErrSyntax
	}

	return nil
}
func (d *Decoder) decodeList(v reflect.Value) error {
	return nil
}
func (d *Decoder) decodeInt(v reflect.Value) error {
	return nil
}
func (d *Decoder) decodeString(v reflect.Value) error {
	return nil
}
