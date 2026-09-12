package bencode

import (
	"bufio"
	"bytes"
	"encoding"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
)

const (
	tagName                  = "bencode"
	DefaultMaxStringBytes    = 8 << 20
	DefaultMaxCaptureBytes   = 8 << 20
	DefaultMaxValueBytes     = 16 << 20
	DefaultMaxRecursionDepth = 128
)

const (
	maxRecBufRetained = 64 << 10
	maxIntegerDigits  = 21
)

var (
	ErrInternal = errors.New("internal error")
	ErrSyntax   = errors.New("syntax error")

	ErrLeadingZero  = fmt.Errorf("%w: leading zero", ErrSyntax)
	ErrNegativeZero = fmt.Errorf("%w: negative zero", ErrSyntax)
	ErrEmpty        = fmt.Errorf("%w: empty", ErrSyntax)

	ErrExceedsMax   = errors.New("exceeds max")
	ErrTypeMismatch = errors.New("type mismatch")
	ErrArrayLength  = fmt.Errorf("%w: array must be exact length", ErrTypeMismatch)

	// ErrOverflow: the value parsed, but does not fit the destination's width.
	ErrOverflow = errors.New("value overflows destination")
	// ErrInvalidDestination: the destination itself is unusable — non-pointer,
	// nil pointer, or an unsettable reflect.Value.
	ErrInvalidDestination = errors.New("invalid destination")
	// ErrMaxDepth: nesting exceeded the decoder's recursion limit.
	ErrMaxDepth = errors.New("max nesting depth exceeded")
)

type RawMessage []byte

var (
	rawMessageType      = reflect.TypeFor[RawMessage]()
	textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
)

var fieldCache sync.Map // tag: []'field idx'

type Unmarshaler interface {
	UnmarshalBencode([]byte) error
}

type SyntaxError struct {
	Offset int64
	msg    string
	cause  error
}

func (e *SyntaxError) Unwrap() error { return e.cause }
func (e *SyntaxError) Error() string {
	return e.msg
}

type TypeError struct {
	Offset int64
	Value  string       // "integer", "string", "list", "dict"
	Type   reflect.Type // destination type
	Struct string       // struct type name, if the value was a struct field
	Field  string       // field key, if the value was a struct field
	cause  error        // ErrTypeMismatch, ErrArrayLength or ErrOverflow
}

func (e *TypeError) Unwrap() error { return e.cause }
func (e *TypeError) Error() string {
	if e.Struct != "" || e.Field != "" {
		return "cannot unmarshal " + e.Value + " into Go struct field " + e.Struct + "." + e.Field + " of type " + e.Type.String() + ": " + e.cause.Error()
	}
	return "cannot unmarshal " + e.Value + " into Go value of type " + e.Type.String() + ": " + e.cause.Error()
}

type LimitError struct {
	Offset int64  // byte offset at which the limit was exceeded
	Limit  string // "MaxStringBytes", "MaxDepth", "MaxValueBytes", ...
	Value  int64  // the value that exceeded it, where meaningful
	cause  error  // ErrExceedsMax or ErrMaxDepth
}

func (e *LimitError) Unwrap() error { return e.cause }
func (e *LimitError) Error() string {
	return fmt.Sprintf("exceeds limit: %s, value: %d", e.Limit, e.Value)
}

type Limits struct {
	MaxStringBytes  int64
	MaxValueBytes   int64
	MaxCaptureBytes int64
	MaxDepth        uint
}

type Decoder struct {
	Limits Limits

	br              *bufio.Reader
	rec             *recorder
	depth           uint
	err             error
	off             int64 // snapshotted offset
	startOff        int64
	captureStartOff int64
	intBuf          [maxIntegerDigits]byte // one buffer for reading int -> no buffer slice alloc every time
}

func NewDecoder(r io.Reader) *Decoder {
	rec := recorder{src: r}
	d := Decoder{
		br:  bufio.NewReader(&rec),
		rec: &rec,
		Limits: Limits{
			MaxStringBytes:  DefaultMaxStringBytes,
			MaxValueBytes:   DefaultMaxValueBytes,
			MaxCaptureBytes: DefaultMaxCaptureBytes,
			MaxDepth:        DefaultMaxRecursionDepth,
		},
	}
	return &d
}

func Unmarshal(b []byte, v any) error {
	d := NewDecoder(bytes.NewReader(b))
	err := d.Decode(v)
	if err != nil {
		return err
	}

	buf, err := d.br.Peek(1)
	if err == nil {
		return &SyntaxError{
			Offset: d.offset(),
			msg:    fmt.Sprintf("trailing data after value, got: %q", buf[0]),
			cause:  ErrSyntax,
		}
	}
	if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (d *Decoder) Decode(a any) error {
	if d.err != nil {
		return d.err
	}

	v := reflect.ValueOf(a)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return ErrInvalidDestination
	}

	d.fixLimits()
	d.startOff = d.offset()
	return d.error(d.decode(v.Elem()))
}

func (d *Decoder) decode(v reflect.Value) error {
	if d.offset()-d.startOff > d.Limits.MaxValueBytes {
		return &LimitError{
			Offset: d.offset(),
			Limit:  "MaxValueBytes",
			Value:  d.Limits.MaxValueBytes,
			cause:  ErrExceedsMax,
		}
	}

	d.snapshotOffset()

	d.depth++
	defer func() { d.depth-- }()
	if d.depth > d.Limits.MaxDepth {
		return &LimitError{
			Offset: d.off,
			Limit:  "MaxDepth",
			Value:  int64(d.Limits.MaxDepth),
			cause:  ErrMaxDepth,
		}
	}

	v = indirect(v)
	if v.Kind() == reflect.Interface && v.Type().NumMethod() != 0 {
		return &TypeError{Offset: d.off, Type: v.Type(), cause: ErrTypeMismatch}
	}

	if v.Type() == rawMessageType {
		return d.decodeRawValue(v)
	}
	if u, ok := customUnmarshaler[Unmarshaler](v); ok {
		return d.useCustomUnmarshal(v, u)
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
		return &SyntaxError{
			Offset: d.off,
			msg:    fmt.Sprintf("unexpected: %q", b),
			cause:  ErrSyntax,
		}
	}
}

func (d *Decoder) decodeDict(v reflect.Value) error {
	t := v.Type()
	if !v.CanSet() {
		return &TypeError{
			Offset: d.off,
			Value:  "dict",
			Type:   t,
			cause:  ErrInvalidDestination,
		}
	}
	switch k := v.Kind(); k {
	case reflect.Struct:
		fields := cachedFields(t)

		for {
			key, ok, err := d.readDictKey()
			if err != nil {
				return &TypeError{Offset: d.off, Value: "dict", Type: t, cause: err}
			}
			if !ok {
				break
			}

			idxs, ok := fields[string(key)]
			if !ok {
				err = d.skipValue()
				if err != nil {
					return &TypeError{Offset: d.offset(), Value: "dict", Type: t, cause: err}
				}
				continue
			}

			field, ft := v, t
			for i, idx := range idxs {
				if i > 0 {
					ft = indirectType(ft.Field(idxs[i-1]).Type)
				}
				field = indirect(field).Field(idx)
			}

			if err := d.decode(field); err != nil {
				if te, ok := errors.AsType[*TypeError](err); ok && te.Field == "" {
					te.Struct = ft.Name()
					te.Field = ft.Field(idxs[len(idxs)-1]).Name
				}
				return err
			}
		}
	case reflect.Map, reflect.Interface:
		t := v.Type()
		if k == reflect.Interface {
			t = reflect.TypeFor[map[string]any]()
		}
		tKey := t.Key()
		tKeyIsText := reflect.PointerTo(tKey).Implements(textUnmarshalerType)
		if tKey.Kind() != reflect.String && !tKeyIsText {
			return &TypeError{Offset: d.off, Value: "dict", Type: t, cause: ErrTypeMismatch}
		}
		m := reflect.MakeMap(t)

		for {
			key, ok, err := d.readDictKey()
			if err != nil {
				return &TypeError{Offset: d.off, Value: "dict", Type: t, cause: err}
			}
			if !ok {
				break
			}

			keyPtr := reflect.New(tKey)
			rvKey := keyPtr.Elem()
			if tKeyIsText {
				if err := keyPtr.Interface().(encoding.TextUnmarshaler).UnmarshalText(key); err != nil {
					return &TypeError{Offset: d.off, Value: "dict", Type: t, cause: err}
				}
			} else {
				rvKey.SetString(string(key))
			}

			fieldDst := reflect.New(t.Elem())
			if err := d.decode(fieldDst.Elem()); err != nil {
				return err
			}

			m.SetMapIndex(rvKey, fieldDst.Elem())
		}
		v.Set(m)
	default:
		return &TypeError{Offset: d.off, Value: "dict", Type: t, cause: ErrTypeMismatch}
	}

	return nil
}

type fieldCandidate struct {
	isExplicit bool
	indexes    []int
}

func cachedFields(t reflect.Type) map[string][]int {
	if f, ok := fieldCache.Load(t); ok {
		return f.(map[string][]int)
	}
	f, _ := fieldCache.LoadOrStore(t, buildFields(t))
	return f.(map[string][]int)
}

func buildFields(t reflect.Type) map[string][]int {
	candidates := make(map[string][]fieldCandidate, t.NumField())
	populateCandidates(t, nil, candidates, map[reflect.Type]struct{}{t: {}})

	fields := make(map[string][]int, len(candidates))
	for name, cs := range candidates {
		if winner, ok := resolveCandidates(cs); ok {
			fields[name] = winner.indexes
		}
	}

	return fields
}

func populateCandidates(t reflect.Type, prevIdx []int, candidates map[string][]fieldCandidate, embedRecursionBan map[reflect.Type]struct{}) {
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() && !field.Anonymous {
			continue
		}

		name, isExplicit, idxs := field.Name, false, append(slices.Clone(prevIdx), i)
		if raw, ok := field.Tag.Lookup(tagName); ok {
			tag, _, hasOpts := strings.Cut(raw, ",")
			if tag == "-" && !hasOpts {
				continue
			}
			if field.Anonymous && !field.IsExported() {
				continue
			}
			if tag != "" {
				name, isExplicit = tag, true
			}
		} else if field.Anonymous {
			if field.Type.Kind() == reflect.Pointer && !field.IsExported() {
				continue
			}
			ft := indirectType(field.Type)
			if _, ok := embedRecursionBan[ft]; ok {
				continue
			}

			switch ft.Kind() {
			case reflect.Struct:
				embedRecursionBan[ft] = struct{}{}
				// embedded struct fields lifting
				populateCandidates(ft, idxs, candidates, embedRecursionBan)
				delete(embedRecursionBan, ft)
				continue
			default:
				if !field.IsExported() {
					continue
				}
			}
		}

		candidates[name] = append(candidates[name], fieldCandidate{isExplicit, idxs})
	}
}

func resolveCandidates(cs []fieldCandidate) (fieldCandidate, bool) {
	if len(cs) == 0 {
		return fieldCandidate{}, false
	}

	// rule 1: shallowest wins outright, tagged or not.
	depth := len(cs[0].indexes)
	for _, c := range cs[1:] {
		depth = min(depth, len(c.indexes))
	}

	var shallowest, tagged fieldCandidate
	tied, explicit := 0, 0
	for _, c := range cs {
		if len(c.indexes) != depth {
			continue
		}
		tied++
		shallowest = c
		if c.isExplicit {
			explicit++
			tagged = c
		}
	}

	switch {
	case tied == 1:
		return shallowest, true
	case explicit == 1:
		// rule 2: exactly one tagged candidate breaks a same-depth tie.
		return tagged, true
	default:
		// rule 3: zero or several tags at that depth -> binds to nothing.
		return fieldCandidate{}, false
	}
}

func (d *Decoder) decodeList(v reflect.Value) error {
	t := v.Type()
	if !v.CanSet() {
		return &TypeError{Offset: d.off, Value: "list", Type: t, cause: ErrInvalidDestination}
	}
	switch k := v.Kind(); k {
	case reflect.Slice, reflect.Interface:
		t := v.Type()
		if k == reflect.Interface {
			t = reflect.TypeFor[[]any]()
		}

		s := reflect.New(t).Elem()

		i := 0
		for {
			lb, err := d.br.ReadByte()
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = io.ErrUnexpectedEOF
				}
				return &TypeError{Offset: d.off, Value: "list", Type: t, cause: err}
			}
			if lb == 'e' {
				break
			}

			err = d.br.UnreadByte()
			if err != nil {
				return &TypeError{Offset: d.off, Value: "list", Type: t, cause: err}
			}

			if i >= s.Cap() {
				newCap := max(s.Cap()+s.Cap()/2, 4)
				grown := reflect.MakeSlice(t, s.Len(), newCap)
				reflect.Copy(grown, s)
				s.Set(grown)
			}
			if i >= s.Len() {
				s.SetLen(i + 1)
			}

			if err := d.decode(s.Index(i)); err != nil {
				return err
			}
			i++
		}

		s.SetLen(i)
		if i == 0 {
			s.Set(reflect.MakeSlice(t, 0, 0)) // non-nil empty slice
		}
		v.Set(s)
	case reflect.Array:
		s := reflect.New(v.Type()).Elem()

		i := 0
		for {
			lb, err := d.br.ReadByte()
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = io.ErrUnexpectedEOF
				}
				return &TypeError{Offset: d.off, Value: "list", Type: t, cause: err}
			}
			if lb == 'e' {
				break
			}
			err = d.br.UnreadByte()
			if err != nil {
				return &TypeError{Offset: d.off, Value: "list", Type: t, cause: err}
			}
			if i >= v.Len() {
				return &TypeError{Offset: d.off, Value: "list", Type: t, cause: ErrArrayLength}
			}

			if err := d.decode(s.Index(i)); err != nil {
				return err
			}
			i++
		}
		if i < v.Len() {
			return &TypeError{Offset: d.off, Value: "list", Type: t, cause: ErrArrayLength}
		}

		v.Set(s)
	default:
		return &TypeError{Offset: d.off, Value: "list", Type: t, cause: ErrTypeMismatch}
	}
	return nil
}
func (d *Decoder) decodeInt(v reflect.Value) error {
	t := v.Type()
	if !v.CanSet() {
		return &TypeError{Offset: d.off, Value: "integer", Type: t, cause: ErrInvalidDestination}
	}

	iStr, err := d.readInt()
	if err != nil {
		return err
	}

	switch k := v.Kind(); k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		iInt, err := strconv.ParseInt(iStr, 10, 64)
		if err != nil {
			return &TypeError{Offset: d.off, Value: "integer", Type: t, cause: ErrOverflow}
		}
		if v.OverflowInt(iInt) {
			return &TypeError{Offset: d.off, Value: "integer", Type: t, cause: ErrOverflow}
		}
		v.SetInt(int64(iInt))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		iUint, err := strconv.ParseUint(iStr, 10, 64)
		if err != nil {
			return &TypeError{Offset: d.off, Value: "integer", Type: t, cause: ErrOverflow}
		}
		if v.OverflowUint(iUint) {
			return &TypeError{Offset: d.off, Value: "integer", Type: t, cause: ErrOverflow}
		}
		v.SetUint(uint64(iUint))
	case reflect.Float32, reflect.Float64:
		iFloat, err := strconv.ParseFloat(iStr, 64)
		if err != nil {
			return &TypeError{Offset: d.off, Value: "integer", Type: t, cause: ErrOverflow}
		}
		if v.OverflowFloat(iFloat) {
			return &TypeError{Offset: d.off, Value: "integer", Type: t, cause: ErrOverflow}
		}
		v.SetFloat(iFloat)
	case reflect.Interface:
		iInt, err := strconv.ParseInt(iStr, 10, 64)
		if err != nil {
			return &TypeError{Offset: d.off, Value: "integer", Type: t, cause: ErrOverflow}
		}
		v.Set(reflect.ValueOf(int64(iInt)))
	case reflect.Bool:
		v.SetBool(iStr != "0")
	default:
		return &TypeError{Offset: d.off, Value: "integer", Type: t, cause: ErrTypeMismatch}
	}
	return nil
}
func (d *Decoder) decodeString(v reflect.Value) error {
	t := v.Type()
	err := d.br.UnreadByte()
	if err != nil {
		return &TypeError{Offset: d.off, Value: "string", Type: t, cause: err}
	}

	if !v.CanSet() {
		return &TypeError{Offset: d.off, Value: "string", Type: t, cause: ErrInvalidDestination}
	}

	lengthInt, err := d.readStrLen()
	if err != nil {
		return err
	}
	if d.offset()-d.startOff+int64(lengthInt) > d.Limits.MaxValueBytes {
		return &LimitError{
			Offset: d.offset(),
			Limit:  "MaxValueBytes",
			Value:  d.offset() + int64(lengthInt),
			cause:  ErrExceedsMax,
		}
	}
	if int64(lengthInt) > d.Limits.MaxStringBytes {
		return &LimitError{
			Offset: d.off,
			Limit:  "MaxStringBytes",
			Value:  int64(lengthInt),
			cause:  ErrExceedsMax,
		}
	}

	str := make([]byte, lengthInt)
	_, err = io.ReadFull(d.br, str)
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return &TypeError{Offset: d.off, Value: "string", Type: t, cause: err}
	}

	if u, ok := customUnmarshaler[encoding.TextUnmarshaler](v); ok {
		err := u.UnmarshalText(str)
		if err != nil {
			return &TypeError{Offset: d.off, Value: "string", Type: t, cause: err}
		}
		return nil
	}

	switch k := v.Kind(); {
	case k == reflect.String:
		v.SetString(string(str))
	case k == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8:
		v.SetBytes(str)
	case k == reflect.Array && v.Type().Elem().Kind() == reflect.Uint8:
		if v.Len() != len(str) {
			return &TypeError{Offset: d.off, Value: "string", Type: t, cause: ErrArrayLength}
		}

		reflect.Copy(v, reflect.ValueOf(str).Convert(t))
	case k == reflect.Interface:
		v.Set(reflect.ValueOf(string(str)))
	default:
		return &TypeError{Offset: d.off, Value: "string", Type: t, cause: ErrTypeMismatch}
	}

	return nil
}

func (d *Decoder) skipValue() error {
	if d.offset()-d.startOff > d.Limits.MaxValueBytes {
		return &LimitError{
			Offset: d.offset(),
			Limit:  "MaxValueBytes",
			Value:  d.Limits.MaxValueBytes,
			cause:  ErrExceedsMax,
		}
	}
	if d.rec.isOn && d.offset()-d.captureStartOff > d.Limits.MaxCaptureBytes {
		return &LimitError{
			Offset: d.offset(),
			Limit:  "MaxCaptureBytes",
			Value:  d.Limits.MaxCaptureBytes,
			cause:  ErrExceedsMax,
		}
	}

	d.snapshotOffset()

	d.depth++
	defer func() { d.depth-- }()
	if d.depth > d.Limits.MaxDepth {
		return &LimitError{
			Offset: d.off,
			Limit:  "MaxDepth",
			Value:  int64(d.Limits.MaxDepth),
			cause:  ErrMaxDepth,
		}
	}

	b, err := d.br.ReadByte()
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
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
		return &SyntaxError{
			Offset: d.off,
			msg:    fmt.Sprintf("unexpected: %q", b),
			cause:  ErrSyntax,
		}
	}
}

func (d *Decoder) skipDictList() error {
	for {
		lb, err := d.br.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
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
	if d.offset()-d.startOff+int64(lengthInt) > d.Limits.MaxValueBytes {
		return &LimitError{
			Offset: d.offset(),
			Limit:  "MaxValueBytes",
			Value:  d.offset() + int64(lengthInt),
			cause:  ErrExceedsMax,
		}
	}
	if d.rec.isOn && d.offset()-d.captureStartOff+int64(lengthInt) > d.Limits.MaxCaptureBytes {
		return &LimitError{
			Offset: d.offset(),
			Limit:  "MaxCaptureBytes",
			Value:  d.Limits.MaxCaptureBytes,
			cause:  ErrExceedsMax,
		}
	}
	if int64(lengthInt) > d.Limits.MaxStringBytes {
		return &LimitError{
			Offset: d.off,
			Limit:  "MaxStringBytes",
			Value:  int64(lengthInt),
			cause:  ErrExceedsMax,
		}
	}

	_, err = d.br.Discard(lengthInt)
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return err
	}

	return nil
}

// readIntSlice reads int from reader byte by byte
//
// note: returned slice aliases [Decoder.intBuf]
func (d *Decoder) readIntSlice(delim byte) ([]byte, error) {
	buf := d.intBuf[:0]
	for {
		b, err := d.br.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if b == delim {
			break
		}
		bl := len(buf)
		if (b < '0' || b > '9') && (b != '-' || bl != 0) {
			return nil, &SyntaxError{
				Offset: d.off,
				msg:    fmt.Sprintf("must be digit or `-`, got: %q", b),
				cause:  ErrSyntax,
			}
		}
		if bl >= cap(buf) {
			return nil, &LimitError{
				Offset: d.off,
				Limit:  "MaxIntegerDigits",
				Value:  int64(bl),
				cause:  ErrExceedsMax,
			}
		}
		buf = append(buf, b)
	}
	if len(buf) == 0 {
		return nil, &SyntaxError{Offset: d.off, msg: "can't be empty", cause: ErrEmpty}
	}
	if len(buf) == 1 && buf[0] == '-' {
		return nil, &SyntaxError{Offset: d.off, msg: "no digits after '-'", cause: ErrSyntax}
	}

	return buf, nil
}

func (d *Decoder) readStrLen() (int, error) {
	buf, err := d.readIntSlice(':')
	if err != nil {
		return 0, err
	}
	if buf[0] == '0' && len(buf) > 1 {
		return 0, &SyntaxError{
			Offset: d.off,
			msg:    "leading zero is forbidden",
			cause:  ErrLeadingZero,
		}
	}
	if buf[0] == '-' {
		return 0, &SyntaxError{
			Offset: d.off,
			msg:    "leading `-` is forbidden for string length",
			cause:  ErrSyntax,
		}
	}
	i, err := strconv.Atoi(string(buf))
	if err != nil {
		return 0, &SyntaxError{
			Offset: d.off,
			msg:    "int overflow",
			cause:  ErrOverflow,
		}
	}

	return i, nil
}

func (d *Decoder) readInt() (string, error) {
	buf, err := d.readIntSlice('e')
	if err != nil {
		return "", err
	}
	bufLen := len(buf)

	if buf[0] == '0' && bufLen > 1 {
		return "", &SyntaxError{
			Offset: d.off,
			msg:    "leading zero is forbidden",
			cause:  ErrLeadingZero,
		}
	}
	if buf[0] == '-' && bufLen > 1 && buf[1] == '0' {
		return "", &SyntaxError{
			Offset: d.off,
			msg:    "negative zero is forbidden",
			cause:  ErrNegativeZero,
		}
	}
	return string(buf), nil
}

func (d *Decoder) readDictKey() ([]byte, bool, error) {
	d.snapshotOffset()
	b, err := d.br.ReadByte()
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, false, err
	}
	if b == 'e' {
		return nil, false, nil
	}
	if b < '0' || b > '9' {
		return nil, false, &SyntaxError{
			Offset: d.off,
			msg:    fmt.Sprintf("dict key must be a string, got %q", b),
			cause:  ErrSyntax,
		}
	}
	err = d.br.UnreadByte()
	if err != nil {
		return nil, false, err
	}

	n, err := d.readStrLen()
	if err != nil {
		return nil, false, err
	}
	if d.offset()-d.startOff+int64(n) > d.Limits.MaxValueBytes {
		return nil, false, &LimitError{
			Offset: d.offset(),
			Limit:  "MaxValueBytes",
			Value:  d.offset() + int64(n),
			cause:  ErrExceedsMax,
		}
	}
	if int64(n) > d.Limits.MaxStringBytes {
		return nil, false, &LimitError{
			Offset: d.off,
			Limit:  "MaxStringBytes",
			Value:  int64(n),
			cause:  ErrExceedsMax,
		}
	}

	buf := make([]byte, n)
	if _, err := io.ReadFull(d.br, buf); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, false, err
	}

	return buf, true, nil
}

func (d *Decoder) error(err error) error {
	d.err = err
	return err
}

func (d *Decoder) fixLimits() {
	if d.Limits.MaxCaptureBytes <= 0 {
		d.Limits.MaxCaptureBytes = DefaultMaxCaptureBytes
	}
	if d.Limits.MaxDepth <= 0 {
		d.Limits.MaxDepth = DefaultMaxRecursionDepth
	}
	if d.Limits.MaxStringBytes <= 0 {
		d.Limits.MaxStringBytes = DefaultMaxStringBytes
	}
	if d.Limits.MaxValueBytes <= 0 {
		d.Limits.MaxValueBytes = DefaultMaxValueBytes
	}
}

func (d *Decoder) useCustomUnmarshal(v reflect.Value, u Unmarshaler) error {
	raw, err := d.readContainer()
	if err != nil {
		return fillType(err, v.Type())
	}

	err = u.UnmarshalBencode(raw)
	if err != nil {
		return &TypeError{Offset: d.off, Type: v.Type(), cause: err}
	}
	return nil
}

func (d *Decoder) decodeRawValue(v reflect.Value) error {
	raw, err := d.readContainer()
	if err != nil {
		return fillType(err, rawMessageType)
	}
	v.SetBytes(slices.Clone(raw))
	return nil
}

func (d *Decoder) readContainer() ([]byte, error) {
	_, err := d.br.Peek(1) // §5.3
	if err != nil {
		isEOF := errors.Is(err, io.EOF)
		if isEOF && d.depth > 1 {
			return nil, io.ErrUnexpectedEOF
		} else if isEOF {
			return nil, err
		}
		return nil, &TypeError{Offset: d.off, cause: err}
	}
	startOffset := d.offset()
	d.captureStartOff = startOffset
	d.startRecorder(startOffset)
	defer d.rec.resetBuf()
	defer d.rec.off()

	// [Decoder.decode] increments depth, then [Decoder.skipValue] increments again, so decrement before calling latter
	d.depth--
	defer func() { d.depth++ }() // compensate defer depth--
	err = d.skipValue()
	if err != nil {
		return nil, err
	}
	endOffset := d.offset()
	raw, err := d.rec.getBuf(startOffset, endOffset)
	if err != nil {
		return nil, &TypeError{Offset: d.off, cause: err}
	}
	return raw, nil
}

func (d *Decoder) offset() int64 {
	return d.rec.pulled - int64(d.br.Buffered())
}

func (d *Decoder) snapshotOffset() { d.off = d.offset() }

// startRecorder must be called only on a value boundary, before the first byte is read
//
// it uses [bufio.Reader.Peek] which prevents a [bufio.Reader.UnreadByte] or [bufio.Reader.UnreadRune] call from succeeding
func (d *Decoder) startRecorder(start int64) {
	// ignore err because it won't return error, because we are peeking data only from buffer and not going over to trigger fill()
	p, _ := d.br.Peek(d.br.Buffered())
	// seed recorder's buf with decoder's, because some data might be in buffer which got there before recorder is turned on
	d.rec.appendBuf(p)
	d.rec.setBase(start)
	d.rec.on()
}

type recorder struct {
	src    io.Reader
	pulled int64
	buf    []byte
	// base acts as offset for buf[0] => (when rec on) data[start:end] = buf[start-base : end-base]
	// data[7:26] in rec buf might look like buf[0:19]
	base int64
	isOn bool
}

func (r *recorder) on()  { r.isOn = true }
func (r *recorder) off() { r.isOn = false }

func (r *recorder) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	r.pulled += int64(n)
	if r.isOn && n > 0 {
		r.buf = append(r.buf, p[:n]...)
	}
	return n, err
}

func (r *recorder) getBuf(start, end int64) ([]byte, error) {
	if end < start {
		return nil, fmt.Errorf("%w: end must be more than start", ErrInternal)
	}
	if start < r.base {
		return nil, fmt.Errorf("%w: start must be more or equal to r.base", ErrInternal)
	}
	if int64(len(r.buf)) < end-r.base {
		return nil, fmt.Errorf("%w: end is out-of-bounds", ErrInternal)
	}
	return r.buf[start-r.base : end-r.base], nil
}

// appendBuf appends data into [recorder.buf]
//
// note: this currently only used for populating [recorder.buf]
// with data from [bufio.Reader]'s buffer
// that get there before turning on recorder,
// thus I thought that it's ok not to check for [Limits.MaxCaptureBytes] here
func (r *recorder) appendBuf(data []byte) {
	r.buf = append(r.buf, data...)
}

func (r *recorder) setBase(newBase int64) {
	r.base = newBase
}

func (r *recorder) resetBuf() {
	if cap(r.buf) > maxRecBufRetained {
		r.buf = nil
	} else {
		r.buf = r.buf[:0]
	}
}

// indirect unwraps pointers, if nil set zero value.
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

func indirectType(t reflect.Type) reflect.Type {
	for {
		switch t.Kind() {
		case reflect.Pointer:
			t = t.Elem()
		default:
			return t
		}
	}
}

func fillType(err error, rt reflect.Type) error {
	if te, ok := errors.AsType[*TypeError](err); ok {
		if te.Type == nil {
			te.Type = rt
		}
	}

	return err
}

func customUnmarshaler[T any](v reflect.Value) (T, bool) {
	var zero T

	if v.Kind() != reflect.Pointer && v.CanAddr() {
		v = v.Addr()
	}

	if !v.CanInterface() {
		return zero, false
	}

	u, ok := v.Interface().(T)
	if !ok {
		return zero, false
	}
	return u, true
}
