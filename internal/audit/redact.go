package audit

// Redaction vocabulary and walker (SA-009 / SA-010).
//
// Redact replaces the values of sensitive fields with "[REDACTED]" before
// trace details reach the WORM chain. The vocabulary below matches
// encoding/json output shapes: field names are matched case-insensitively,
// with exact hits plus word-boundary suffix hits on '_' and '-' ("db_password"
// matches "password", "access-token" matches "token"). The bare word "key"
// is matched ONLY as an exact hit — it is too common as a substring
// ("sort_key", "monkey") and broad suffix matching would over-redact
// legitimate fields into useless audit records.
//
// Known limitation (documented, deliberately not fixed this round, in the
// spirit of SA-011): secrets EMBEDDED IN VALUES — e.g. the error string
// "auth failed for secret=hunter2" recorded by buildDetail — carry no key
// context and can never be caught by key-name matching.

import (
	"encoding"
	"encoding/json"
	"reflect"
	"strings"
	"sync/atomic"
	"time"
)

// sensitiveFields is the set of field names whose values are replaced with
// "[REDACTED]" by Redact. Matching is case-insensitive; see the vocabulary
// header for the exact/suffix rule.
var sensitiveFields = map[string]struct{}{
	"password":          {},
	"passwd":            {},
	"key":               {},
	"token":             {},
	"secret":            {},
	"credential":        {},
	"private_key":       {},
	"api_key":           {},
	"passphrase":        {},
	"auth_code":         {},
	"refresh_token":     {},
	"access_token":      {},
	"ssh_key":           {},
	"cert":              {},
	"certificate":       {},
	"connection_string": {},
}

// bareExactOnlyWords are vocabulary entries matched only by exact name
// (no boundary-suffix matching), because they are too generic as suffixes.
var bareExactOnlyWords = map[string]struct{}{
	"key": {},
}

// redactedValue is the placeholder substituted for sensitive field values.
const redactedValue = "[REDACTED]"

// maxRedactDepth bounds recursion through composites. Depth is the PRIMARY
// cycle guard: the visited set only works for maps and pointers (value
// semantics have no stable address), so reference cycles through raw pointers
// — impossible in normal JSON/YAML-derived data — are caught here instead.
const maxRedactDepth = 8

// extraSensitiveFields holds the process-wide user-supplied vocabulary from
// security.sensitive_fields, lower-cased and merged with the built-in set at
// match time. It is an atomic snapshot: Redact runs on hot audit paths from
// many goroutines while SetSensitiveFields is called once at startup.
var extraSensitiveFields atomic.Pointer[map[string]struct{}]

// SetSensitiveFields registers additional sensitive field names (the
// security.sensitive_fields config key) on top of the built-in vocabulary.
// Names are matched with the same rules as built-in entries (case-insensitive,
// exact or '_'-/'-'-boundary suffix). Passing an empty slice clears the
// extras. Fields are NOT existence-checked: unknown names are harmless.
func SetSensitiveFields(fields []string) {
	snap := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		f = strings.ToLower(strings.TrimSpace(f))
		if f == "" {
			continue
		}
		snap[f] = struct{}{}
	}
	extraSensitiveFields.Store(&snap)
}

// extraSensitive returns the current user-supplied vocabulary snapshot.
func extraSensitive() map[string]struct{} {
	if p := extraSensitiveFields.Load(); p != nil {
		return *p
	}
	return nil
}

// isSensitive reports whether key names a sensitive field. Matching is
// case-insensitive: an exact vocabulary hit, or a boundary-suffix hit
// (the field ends with the vocabulary word preceded by '_' or '-') for
// every word except the bareExactOnlyWords.
func isSensitive(key string) bool {
	lk := strings.ToLower(key)
	if matchesVocabulary(lk, sensitiveFields) {
		return true
	}
	return matchesVocabulary(lk, extraSensitive())
}

// matchesVocabulary reports whether the lower-cased field name hits vocab,
// by exact match or by word-boundary suffix match.
func matchesVocabulary(lk string, vocab map[string]struct{}) bool {
	if len(vocab) == 0 {
		return false
	}
	if _, ok := vocab[lk]; ok {
		return true
	}
	for word := range vocab {
		if _, bare := bareExactOnlyWords[word]; bare {
			continue
		}
		if boundarySuffixHit(lk, word) {
			return true
		}
	}
	return false
}

// boundarySuffixHit reports whether field ends with word preceded by a '_'
// or '-' separator: "db_password" hits "password", "access-token" hits
// "token", but "passwordstore" does not hit "password".
func boundarySuffixHit(field, word string) bool {
	if len(field) <= len(word) || !strings.HasSuffix(field, word) {
		return false
	}
	sep := field[len(field)-len(word)-1]
	return sep == '_' || sep == '-'
}

// Redact returns a copy of input with every sensitive field replaced by
// "[REDACTED]". Matching is case-insensitive and recursive: nested maps,
// string-keyed maps, slices and STRUCTS (including slices/arrays of structs
// reached through any container) are walked and redacted. Non-composite
// values are returned unchanged.
//
// Structs are normalised into map[string]any (which json.Marshal renders
// identically) ONLY when their subtree contains at least one hit; otherwise
// the original value is passed through untouched, so audit records of
// secret-free structs keep their exact prior shape and allocation profile.
//
// Redact never mutates the input map; it always returns a fresh map (or the
// original value when input is not a map).
func Redact(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	out := make(map[string]any, len(input))
	for k, v := range input {
		out[k] = redactValueForKey(k, v)
	}
	return out
}

// RedactStringMap returns a copy of m with every sensitive key's value
// replaced by "[REDACTED]". It exists because trace details carry string
// maps too (e.g. TraceRecord.Metadata), which previously bypassed redaction
// entirely. Matching is case-insensitive; a nil input yields nil.
func RedactStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if isSensitive(k) {
			out[k] = redactedValue
			continue
		}
		out[k] = v
	}
	return out
}

// redactValueForKey redacts a single value given its key. If the key is
// sensitive the value is replaced with "[REDACTED]"; otherwise the value is
// recursively redacted when it is a composite (map, slice or struct).
func redactValueForKey(key string, value any) any {
	if isSensitive(key) {
		return redactedValue
	}
	return redactCompositeDepth(value, 0, nil)
}

// redactCompositeDepth walks a composite value at the given nesting depth,
// threading the cycle-detection visited set (lazily allocated, only maps and
// pointers register addresses).
func redactCompositeDepth(value any, depth int, visited map[uintptr]struct{}) any {
	if depth > maxRedactDepth {
		return value
	}
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = redactValueForKeyDepth(k, item, depth, visited)
		}
		return out
	case map[string]string:
		return RedactStringMap(v)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = redactCompositeDepth(item, depth+1, visited)
		}
		return out
	default:
		red, changed := redactReflect(reflect.ValueOf(value), depth+1, visited)
		if !changed {
			return value
		}
		return red
	}
}

// redactValueForKeyDepth is redactValueForKey with depth/visited threading.
func redactValueForKeyDepth(key string, value any, depth int, visited map[uintptr]struct{}) any {
	if isSensitive(key) {
		return redactedValue
	}
	return redactCompositeDepth(value, depth+1, visited)
}

var (
	timeType        = reflect.TypeOf(time.Time{})
	rawMsgType      = reflect.TypeOf(json.RawMessage(nil))
	errorType       = reflect.TypeOf((*error)(nil)).Elem()
	marshalType     = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshalType = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
)

// isKnownLeaf reports whether t is a type the walker must NOT recurse into:
// time.Time, json.RawMessage, error implementations, types with custom JSON
// marshalling, and byte slices. Recursing into these would either expand
// implementation internals the JSON never shows (time.Time → wall/ext/loc),
// burn depth on arrays (e.g. [16]byte → 16 elements), or misrender the
// value.
func isKnownLeaf(t reflect.Type) bool {
	if t == timeType || t == rawMsgType {
		return true
	}
	pt := t
	if t.Kind() != reflect.Pointer {
		pt = reflect.PointerTo(t)
	}
	return t.Implements(errorType) || pt.Implements(errorType) ||
		t.Implements(marshalType) || t.Implements(textMarshalType)
}

// redactReflect walks an arbitrary reflected value without key context and
// reports whether a redaction copy must replace it. When changed is false the
// caller keeps the original value untouched.
func redactReflect(v reflect.Value, depth int, visited map[uintptr]struct{}) (any, bool) {
	if depth > maxRedactDepth || !v.IsValid() {
		return nil, false
	}
	t := v.Type()
	if isKnownLeaf(t) {
		return nil, false
	}
	switch t.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return nil, false
		}
		return redactReflect(v.Elem(), depth, visited)

	case reflect.Pointer:
		if v.IsNil() {
			return nil, false
		}
		ptr := v.Pointer()
		if visited != nil {
			if _, cyc := visited[ptr]; cyc {
				return nil, false
			}
		} else {
			visited = make(map[uintptr]struct{})
		}
		visited[ptr] = struct{}{}
		red, changed := redactReflect(v.Elem(), depth+1, visited)
		delete(visited, ptr)
		return red, changed

	case reflect.Map:
		// Non-string keys cannot appear in the JSON payload anyway; leave
		// such maps untouched rather than guessing a key encoding.
		if t.Key().Kind() != reflect.String || v.IsNil() {
			return nil, false
		}
		ptr := v.Pointer()
		if visited != nil {
			if _, cyc := visited[ptr]; cyc {
				return nil, false
			}
		} else {
			visited = make(map[uintptr]struct{})
		}
		visited[ptr] = struct{}{}
		defer delete(visited, ptr)
		out := make(map[string]any, v.Len())
		changed := false
		iter := v.MapRange()
		for iter.Next() {
			key := iter.Key().String()
			if isSensitive(key) {
				out[key] = redactedValue
				changed = true
				continue
			}
			val := iter.Value()
			if red, c := redactReflect(val, depth+1, visited); c {
				out[key] = red
				changed = true
			} else if val.CanInterface() {
				out[key] = val.Interface()
			} else {
				// Unreadable value (read-only path): keep marshal-equivalent
				// absence rather than risking a panic.
				continue
			}
		}
		if !changed {
			return nil, false
		}
		return out, true

	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return nil, false // []byte — opaque base64 in JSON
		}
		if v.IsNil() {
			return nil, false
		}
		out := make([]any, v.Len())
		changed := false
		for i := 0; i < v.Len(); i++ {
			item := v.Index(i)
			if red, c := redactReflect(item, depth+1, visited); c {
				out[i] = red
				changed = true
			} else if item.CanInterface() {
				out[i] = item.Interface()
			} else {
				out[i] = nil
			}
		}
		if !changed {
			return nil, false
		}
		return out, true

	case reflect.Struct:
		return redactStruct(v, depth, visited)

	default:
		// Arrays (e.g. [16]byte salts), primitives, chan/func/unsafe: leave
		// untouched — arrays must not be expanded element-wise.
		return nil, false
	}
}

// structField is one marshal-visible field of a struct, as decided by
// jsonFieldInfo.
type structField struct {
	name      string // JSON key ("" for promoted embedded structs)
	embedded  bool
	omitempty bool
	value     reflect.Value
}

// jsonFieldInfo decides a field's JSON key and skip behaviour, mirroring the
// subset of encoding/json naming rules the walker needs. The visibility bar
// is "json.Marshal visible": unexported fields are skipped (CanInterface() is
// false for them, and they never appear in the marshalled JSON), as are
// fields tagged json:"-".
//
// Known deviation (accepted by design): encoding/json ALSO promotes the
// exported fields of an unexported embedded struct type, but reflect exposes
// those values read-only (CanInterface() == false), so the walker cannot read
// them without unsafe. Instead of skipping blindly, redactStruct consults the
// TYPE metadata (field names are always readable): if any promoted JSON name
// in such a subtree is sensitive, the enclosing struct is rebuilt and the
// whole embedded subtree is OMITTED — over-removal is safe, leaking is not.
// Secret-free unexported embedded subtrees flow through untouched.
func jsonFieldInfo(f reflect.StructField) (sf structField, skip bool) {
	if !f.IsExported() {
		return structField{}, true
	}
	tag, tagged := f.Tag.Lookup("json")
	if tagged {
		parts := strings.Split(tag, ",")
		if tag == "-" {
			return structField{}, true
		}
		sf.name = parts[0]
		for _, o := range parts[1:] {
			if o == "omitempty" {
				sf.omitempty = true
			}
		}
	}
	if sf.name == "" {
		if f.Anonymous {
			// Embedded struct without an explicit name: promoted. Embedded
			// non-struct types are rejected by encoding/json itself; we key
			// them by type name as a harmless fallback.
			rt := f.Type
			if rt.Kind() == reflect.Pointer {
				rt = rt.Elem()
			}
			if rt.Kind() == reflect.Struct {
				sf.embedded = true
				return sf, false
			}
		}
		sf.name = f.Name
	}
	sf.value = reflect.Value{} // filled by the caller
	return sf, false
}

// redactStruct walks a struct's marshal-visible fields. It returns a
// map[string]any copy only when at least one subtree hit is found —
// otherwise the original struct flows to json.Marshal unchanged, keeping
// the audit record shape byte-identical for secret-free payloads.
func redactStruct(v reflect.Value, depth int, visited map[uintptr]struct{}) (any, bool) {
	t := v.Type()
	type entry struct {
		sf       structField
		red      any
		changed  bool
		promoted map[string]any // merged keys when an embedded struct changed
	}
	entries := make([]entry, 0, t.NumField())
	anyChanged := false

	for i := 0; i < t.NumField(); i++ {
		ft := t.Field(i)
		sf, skip := jsonFieldInfo(ft)
		if skip {
			// json.Marshal promotes the exported fields of unexported
			// EMBEDDED struct types even though reflect cannot read them.
			// Their values are unreadable here, but their NAMES are visible
			// via type metadata — if the promoted subtree carries a
			// sensitive name, force the parent rebuild and drop the subtree
			// rather than let it marshal a secret.
			if ft.Anonymous && !ft.IsExported() && typeHasSensitiveNames(ft.Type, 0) {
				anyChanged = true // no entry: subtree omitted
			}
			continue
		}
		fv := v.Field(i)
		if !fv.CanInterface() {
			continue
		}
		sf.value = fv // readable from here on; entries reuse it on rebuild
		if sf.embedded {
			// nil embedded pointer contributes nothing to JSON.
			if fv.Kind() == reflect.Pointer && fv.IsNil() {
				continue
			}
			if sf.omitempty && isEmptyJSONValue(fv) {
				continue
			}
			red, c := redactReflect(fv, depth+1, visited)
			if c {
				if m, ok := red.(map[string]any); ok {
					entries = append(entries, entry{sf: sf, changed: true, promoted: m})
					anyChanged = true
					continue
				}
				// A changed non-map embedded value (e.g. promoted map type):
				// key it by type name; encoding/json would reject this layout
				// anyway.
				entries = append(entries, entry{sf: structField{name: ft.Name}, red: red, changed: true})
				anyChanged = true
				continue
			}
			entries = append(entries, entry{sf: sf}) // unchanged: flatten on demand
			continue
		}
		if sf.omitempty && isEmptyJSONValue(fv) {
			continue // json.Marshal would omit it; there is nothing to leak
		}
		if isSensitive(sf.name) {
			entries = append(entries, entry{sf: sf, red: redactedValue, changed: true})
			anyChanged = true
			continue
		}
		red, c := redactReflect(fv, depth+1, visited)
		if c {
			entries = append(entries, entry{sf: sf, red: red, changed: true})
			anyChanged = true
		} else {
			entries = append(entries, entry{sf: sf})
		}
	}

	if !anyChanged {
		return nil, false
	}

	out := make(map[string]any, t.NumField())
	for _, e := range entries {
		switch {
		case e.sf.embedded && e.promoted != nil:
			for k, val := range e.promoted {
				out[k] = val
			}
		case e.sf.embedded:
			flattenPromoted(e.sf.value, out)
		default:
			if e.changed {
				out[e.sf.name] = e.red
			} else if e.sf.value.CanInterface() {
				out[e.sf.name] = e.sf.value.Interface()
			}
		}
	}
	return out, true
}

// flattenPromoted seeds the JSON-visible keys of an UNCHANGED embedded struct
// into out. Only called once the enclosing struct is already being rebuilt;
// values are taken verbatim because the walker proved no subtree hit exists.
func flattenPromoted(v reflect.Value, out map[string]any) {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct || isKnownLeaf(v.Type()) {
		return
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		ft := t.Field(i)
		sf, skip := jsonFieldInfo(ft)
		if skip {
			continue
		}
		fv := v.Field(i)
		if !fv.CanInterface() {
			continue
		}
		if sf.omitempty && isEmptyJSONValue(fv) {
			continue
		}
		if sf.embedded {
			if fv.Kind() == reflect.Pointer && fv.IsNil() {
				continue
			}
			flattenPromoted(fv, out)
			continue
		}
		out[sf.name] = fv.Interface()
	}
}

// typeHasSensitiveNames reports whether the JSON-marshalled shape of t could
// carry a sensitive key name. It walks TYPE metadata only — safe for
// subtrees whose values are not readable through reflect. Leaf rules mirror
// the value walker (known leaves and byte arrays are opaque).
func typeHasSensitiveNames(t reflect.Type, depth int) bool {
	if depth > maxRedactDepth {
		return false
	}
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Interface {
		t = t.Elem()
	}
	if isKnownLeaf(t) {
		return false
	}
	switch t.Kind() {
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			ft := t.Field(i)
			sf, skip := jsonFieldInfo(ft)
			if !skip {
				if !sf.embedded && isSensitive(sf.name) {
					return true
				}
				if typeHasSensitiveNames(ft.Type, depth+1) {
					return true
				}
				continue
			}
			// Skipped field: only an unexported embedded struct is still
			// marshalled (via promotion); everything else is invisible to
			// json.Marshal.
			if ft.Anonymous && !ft.IsExported() && typeHasSensitiveNames(ft.Type, depth+1) {
				return true
			}
		}
		return false
	case reflect.Map:
		return typeHasSensitiveNames(t.Elem(), depth+1)
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return false
		}
		return typeHasSensitiveNames(t.Elem(), depth+1)
	default:
		// Primitives, arrays and opaque kinds match the value walker's leaf
		// behaviour.
		return false
	}
}

// isEmptyJSONValue mirrors encoding/json's omitempty notion of emptiness.
func isEmptyJSONValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if !isEmptyJSONValue(v.Index(i)) {
				return false
			}
		}
		return true
	case reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Interface, reflect.Pointer:
		return v.IsNil()
	default:
		return false
	}
}
