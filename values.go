package zapsmtp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// String returns a pointer to v, for optional string fields of params.
func String(v string) *string { return &v }

// Int returns a pointer to v, for optional integer fields of params.
func Int(v int) *int { return &v }

// Bool returns a pointer to v, for optional boolean fields of params.
func Bool(v bool) *bool { return &v }

// Float returns a pointer to v, for optional number fields of params.
func Float(v float64) *float64 { return &v }

// Ptr returns a pointer to v, for optional fields of any type, such as an enum:
// Stream: zapsmtp.Ptr(zapsmtp.StreamBroadcast).
func Ptr[T any](v T) *T { return &v }

// Nullable is a params field that can be left out (a nil *Nullable), set to a
// value (NewNullable), or set to null to clear it (Null).
type Nullable[T any] struct {
	value T
	set   bool
}

// NewNullable returns a Nullable set to v.
func NewNullable[T any](v T) *Nullable[T] { return &Nullable[T]{value: v, set: true} }

// Null returns a Nullable that sends null: it clears the field.
func Null[T any]() *Nullable[T] { return &Nullable[T]{} }

// Get returns the value, and false when it is null.
func (n Nullable[T]) Get() (T, bool) { return n.value, n.set }

// MarshalJSON writes the value, or null.
func (n Nullable[T]) MarshalJSON() ([]byte, error) {
	if !n.set {
		return []byte("null"), nil
	}
	return json.Marshal(n.value)
}

// marshalObject writes a params struct as a JSON object, the way Go 1.24's
// omitzero would: a field tagged omitzero is left out when it is the zero
// value (a nil pointer, slice, map or interface), so an empty, non-nil slice or
// map is sent as [] or {}. On update that clears the field.
func marshalObject(v any) ([]byte, error) {
	rv := reflect.ValueOf(v)
	var b bytes.Buffer
	b.WriteByte('{')
	for i := range rv.NumField() {
		name, opt, _ := strings.Cut(rv.Type().Field(i).Tag.Get("json"), ",")
		f := rv.Field(i)
		if name == "" || name == "-" || opt == "omitzero" && f.IsZero() {
			continue
		}
		val, err := json.Marshal(f.Interface())
		if err != nil {
			return nil, err
		}
		if b.Len() > 1 {
			b.WriteByte(',')
		}
		key, _ := json.Marshal(name)
		b.Write(key)
		b.WriteByte(':')
		b.Write(val)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// decodeLenient decodes JSON data into the struct v points to without ever
// failing: when the whole value does not decode, it starts over field by field
// and leaves out each field that does not (a field of an unexpected type, a
// date that is not RFC 3339). Data that is not a JSON object leaves v empty.
func decodeLenient(data []byte, v any) {
	if json.Unmarshal(data, v) == nil {
		return
	}
	rv := reflect.ValueOf(v).Elem()
	rv.SetZero()
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return
	}
	t := rv.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		name := jsonName(f)
		raw, ok := fields[name]
		if !ok || name == "" {
			continue
		}
		fv := reflect.New(f.Type)
		if json.Unmarshal(raw, fv.Interface()) == nil {
			rv.Field(i).Set(fv.Elem())
		}
	}
}

// jsonName is the JSON name of an exported field with a json tag, else "".
func jsonName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	if !f.IsExported() || name == "-" {
		return ""
	}
	return name
}

// encodeQuery turns the url-tagged fields of a params struct into a query:
// nil pointers are left out, slices repeat the name, maps become name[key].
func encodeQuery(params any) url.Values {
	q := url.Values{}
	v := reflect.ValueOf(params)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return q
	}
	v = v.Elem()
	for i := range v.NumField() {
		if name := v.Type().Field(i).Tag.Get("url"); name != "" {
			addQuery(q, name, v.Field(i))
		}
	}
	return q
}

func addQuery(q url.Values, name string, v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			addQuery(q, name, v.Elem())
		}
	case reflect.Slice:
		for i := range v.Len() {
			q.Add(name, queryValue(v.Index(i)))
		}
	case reflect.Map:
		keys := v.MapKeys()
		slices.SortFunc(keys, func(a, b reflect.Value) int { return strings.Compare(a.String(), b.String()) })
		for _, k := range keys {
			q.Add(name+"["+k.String()+"]", queryValue(v.MapIndex(k)))
		}
	default:
		q.Add(name, queryValue(v))
	}
}

func queryValue(v reflect.Value) string {
	switch v.Kind() {
	case reflect.String:
		return v.String()
	case reflect.Bool:
		return strconv.FormatBool(v.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10)
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(v.Float(), 'f', -1, 64)
	}
	return fmt.Sprint(v.Interface())
}
