package bencoderef

import (
	"bufio"
	"bytes"
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
	maxInt64Digits    = 20
)

var (
	ErrInternal        = errors.New("internal error")
	ErrUnsupportedType = errors.New("unsupported type")
	ErrSyntax          = errors.New("syntax error")

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

var errCaptureTooLarge = errors.New("capture limit exceeded")

type RawMessage []byte

var rawMessageType = reflect.TypeFor[RawMessage]()

var fieldCache sync.Map

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
		return "cannot unmarshal " + e.Value + " into Go struct field " + e.Struct + "." + e.Field + " of type " + e.Type.String()
	}
	return "cannot unmarshal " + e.Value + " into Go value of type " + e.Type.String()
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

	br       *bufio.Reader
	rec      *recorder
	depth    uint
	err      error
	off      int64 // snapshotted offset
	startOff int64
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
	d := NewDecoder(bytes.NewBuffer(b))
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

			idx, ok := fields[string(key)]
			if !ok {
				err = d.skipValue()
				if err != nil {
					return &TypeError{Offset: d.offset(), Value: "dict", Type: t, cause: err}
				}
				continue
			}

			if err := d.decode(v.Field(idx)); err != nil {
				if tErr, ok := errors.AsType[*TypeError](err); ok {
					tErr.Struct = t.Name()
					tErr.Field = t.Field(idx).Name
				}
				return err
			}
		}
	case reflect.Map, reflect.Interface:
		t := v.Type()
		if k == reflect.Interface {
			t = reflect.TypeFor[map[string]any]()
		}
		if t.Key().Kind() != reflect.String {
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

			fieldDst := reflect.New(t.Elem())
			if err := d.decode(fieldDst.Elem()); err != nil {
				return err
			}

			m.SetMapIndex(reflect.ValueOf(key).Convert(t.Key()), fieldDst.Elem())
		}
		v.Set(m)
	default:
		return &TypeError{Offset: d.off, Value: "dict", Type: t, cause: ErrTypeMismatch}
	}

	return nil
}

type fieldCandidate struct {
	isExplicit bool
	index      int
}

func cachedFields(t reflect.Type) map[string]int {
	if f, ok := fieldCache.Load(t); ok {
		return f.(map[string]int)
	}
	f, _ := fieldCache.LoadOrStore(t, buildFields(t))
	return f.(map[string]int)
}

func buildFields(t reflect.Type) map[string]int {
	candidates := make(map[string][]fieldCandidate, t.NumField())

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
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

		candidates[name] = append(candidates[name], fieldCandidate{isExplicit, i})
	}

	fields := make(map[string]int, len(candidates))
	for name, cs := range candidates {
		if winner, ok := resolveCandidates(cs); ok {
			fields[name] = winner.index
		}
	}

	return fields
}

func resolveCandidates(cs []fieldCandidate) (fieldCandidate, bool) {
	if len(cs) == 1 {
		return cs[0], true
	}

	var winner fieldCandidate
	explicit := 0
	for _, c := range cs {
		if c.isExplicit {
			explicit++
			winner = c
		}
	}
	if explicit == 1 {
		return winner, true
	}
	return fieldCandidate{}, false
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
		s := reflect.New(reflect.ArrayOf(v.Len(), v.Type().Elem())).Elem()

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
				err = d.skipValue()
				if err != nil {
					return &TypeError{Offset: d.off, Value: "list", Type: t, cause: err}
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
		err := &LimitError{
			Offset: d.off,
			Limit:  "MaxStringBytes",
			Value:  int64(lengthInt),
			cause:  ErrExceedsMax,
		}
		return &TypeError{Offset: d.off, Value: "string", Type: t, cause: err}
	}

	str := make([]byte, lengthInt)
	_, err = io.ReadFull(d.br, str)
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return &TypeError{Offset: d.off, Value: "string", Type: t, cause: err}
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

		reflect.Copy(v, reflect.ValueOf(str))
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

	_, err = d.br.Discard(lengthInt)
	if err != nil {
		return err
	}

	return nil
}

func (d *Decoder) readIntSlice(delim byte) ([]byte, error) {
	var buf []byte
	var n int64
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
		if (b < '0' || b > '9') && (b != '-' || n != 0) {
			return nil, &SyntaxError{
				Offset: d.off,
				msg:    fmt.Sprintf("must be digit or `-`, got: %q", b),
				cause:  ErrSyntax,
			}
		}
		if n > maxInt64Digits {
			return nil, &SyntaxError{
				Offset: d.off,
				msg:    fmt.Sprintf("integer can only hold %d digits", maxInt64Digits),
				cause:  ErrExceedsMax,
			}
		}
		buf = append(buf, b)
		n++
	}
	if n == 0 {
		return nil, &SyntaxError{Offset: d.off, msg: "can't be empty", cause: ErrEmpty}
	}
	if n == 1 && buf[0] == '-' {
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
	return strconv.Atoi(string(buf))
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
			msg:    "negavie zero is forbidden",
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
	if errors.Is(err, errCaptureTooLarge) {
		err = &LimitError{
			Offset: d.offset(),
			Limit:  "MaxCaptureBytes",
			Value:  d.Limits.MaxCaptureBytes,
			cause:  ErrExceedsMax,
		}
	}
	d.err = err
	return err
}

func (d *Decoder) fixLimits() {
	if d.Limits.MaxCaptureBytes == 0 {
		d.Limits.MaxCaptureBytes = DefaultMaxCaptureBytes
	}
	if d.Limits.MaxDepth == 0 {
		d.Limits.MaxDepth = DefaultMaxRecursionDepth
	}
	if d.Limits.MaxStringBytes == 0 {
		d.Limits.MaxStringBytes = DefaultMaxStringBytes
	}
	if d.Limits.MaxValueBytes == 0 {
		d.Limits.MaxValueBytes = DefaultMaxValueBytes
	}
}

func (d *Decoder) decodeRawValue(v reflect.Value) error {
	startOffset := d.offset()
	d.startRecorder(startOffset)
	defer d.rec.resetBuf()
	defer d.rec.off()

	err := d.skipValue()
	if err != nil {
		return &TypeError{Offset: d.off, Type: rawMessageType, cause: err}
	}
	endOffset := d.offset()
	raw, err := d.rec.getBuf(startOffset, endOffset)
	if err != nil {
		return &TypeError{Offset: d.off, Type: rawMessageType, cause: err}
	}
	v.SetBytes(slices.Clone(raw))
	return nil
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
	d.rec.on(d.Limits.MaxCaptureBytes)
}

type recorder struct {
	src    io.Reader
	pulled int64
	buf    []byte
	// base acts as offset for buf[0] => (when rec on) data[start:end] = buf[start-base : end-base]
	// data[7:26] in rec buf might look like buf[0:19]
	base int64
	isOn bool
	max  int64
}

func (r *recorder) on(max int64) { r.isOn, r.max = true, max }
func (r *recorder) off()         { r.isOn = false }

func (r *recorder) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	if r.isOn && n > 0 {
		if int64(len(r.buf))+int64(n) > r.max {
			return n, errCaptureTooLarge
		}
		r.buf = append(r.buf, p[:n]...)
	}
	r.pulled += int64(n)
	return n, err
}

func (r *recorder) getBuf(start, end int64) ([]byte, error) {
	if end < start {
		return nil, fmt.Errorf("%w: end must be more that start", ErrInternal)
	}
	if start < r.base {
		return nil, fmt.Errorf("%w: start must be more or equal to r.base", ErrInternal)
	}
	if int64(len(r.buf)) < end-r.base {
		return nil, fmt.Errorf("%w: end is out-of-bounds", ErrInternal)
	}
	return r.buf[start-r.base : end-r.base], nil
}

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
