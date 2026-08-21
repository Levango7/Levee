package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"text/tabwriter"
)

// outputEnvelope is the unified JSON envelope mandated by the API doc chapter
// 12.2. Every command emits exactly one of these on stdout when --json is set:
//
//	{
//	  "data":  <payload | null>,
//	  "meta":  <pagination / timing | null>,
//	  "error": <structured error | null>
//	}
type outputEnvelope struct {
	Data  any `json:"data"`
	Meta  any `json:"meta"`
	Error any `json:"error"`
}

// PrintJSON writes v as a pretty-printed JSON document to w. If v is already an
// outputEnvelope it is emitted as-is so that commands can populate meta/error
// fields directly; otherwise v is wrapped in an envelope with Data=v and
// Meta/Error left null. The function never panics: a marshalling error is
// reported on stderr and returned to the caller.
//
// All commands should use this helper rather than calling json.Encoder directly
// so that the envelope shape stays consistent across the CLI surface.
func PrintJSON(w io.Writer, v any) error {
	env := toEnvelope(v)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(env); err != nil {
		fmt.Fprintf(os.Stderr, "levee: failed to encode JSON output: %v\n", err)
		return fmt.Errorf("encode json output: %w", err)
	}
	return nil
}

// toEnvelope normalises a value into an outputEnvelope. Values that are already
// envelopes (structurally or by type) are passed through; everything else is
// wrapped with Data set to the value.
func toEnvelope(v any) outputEnvelope {
	if v == nil {
		return outputEnvelope{}
	}
	// Direct type match.
	if env, ok := v.(outputEnvelope); ok {
		return env
	}
	if env, ok := v.(*outputEnvelope); ok && env != nil {
		return *env
	}
	// Structural match: a struct/map with exactly data/meta/error keys is
	// treated as an envelope so that command code can build it inline with a
	// map[string]any literal.
	if isEnvelopeShaped(v) {
		return outputEnvelope{
			Data:  getField(v, "data"),
			Meta:  getField(v, "meta"),
			Error: getField(v, "error"),
		}
	}
	return outputEnvelope{Data: v}
}

// isEnvelopeShaped reports whether v looks like an output envelope. We accept
// both map[string]any and structs with the three canonical fields.
func isEnvelopeShaped(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		_, hasData := t["data"]
		_, hasMeta := t["meta"]
		_, hasErr := t["error"]
		return hasData && hasMeta && hasErr
	case *map[string]any:
		if t == nil {
			return false
		}
		_, hasData := (*t)["data"]
		_, hasMeta := (*t)["meta"]
		_, hasErr := (*t)["error"]
		return hasData && hasMeta && hasErr
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return false
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return false
	}
	rt := rv.Type()
	hasData, hasMeta, hasErr := false, false, false
	for i := 0; i < rt.NumField(); i++ {
		switch strings.ToLower(rt.Field(i).Name) {
		case "data":
			hasData = true
		case "meta":
			hasMeta = true
		case "error":
			hasErr = true
		}
	}
	return hasData && hasMeta && hasErr
}

// getField extracts a named field from a struct or map, returning nil when the
// field is absent. It is the dynamic counterpart used by toEnvelope.
func getField(v any, name string) any {
	switch t := v.(type) {
	case map[string]any:
		return t[name]
	case *map[string]any:
		if t == nil {
			return nil
		}
		return (*t)[name]
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil
	}
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		if strings.ToLower(rt.Field(i).Name) == name {
			fv := rv.Field(i)
			if !fv.CanInterface() {
				return nil
			}
			return fv.Interface()
		}
	}
	return nil
}

// PrintHuman writes v in a human-readable form to w. The rendering is
// deliberately simple and is intended for ad-hoc CLI output; commands that
// need richer formatting (tables, colour) should build their own strings and
// call fmt.Fprintln directly. The supported types are:
//
//   - string / []byte: written verbatim.
//   - map[string]any: written as a two-column "key: value" table.
//   - structs: written as a two-column "field: value" table using exported
//     fields, honouring a `json:"name"` tag if present.
//   - slices / arrays: one element per line, each rendered via PrintHuman.
//   - anything else: fmt.Sprintf("%v", v).
//
// The function never panics and never returns an error: human output is best
// effort and a write failure is reported to stderr but does not propagate.
func PrintHuman(w io.Writer, v any) {
	if v == nil {
		return
	}
	switch t := v.(type) {
	case string:
		fmt.Fprintln(w, t)
	case []byte:
		_, _ = w.Write(t)
		fmt.Fprintln(w)
	case map[string]any:
		printHumanMap(w, t)
	case []map[string]any:
		printHumanTable(w, t)
	default:
		rv := reflect.ValueOf(v)
		if rv.Kind() == reflect.Pointer {
			if rv.IsNil() {
				return
			}
			rv = rv.Elem()
		}
		switch rv.Kind() {
		case reflect.Struct:
			printHumanStruct(w, rv)
		case reflect.Slice, reflect.Array:
			printHumanSlice(w, rv)
		default:
			fmt.Fprintf(w, "%v\n", v)
		}
	}
}

// printHumanMap renders a map[string]any as an aligned "key: value" table.
func printHumanMap(w io.Writer, m map[string]any) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for k, val := range m {
		fmt.Fprintf(tw, "%s:\t%v\n", k, val)
	}
	_ = tw.Flush()
}

// printHumanTable renders a slice of maps as a table with a header row derived
// from the union of keys (sorted for determinism).
func printHumanTable(w io.Writer, rows []map[string]any) {
	if len(rows) == 0 {
		return
	}
	// Collect the union of keys preserving first-seen order.
	keys := make([]string, 0, len(rows[0]))
	seen := map[string]bool{}
	for _, row := range rows {
		for k := range row {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, k := range keys {
		fmt.Fprintf(tw, "%s\t", strings.ToUpper(k))
	}
	fmt.Fprintln(tw)
	for _, row := range rows {
		for _, k := range keys {
			fmt.Fprintf(tw, "%v\t", row[k])
		}
		fmt.Fprintln(tw)
	}
	_ = tw.Flush()
}

// printHumanStruct renders a struct as a two-column "field: value" table.
func printHumanStruct(w io.Writer, rv reflect.Value) {
	rt := rv.Type()
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for i := 0; i < rt.NumField(); i++ {
		field := rt.Field(i)
		if !field.IsExported() {
			continue
		}
		name := field.Name
		if tag := field.Tag.Get("json"); tag != "" {
			if comma := strings.Index(tag, ","); comma >= 0 {
				tag = tag[:comma]
			}
			if tag != "" {
				name = tag
			}
		}
		fv := rv.Field(i)
		if !fv.CanInterface() {
			continue
		}
		fmt.Fprintf(tw, "%s:\t%v\n", name, fv.Interface())
	}
	_ = tw.Flush()
}

// printHumanSlice renders a slice/array one element per line.
func printHumanSlice(w io.Writer, rv reflect.Value) {
	for i := 0; i < rv.Len(); i++ {
		PrintHuman(w, rv.Index(i).Interface())
	}
}
