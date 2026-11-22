package install

// Order-preserving JSON values so merging hooks into someone's settings file
// keeps their key order (and therefore produces a minimal, readable diff).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Object is a JSON object with ordered keys.
type Object struct {
	Keys []string
	Vals map[string]any
}

// NewObject returns an empty object.
func NewObject() *Object { return &Object{Vals: map[string]any{}} }

// Get returns the value for key.
func (o *Object) Get(key string) (any, bool) {
	v, ok := o.Vals[key]
	return v, ok
}

// Set sets key, appending it if new.
func (o *Object) Set(key string, v any) {
	if _, ok := o.Vals[key]; !ok {
		o.Keys = append(o.Keys, key)
	}
	o.Vals[key] = v
}

// Delete removes key.
func (o *Object) Delete(key string) {
	if _, ok := o.Vals[key]; !ok {
		return
	}
	delete(o.Vals, key)
	for i, k := range o.Keys {
		if k == key {
			o.Keys = append(o.Keys[:i], o.Keys[i+1:]...)
			break
		}
	}
}

// Len is the number of keys.
func (o *Object) Len() int { return len(o.Keys) }

// Parse decodes JSON into *Object / []any / string / json.Number / bool / nil.
func Parse(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := parseValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("unexpected data after top-level JSON value")
	}
	return v, nil
}

func parseValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := NewObject()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("invalid object key %v", kt)
				}
				val, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				obj.Set(key, val) // duplicate keys: last wins, like encoding/json
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return obj, nil
		case '[':
			arr := []any{}
			for dec.More() {
				val, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, val)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return arr, nil
		}
		return nil, fmt.Errorf("unexpected delimiter %v", t)
	default:
		return t, nil
	}
}

// Marshal encodes v with two-space indentation and a trailing newline.
func Marshal(v any) []byte {
	var b bytes.Buffer
	writeValue(&b, v, 0)
	b.WriteByte('\n')
	return b.Bytes()
}

func writeValue(b *bytes.Buffer, v any, depth int) {
	indent := strings.Repeat("  ", depth+1)
	closeIndent := strings.Repeat("  ", depth)
	switch t := v.(type) {
	case *Object:
		if t.Len() == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		for i, k := range t.Keys {
			b.WriteString(indent)
			writeString(b, k)
			b.WriteString(": ")
			writeValue(b, t.Vals[k], depth+1)
			if i < len(t.Keys)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(closeIndent + "}")
	case []any:
		if len(t) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i, item := range t {
			b.WriteString(indent)
			writeValue(b, item, depth+1)
			if i < len(t)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(closeIndent + "]")
	case string:
		writeString(b, t)
	case json.Number:
		b.WriteString(t.String())
	case int:
		fmt.Fprintf(b, "%d", t)
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case nil:
		b.WriteString("null")
	default:
		data, _ := json.Marshal(t)
		b.Write(data)
	}
}

func writeString(b *bytes.Buffer, s string) {
	var tmp bytes.Buffer
	enc := json.NewEncoder(&tmp)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	b.Write(bytes.TrimRight(tmp.Bytes(), "\n"))
}
