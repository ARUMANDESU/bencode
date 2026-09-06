package bencoderef

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
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

	ErrLeadingZero  = fmt.Errorf("%w: leading zero", ErrSyntax)
	ErrNegativeZero = fmt.Errorf("%w: negative zero", ErrSyntax)
	ErrEmpty        = fmt.Errorf("%w: empty", ErrSyntax)

	ErrExceedsMax = errors.New("exceeds max")

	ErrTypeMismatch = errors.New("type mismatch")
	// ErrOverflow: the value parsed, but does not fit the destination's width.
	ErrOverflow = errors.New("value overflows destination")
	// ErrInvalidDestination: the destination itself is unusable — non-pointer,
	// nil pointer, or an unsettable reflect.Value.
	ErrInvalidDestination = errors.New("invalid destination")
	// ErrMaxDepth: nesting exceeded the decoder's recursion limit.
	ErrMaxDepth = errors.New("max nesting depth exceeded")
)

type RawMessage []byte

var rawMessageType = reflect.TypeFor[RawMessage]()

type reader interface {
	io.ByteScanner
	io.Reader
}

type discarder interface {
	Discard(n int) (int, error)
}

type Decoder struct {
	br    reader
	depth uint
	err   error
}

func NewDecoder(r io.Reader) *Decoder {
	if br, ok := r.(reader); ok {
		return &Decoder{br: br}
	}
	return &Decoder{br: bufio.NewReader(r)}
}

func (d *Decoder) Decode(a any) error {
	if d.err != nil {
		return d.err
	}

	v := reflect.ValueOf(a)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return ErrInvalidDestination
	}
	return d.error(d.decode(v.Elem()))
}

func (d *Decoder) decode(v reflect.Value) error {
	d.depth++
	defer func() { d.depth-- }()
	if d.depth >= MaxRecurtionDepth {
		return ErrMaxDepth

	}

	v = indirect(v)
	if v.Kind() == reflect.Interface && v.Type().NumMethod() != 0 {
		return ErrTypeMismatch
	}

	if v.Type() == rawMessageType {
		raw, err := d.readRawValue()
		if err != nil {
			return err
		}
		v.SetBytes(raw)
		return nil
	}

	b, err := d.br.ReadByte()
	if err != nil {
		if errors.Is(err, io.EOF) && d.depth > 1 {
			return io.ErrUnexpectedEOF
		}
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
		tags := d.buildTagsMap(v)

		for {
			db, err := d.br.ReadByte()
			if err != nil {
				if errors.Is(err, io.EOF) {
					return io.ErrUnexpectedEOF
				}
				return err
			}
			if db == 'e' {
				break
			}
			err = d.br.UnreadByte()
			if err != nil {
				return err
			}

			var key any
			keyDst := reflect.ValueOf(&key).Elem()
			if err := d.decode(keyDst); err != nil {
				return err
			}
			if keyDst.Elem().Kind() != reflect.String {
				return fmt.Errorf("%w: dict key must be string, not: %s", ErrSyntax, keyDst.Elem().Kind().String())
			}

			fieldDst, ok := tags[key.(string)]
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
		if t.Key().Kind() != reflect.String {
			return ErrTypeMismatch
		}

		for {
			db, err := d.br.ReadByte()
			if err != nil {
				if errors.Is(err, io.EOF) {
					return io.ErrUnexpectedEOF
				}
				return err
			}
			if db == 'e' {
				break
			}
			err = d.br.UnreadByte()
			if err != nil {
				return err
			}

			var key any
			keyDst := reflect.ValueOf(&key).Elem()
			if err := d.decode(keyDst); err != nil {
				return err
			}
			if keyDst.Elem().Kind() != reflect.String {
				return fmt.Errorf("%w: dict key must be string, not: %s", ErrSyntax, keyDst.Elem().Kind().String())
			}

			fieldDst := reflect.New(t.Elem())
			if err := d.decode(fieldDst.Elem()); err != nil {
				return err
			}

			m.SetMapIndex(reflect.ValueOf(key).Convert(t.Key()), fieldDst.Elem())
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

type tagFieldCandidate struct {
	value      reflect.Value
	isExplicit bool
}

func (d *Decoder) buildTagsMap(v reflect.Value) map[string]reflect.Value {
	rt := v.Type()
	candidates := make(map[string][]tagFieldCandidate, rt.NumField())

	for i := 0; i < rt.NumField(); i++ {
		field := rt.Field(i)
		if !field.IsExported() {
			continue
		}

		name, isExplicit := field.Name, false
		if raw, ok := field.Tag.Lookup(tagName); ok {
			tag, _, hasOpts := strings.Cut(raw, ",")
			if tag == "-" && !hasOpts {
				continue
			}
			if tag != "" {
				name, isExplicit = tag, true
			}
		}

		candidates[name] = append(candidates[name], tagFieldCandidate{v.Field(i), isExplicit})
	}

	tags := make(map[string]reflect.Value, len(candidates))
	for name, cs := range candidates {
		if winner, ok := resolveTagCandidates(cs); ok {
			tags[name] = winner
		}
	}

	return tags
}

func resolveTagCandidates(cs []tagFieldCandidate) (reflect.Value, bool) {
	if len(cs) == 1 {
		return cs[0].value, true
	}

	var winner reflect.Value
	explicit := 0
	for _, c := range cs {
		if c.isExplicit {
			explicit++
			winner = c.value
		}
	}
	if explicit == 1 {
		return winner, true
	}
	return reflect.Value{}, false
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
				if errors.Is(err, io.EOF) {
					return io.ErrUnexpectedEOF
				}
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
				if errors.Is(err, io.EOF) {
					return io.ErrUnexpectedEOF
				}
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
	case reflect.Bool:
		v.SetBool(iStr != "0")
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
		if errors.Is(err, io.EOF) {
			return io.ErrUnexpectedEOF
		}
		return err
	}

	switch k := v.Kind(); {
	case k == reflect.String:
		v.SetString(string(str))
	case k == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8:
		v.SetBytes(str)
	case k == reflect.Array && v.Type().Elem().Kind() == reflect.Uint8:
		if v.Len() != len(str) {
			return ErrTypeMismatch
		}

		reflect.Copy(v, reflect.ValueOf(str))
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
	_, err := d.readInt()
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

	_, err = d.discard(lengthInt)
	if err != nil {
		return err
	}

	return nil
}

func (d *Decoder) discard(n int) (int, error) {
	if n < 0 {
		return 0, bufio.ErrNegativeCount
	}
	if dr, ok := d.br.(discarder); ok {
		return dr.Discard(n)
	}
	m, err := io.CopyN(io.Discard, d.br, int64(n))
	return int(m), err
}

func (d *Decoder) readIntSlice(delim byte, limit int) ([]byte, error) {
	var buf []byte
	n := 0
	for {
		b, err := d.br.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if b == delim {
			break
		}
		if (b < '0' || b > '9') && (b != '-' || n != 0) {
			return nil, ErrSyntax
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
	if n == 1 && buf[0] == '-' {
		return nil, ErrSyntax
	}

	return buf, nil
}

func (d *Decoder) readStrLen() (int, error) {
	buf, err := d.readIntSlice(':', MaxStringLenDigits)
	if err != nil {
		return 0, err
	}
	if buf[0] == '0' && len(buf) > 1 {
		return 0, ErrLeadingZero
	}
	if buf[0] == '-' {
		return 0, ErrSyntax
	}
	return strconv.Atoi(string(buf))
}

func (d *Decoder) readInt() (string, error) {
	buf, err := d.readIntSlice('e', MaxIntDigits)
	if err != nil {
		if errors.Is(err, ErrExceedsMax) {
			return "", ErrOverflow
		}
		return "", err
	}
	bufLen := len(buf)

	if buf[0] == '0' && bufLen > 1 {
		return "", ErrLeadingZero
	}
	if buf[0] == '-' && bufLen > 1 && buf[1] == '0' {
		return "", ErrNegativeZero
	}
	return string(buf), nil
}

func (d *Decoder) readRawValue() ([]byte, error) {
	return nil, nil
}

func (d *Decoder) error(err error) error {
	if !errors.Is(err, io.EOF) {
		d.err = err
	}
	return err
}

func getTag(tag string) (string, bool) {
	if len(tag) == 0 {
		return "", true
	}
	opts := splitTag(tag)
	if opts[0] == "-" && len(tag) == 1 {
		return "", false
	}
	return opts[0], true
}

func splitTag(tag string) []string {
	if len(tag) == 0 {
		return nil
	}

	c := count(tag, ',')
	if c == 0 {
		return []string{tag}
	}

	s := make([]string, 0, c)
	var n, m int
	for n <= len(tag) {
		if n == len(tag) || tag[n] == ',' {
			s = append(s, tag[m:n])
			m = n + 1
		}

		n++
	}
	return s
}

func count(s string, ch byte) int {
	if len(s) == 0 || ch == 0 {
		return 0
	}

	count := 0
	for n := 0; n < len(s); n++ {
		if s[n] == ch {
			count++
		}
	}

	return count
}

func indirect(v reflect.Value) reflect.Value {
	for {
		switch v.Kind() {
		case reflect.Pointer:
			if v.IsNil() {
				if !v.CanSet() {
					return v
				}
				v.Set(reflect.New(v.Type().Elem()))
			}
			v = v.Elem()
		default:
			return v
		}
	}
}
