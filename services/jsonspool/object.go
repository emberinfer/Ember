// Package jsonspool indexes JSON objects on disk without retaining array payloads.
package jsonspool

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

type Span struct{ Start, End int64 }
type Field struct {
	Name    string
	Span    Span
	Entries []Span
	Array   bool
}
type Object struct {
	Ordered []Field
	File    *os.File
	Fields  map[string]Span
	Arrays  map[string][]Span
}

// Index consumes exactly one object. Arrays are decoded one element at a time;
// json.Decoder never sees a complete large result or evidence array.
func Index(f *os.File) (*Object, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	d := json.NewDecoder(bufio.NewReaderSize(f, 64<<10))
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	if t != json.Delim('{') {
		return nil, fmt.Errorf("expected JSON object")
	}
	o := &Object{File: f, Fields: map[string]Span{}, Arrays: map[string][]Span{}}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return nil, err
		}
		name := key.(string)
		start := d.InputOffset()
		// Find the value after the colon without teaching Decoder about large arrays.
		var b [1]byte
		for {
			if _, err = f.ReadAt(b[:], start); err != nil {
				return nil, err
			}
			if b[0] != ':' && b[0] != ' ' && b[0] != '\n' && b[0] != '\r' && b[0] != '\t' {
				break
			}
			start++
		}
		if b[0] == '[' {
			if _, err = d.Token(); err != nil {
				return nil, err
			}
			entries := []Span{}
			for d.More() {
				begin := d.InputOffset()
				var raw json.RawMessage
				if err = d.Decode(&raw); err != nil {
					return nil, err
				}
				end := d.InputOffset()
				begin = end - int64(len(raw))
				entries = append(entries, Span{begin, end})
			}
			if _, err = d.Token(); err != nil {
				return nil, err
			}
			o.Arrays[name] = entries
		} else {
			delete(o.Arrays, name)
			var raw json.RawMessage
			if err = d.Decode(&raw); err != nil {
				return nil, err
			}
		}
		o.Fields[name] = Span{start, d.InputOffset()}
		o.Ordered = append(o.Ordered, Field{Name: name, Span: o.Fields[name], Entries: o.Arrays[name], Array: b[0] == '['})
	}
	if _, err = d.Token(); err != nil {
		return nil, err
	}
	var extra json.RawMessage
	if err = d.Decode(&extra); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("extra JSON value")
		}
		return nil, err
	}
	return o, nil
}
func (o *Object) Reader(s Span) *io.SectionReader {
	return io.NewSectionReader(o.File, s.Start, s.End-s.Start)
}
func (o *Object) Decode(name string, v any) error {
	s, ok := o.Fields[name]
	if !ok {
		return nil
	}
	return json.NewDecoder(o.Reader(s)).Decode(v)
}
func (o *Object) DecodeSpan(s Span, v any) error { return json.NewDecoder(o.Reader(s)).Decode(v) }

// WriteMerge emits the same compact, HTML-escaped JSON as json.Marshal(metadata)
// with selected RawMessage fields replaced by their on-disk values. Metadata's
// field order and omission decisions are retained.
func WriteMerge(w io.Writer, metadata []byte, source *Object, fields ...string) error {
	return WriteMergeTransform(w, metadata, source, nil, fields...)
}

func WriteMergeTransform(w io.Writer, metadata []byte, source *Object, transforms map[string]func(io.Writer) error, fields ...string) error {
	replace := map[string]bool{}
	for _, field := range fields {
		replace[field] = true
	}
	d := json.NewDecoder(bytes.NewReader(metadata))
	if _, err := d.Token(); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "{"); err != nil {
		return err
	}
	first := true
	for d.More() {
		k, err := d.Token()
		if err != nil {
			return err
		}
		key := k.(string)
		var raw json.RawMessage
		if err = d.Decode(&raw); err != nil {
			return err
		}
		if !first {
			if _, err = io.WriteString(w, ","); err != nil {
				return err
			}
		}
		first = false
		name, _ := json.Marshal(key)
		if _, err = w.Write(name); err != nil {
			return err
		}
		if _, err = io.WriteString(w, ":"); err != nil {
			return err
		}
		if s, ok := source.Fields[key]; ok && replace[key] {
			if transform := transforms[key]; transform != nil {
				err = transform(w)
			} else {
				err = Compact(w, source.Reader(s))
			}
		} else {
			_, err = w.Write(raw)
		}
		if err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "}")
	return err
}

// Compact removes insignificant whitespace in fixed-size chunks. Result evidence
// is ASCII JSON (hex payloads); escaping matches encoding/json's RawMessage path.
func Compact(w io.Writer, r io.Reader) error {
	in, escape := false, false
	src := make([]byte, (64<<10)+2)
	dst := make([]byte, 0, 64<<10)
	pending := 0
	for {
		n, err := r.Read(src[pending:])
		available := pending + n
		limit := available
		if err == nil {
			limit -= 2
			if limit < 0 {
				limit = 0
			}
		}
		dst = dst[:0]
		i := 0
		for i < limit {
			b := src[i]
			if in {
				if !escape && b == 0xe2 && i+2 < available && src[i+1] == 0x80 && (src[i+2] == 0xa8 || src[i+2] == 0xa9) {
					dst = append(dst, '\\', 'u', '2', '0', '2', "89"[src[i+2]-0xa8])
					i += 3
					continue
				}
				if !escape && (b == '<' || b == '>' || b == '&') {
					const h = "0123456789abcdef"
					dst = append(dst, '\\', 'u', '0', '0', h[b>>4], h[b&15])
				} else {
					dst = append(dst, b)
				}
				if escape {
					escape = false
				} else if b == '\\' {
					escape = true
				} else if b == '"' {
					in = false
				}
			} else {
				if b == '"' {
					in = true
				}
				if b != ' ' && b != '\n' && b != '\r' && b != '\t' {
					dst = append(dst, b)
				}
			}
			i++
		}
		if _, e := w.Write(dst); e != nil {
			return e
		}
		pending = copy(src, src[i:available])
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// Metadata reads only fields other than the supplied large evidence fields.
func (o *Object) Metadata(exclude ...string) ([]byte, error) {
	skip := map[string]bool{}
	for _, k := range exclude {
		skip[k] = true
	}
	fields := map[string]json.RawMessage{}
	for k, s := range o.Fields {
		if skip[k] {
			fields[k] = json.RawMessage("null")
			continue
		}
		var raw json.RawMessage
		if err := o.DecodeSpan(s, &raw); err != nil {
			return nil, err
		}
		fields[k] = raw
	}
	return json.Marshal(fields)
}
