// Template instantiation for LEVEE.
//
// Instantiation takes a Template (see library.go) and a caller-supplied set
// of key=value parameters, fills the template's {{.placeholder}} placeholders
// with the resolved values, and returns the completed YAML workflow content.
// Along the way it validates that every required parameter is supplied (or
// has a default), that supplied values match their declared types, and that
// no unknown parameters are passed.
//
// The Instantiator is stateless: its zero value is ready to use and a single
// instance may be shared across goroutines.
//
// This file is part of package template and relies on the Template and
// TemplateParam types defined in library.go. The sentinel error
// ErrEmptyContent is also defined in library.go and is reused here.
package template

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// --- Sentinel errors --------------------------------------------------------

var (
	// ErrRequiredParamMissing is returned when a required template parameter
	// is neither supplied by the caller nor has a default value.
	ErrRequiredParamMissing = errors.New("template: required parameter missing")

	// ErrParamTypeMismatch is returned when a supplied parameter value does
	// not match the parameter's declared type.
	ErrParamTypeMismatch = errors.New("template: parameter type mismatch")

	// ErrUnknownParam is returned when the caller supplies a parameter that
	// is not declared by the template.
	ErrUnknownParam = errors.New("template: unknown parameter")
)

// Note: ErrEmptyContent is defined in library.go and reused here to avoid
// a duplicate declaration.

// --- Instantiator -----------------------------------------------------------

// Instantiator fills template placeholders with caller-supplied parameter
// values and validates parameter completeness and types.
//
// An Instantiator is stateless; the zero value is ready to use. A single
// instance may be safely shared across goroutines.
type Instantiator struct{}

// InstantiateResult holds the result of instantiating a template.
type InstantiateResult struct {
	// TemplateName is the name of the source template.
	TemplateName string

	// Content is the fully-substituted YAML workflow content. On success
	// this is the template's Content with every resolvable placeholder
	// replaced by its value.
	Content string

	// Params is the map of parameter names to the values actually used,
	// including defaults applied for omitted parameters. Optional
	// parameters that were omitted and had no default are absent from
	// this map (their placeholders are left in Content unchanged).
	Params map[string]string

	// Missing lists the required parameters that were missing and could
	// not be filled from defaults. On a successful Instantiate call this
	// is nil. When Instantiate returns ErrRequiredParamMissing, the
	// returned result (if non-nil) carries the missing names here.
	Missing []string
}

// NewInstantiator creates a new Instantiator.
func NewInstantiator() *Instantiator {
	return &Instantiator{}
}

// Instantiate fills the template's placeholders with the given params,
// validates required parameters, applies defaults, and returns the
// completed workflow content.
//
// Resolution rules for each declared parameter:
//   - If params provides the parameter, the supplied value is used.
//   - Otherwise, if the parameter has a non-empty Default, the default
//     is used.
//   - Otherwise, if the parameter is Required, the call fails with
//     ErrRequiredParamMissing (the returned result, if non-nil, carries
//     the missing names in Missing).
//   - Otherwise (optional, no default), the placeholder is left in the
//     content unchanged and the parameter is absent from Params.
//
// Type validation is performed on each resolved value:
//   - "string" (or empty/unknown type): any value accepted.
//   - "int": value must be parseable by strconv.Atoi.
//   - "bool": value must be one of "true", "false", "yes", "no", "1", "0".
//   - "list": any value accepted (interpreted as a comma-separated list).
//
// Placeholders in tmpl.Content are replaced using strings.ReplaceAll on
// the literal "{{.name}}" pattern. Go's text/template package is not used
// to avoid additional dependencies and template-injection risks.
//
// Returns:
//   - ErrEmptyContent (from library.go) when tmpl.Content is empty.
//   - ErrUnknownParam when params contains a key not declared by tmpl.
//   - ErrParamTypeMismatch when a resolved value fails type validation.
//   - ErrRequiredParamMissing when a required parameter has no value and
//     no default.
func (inst *Instantiator) Instantiate(tmpl *Template, params map[string]string) (*InstantiateResult, error) {
	if tmpl == nil {
		return nil, fmt.Errorf("template: instantiate: nil template")
	}
	if tmpl.Content == "" {
		return nil, ErrEmptyContent
	}

	// Detect unknown parameters: keys in params that the template does not
	// declare. This is a strict check that catches caller typos early.
	declared := make(map[string]bool, len(tmpl.Parameters))
	for _, p := range tmpl.Parameters {
		declared[p.Name] = true
	}
	for k := range params {
		if !declared[k] {
			return nil, fmt.Errorf("%w: %s", ErrUnknownParam, k)
		}
	}

	// Resolve and validate each declared parameter in declaration order.
	resolved := make(map[string]string, len(tmpl.Parameters))
	var missing []string
	for _, p := range tmpl.Parameters {
		val, ok := params[p.Name]
		if !ok {
			if p.Default != "" {
				val = p.Default
			} else if p.Required {
				missing = append(missing, p.Name)
				continue
			} else {
				// Optional, no default: leave placeholder unchanged.
				continue
			}
		}
		if err := validateParamType(p, val); err != nil {
			return nil, fmt.Errorf("%w: param %s value %q: %v",
				ErrParamTypeMismatch, p.Name, val, err)
		}
		resolved[p.Name] = val
	}

	if len(missing) > 0 {
		// Return a partial result carrying the missing names so callers
		// can inspect which required parameters were not supplied.
		return &InstantiateResult{
			TemplateName: tmpl.Name,
			Content:      tmpl.Content,
			Params:       resolved,
			Missing:      missing,
		}, fmt.Errorf("%w: %s", ErrRequiredParamMissing, strings.Join(missing, ", "))
	}

	// Substitute placeholders. We iterate over the resolved map and replace
	// each "{{.name}}" literal with its value. Unresolved optional
	// placeholders remain in the content verbatim.
	out := tmpl.Content
	for name, val := range resolved {
		out = strings.ReplaceAll(out, "{{."+name+"}}", val)
	}

	return &InstantiateResult{
		TemplateName: tmpl.Name,
		Content:      out,
		Params:       resolved,
		Missing:      nil,
	}, nil
}

// validateParamType checks that val matches the declared type of p.
// Empty or unknown types are treated as "string" and accept any value.
// The returned error describes the type violation but is not a sentinel;
// callers wrap it with ErrParamTypeMismatch.
func validateParamType(p TemplateParam, val string) error {
	switch p.Type {
	case "", "string":
		// Any value is acceptable.
		return nil
	case "int":
		if _, err := strconv.Atoi(val); err != nil {
			return fmt.Errorf("not an int: %q", val)
		}
		return nil
	case "bool":
		switch val {
		case "true", "false", "yes", "no", "1", "0":
			return nil
		}
		return fmt.Errorf("not a bool: %q", val)
	case "list":
		// Any value is acceptable; interpreted as comma-separated.
		return nil
	default:
		// Unknown type: treat as string.
		return nil
	}
}

// ParseParams parses a CLI-style "key=val,key2=val2" string into a map.
//
// Rules:
//   - An empty input yields an empty (non-nil) map.
//   - Each pair must contain a '='; otherwise an error is returned.
//   - Keys must be non-empty after trimming; values may be empty.
//   - Key and value are trimmed of surrounding whitespace.
//   - A literal comma inside a value is escaped as "\,"; the backslash
//     is removed in the returned value. For example, "tags=a\,b,c=d"
//     yields {"tags": "a,b", "c": "d"}.
//   - Duplicate keys: the last occurrence wins.
//
// ParseParams is used by the CLI front-end to translate the
// --params flag value into the map expected by Instantiate.
func ParseParams(s string) (map[string]string, error) {
	out := make(map[string]string)
	if s == "" {
		return out, nil
	}

	pairs := splitParamPairs(s)
	for _, pair := range pairs {
		idx := strings.IndexByte(pair, '=')
		if idx < 0 {
			return nil, fmt.Errorf("template: parse params: missing '=' in %q", pair)
		}
		key := strings.TrimSpace(pair[:idx])
		val := strings.TrimSpace(pair[idx+1:])
		if key == "" {
			return nil, fmt.Errorf("template: parse params: empty key in %q", pair)
		}
		out[key] = val
	}
	return out, nil
}

// splitParamPairs splits the input on unescaped commas. A "\," sequence is
// treated as a literal comma belonging to the current pair; the backslash
// is removed from the returned pair text. A trailing backslash with no
// following comma is preserved as a literal backslash.
func splitParamPairs(s string) []string {
	var pairs []string
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) && s[i+1] == ',' {
			// Escaped comma: emit a literal comma into the current pair.
			b.WriteByte(',')
			i++
			continue
		}
		if c == ',' {
			pairs = append(pairs, b.String())
			b.Reset()
			continue
		}
		b.WriteByte(c)
	}
	pairs = append(pairs, b.String())
	return pairs
}
