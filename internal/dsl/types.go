
// Package dsl parses the LEVEELang YAML subset (MVP stage) into an abstract
// syntax tree (AST). The AST is consumed by downstream phases (plan, apply,
// rollback) and carries enough structure to support basic compile-time
// validation such as required-field checks, enum validation and batch legality.
//
// types.go declares the LEVEELang compile-time type system. It introduces eight
// basic types (string, int, float, bool, duration, percent, map, list), type
// aliases ("type port = int") and enum types ("enum status { ok, warn, crit }").
// A TypeRegistry tracks user-defined aliases and enums; the type checker uses it
// to resolve symbolic type names appearing in the AST.
package dsl

// Type is the interface implemented by every LEVEELang type. The four methods
// cover the operations required by the compile-time type checker:
//   - String returns the canonical textual representation (e.g. "int",
//     "map<string,int>", "enum status").
//   - IsBasic reports whether the type is one of the eight basic types.
//   - Equals reports structural equality.
//   - Compatible reports whether a value of `other` may be assigned to a slot
//     of this type under LEVEELang's weak-compatibility rules.
type Type interface {
	String() string
	IsBasic() bool
	Equals(other Type) bool
	Compatible(other Type) bool
}

// ---------------------------------------------------------------------------
// Eight basic types
// ---------------------------------------------------------------------------

// TypeString is the basic string type.
type TypeString struct{}

// String returns the canonical name.
func (TypeString) String() string { return "string" }

// IsBasic returns true.
func (TypeString) IsBasic() bool { return true }

// Equals reports structural equality.
func (TypeString) Equals(other Type) bool {
	if other == nil {
		return false
	}
	_, ok := resolveBasic(other).(TypeString)
	return ok
}

// Compatible allows any type to be stringified under weak compatibility.
func (TypeString) Compatible(other Type) bool {
	if other == nil {
		return false
	}
	// string accepts anything (weak compatibility: stringify).
	return true
}

// TypeInt is the basic integer type.
type TypeInt struct{}

// String returns the canonical name.
func (TypeInt) String() string { return "int" }

// IsBasic returns true.
func (TypeInt) IsBasic() bool { return true }

// Equals reports structural equality.
func (TypeInt) Equals(other Type) bool {
	if other == nil {
		return false
	}
	_, ok := resolveBasic(other).(TypeInt)
	return ok
}

// Compatible accepts int, float (widening) and string (weak mode).
func (TypeInt) Compatible(other Type) bool {
	if other == nil {
		return false
	}
	switch resolveBasic(other).(type) {
	case TypeInt, TypeFloat, TypeString:
		return true
	}
	return false
}

// TypeFloat is the basic floating-point type.
type TypeFloat struct{}

// String returns the canonical name.
func (TypeFloat) String() string { return "float" }

// IsBasic returns true.
func (TypeFloat) IsBasic() bool { return true }

// Equals reports structural equality.
func (TypeFloat) Equals(other Type) bool {
	if other == nil {
		return false
	}
	_, ok := resolveBasic(other).(TypeFloat)
	return ok
}

// Compatible accepts float, int (widening) and string (weak mode).
func (TypeFloat) Compatible(other Type) bool {
	if other == nil {
		return false
	}
	switch resolveBasic(other).(type) {
	case TypeFloat, TypeInt, TypeString:
		return true
	}
	return false
}

// TypeBool is the basic boolean type.
type TypeBool struct{}

// String returns the canonical name.
func (TypeBool) String() string { return "bool" }

// IsBasic returns true.
func (TypeBool) IsBasic() bool { return true }

// Equals reports structural equality.
func (TypeBool) Equals(other Type) bool {
	if other == nil {
		return false
	}
	_, ok := resolveBasic(other).(TypeBool)
	return ok
}

// Compatible accepts bool and string (weak mode).
func (TypeBool) Compatible(other Type) bool {
	if other == nil {
		return false
	}
	switch resolveBasic(other).(type) {
	case TypeBool, TypeString:
		return true
	}
	return false
}

// TypeDuration is the basic duration type (e.g. "5m", "1h30m").
type TypeDuration struct{}

// String returns the canonical name.
func (TypeDuration) String() string { return "duration" }

// IsBasic returns true.
func (TypeDuration) IsBasic() bool { return true }

// Equals reports structural equality.
func (TypeDuration) Equals(other Type) bool {
	if other == nil {
		return false
	}
	_, ok := resolveBasic(other).(TypeDuration)
	return ok
}

// Compatible accepts duration and string (weak mode).
func (TypeDuration) Compatible(other Type) bool {
	if other == nil {
		return false
	}
	switch resolveBasic(other).(type) {
	case TypeDuration, TypeString:
		return true
	}
	return false
}

// TypePercent is the basic percent type (integer in [0, 100]).
type TypePercent struct{}

// String returns the canonical name.
func (TypePercent) String() string { return "percent" }

// IsBasic returns true.
func (TypePercent) IsBasic() bool { return true }

// Equals reports structural equality.
func (TypePercent) Equals(other Type) bool {
	if other == nil {
		return false
	}
	_, ok := resolveBasic(other).(TypePercent)
	return ok
}

// Compatible accepts percent, int and string (weak mode).
func (TypePercent) Compatible(other Type) bool {
	if other == nil {
		return false
	}
	switch resolveBasic(other).(type) {
	case TypePercent, TypeInt, TypeString:
		return true
	}
	return false
}

// TypeMap is the basic key-value map type. KeyType and ValueType are
// themselves Types; the canonical form is "map<key,value>".
type TypeMap struct {
	KeyType   Type
	ValueType Type
}

// String returns the canonical form "map<key,value>".
func (m TypeMap) String() string {
	if m.KeyType == nil || m.ValueType == nil {
		return "map<?,?>"
	}
	return "map<" + m.KeyType.String() + "," + m.ValueType.String() + ">"
}

// IsBasic returns true: map is one of the eight basic types.
func (TypeMap) IsBasic() bool { return true }

// Equals reports structural equality of key and value types.
func (m TypeMap) Equals(other Type) bool {
	if other == nil {
		return false
	}
	om, ok := other.(TypeMap)
	if !ok {
		return false
	}
	if m.KeyType == nil || m.ValueType == nil || om.KeyType == nil || om.ValueType == nil {
		return false
	}
	return m.KeyType.Equals(om.KeyType) && m.ValueType.Equals(om.ValueType)
}

// Compatible requires a TypeMap whose key/value types are pairwise compatible.
func (m TypeMap) Compatible(other Type) bool {
	if other == nil {
		return false
	}
	om, ok := other.(TypeMap)
	if !ok {
		return false
	}
	if m.KeyType == nil || m.ValueType == nil || om.KeyType == nil || om.ValueType == nil {
		return false
	}
	return m.KeyType.Compatible(om.KeyType) && m.ValueType.Compatible(om.ValueType)
}

// TypeList is the basic list type. ElementType is the element type; the
// canonical form is "list<elem>".
type TypeList struct {
	ElementType Type
}

// String returns the canonical form "list<elem>".
func (l TypeList) String() string {
	if l.ElementType == nil {
		return "list<?>"
	}
	return "list<" + l.ElementType.String() + ">"
}

// IsBasic returns true: list is one of the eight basic types.
func (TypeList) IsBasic() bool { return true }

// Equals reports structural equality of the element type.
func (l TypeList) Equals(other Type) bool {
	if other == nil {
		return false
	}
	ol, ok := other.(TypeList)
	if !ok {
		return false
	}
	if l.ElementType == nil || ol.ElementType == nil {
		return false
	}
	return l.ElementType.Equals(ol.ElementType)
}

// Compatible requires a TypeList whose element type is compatible.
func (l TypeList) Compatible(other Type) bool {
	if other == nil {
		return false
	}
	ol, ok := other.(TypeList)
	if !ok {
		return false
	}
	if l.ElementType == nil || ol.ElementType == nil {
		return false
	}
	return l.ElementType.Compatible(ol.ElementType)
}

// resolveBasic unwraps a TypeAlias so the basic-type switch above can compare
// on the underlying target type. Enums and unaliased types are returned as-is.
func resolveBasic(t Type) Type {
	for t != nil {
		a, ok := t.(*TypeAlias)
		if !ok || a == nil || a.Target == nil {
			return t
		}
		t = a.Target
	}
	return t
}

// ---------------------------------------------------------------------------
// TypeAlias: "type port = int"
// ---------------------------------------------------------------------------

// TypeAlias declares a named alias for another type. The alias is
// interchangeable with its target for compatibility purposes, but keeps its
// own name for diagnostics and IR serialisation.
type TypeAlias struct {
	Name   string
	Target Type
}

// String returns the alias name.
func (a *TypeAlias) String() string {
	if a == nil {
		return "<nil-alias>"
	}
	return a.Name
}

// IsBasic returns false: an alias is a user-defined type, not a basic one.
func (a *TypeAlias) IsBasic() bool { return false }

// Equals reports equality by alias name. Two aliases with different names are
// never equal; when both names are empty, equality falls back to the target
// type so that anonymous aliases compare structurally.
func (a *TypeAlias) Equals(other Type) bool {
	if a == nil || other == nil {
		return false
	}
	oa, ok := other.(*TypeAlias)
	if !ok {
		return false
	}
	if a.Name != oa.Name {
		return false
	}
	if a.Name != "" {
		return true
	}
	if a.Target == nil || oa.Target == nil {
		return false
	}
	return a.Target.Equals(oa.Target)
}

// Compatible delegates to the target type's compatibility rules.
func (a *TypeAlias) Compatible(other Type) bool {
	if a == nil || a.Target == nil || other == nil {
		return false
	}
	return a.Target.Compatible(other)
}

// ---------------------------------------------------------------------------
// TypeEnum: "enum status { ok, warn, crit }"
// ---------------------------------------------------------------------------

// TypeEnum declares a named enumeration with a fixed set of string values.
// An enum value is compatible with the same enum or with string (weak mode).
type TypeEnum struct {
	Name   string
	Values []string
}

// String returns the canonical form "enum <name>".
func (e *TypeEnum) String() string {
	if e == nil {
		return "<nil-enum>"
	}
	return "enum " + e.Name
}

// IsBasic returns false: an enum is a user-defined type.
func (e *TypeEnum) IsBasic() bool { return false }

// Equals reports equality by enum name.
func (e *TypeEnum) Equals(other Type) bool {
	if e == nil || other == nil {
		return false
	}
	oe, ok := other.(*TypeEnum)
	if !ok {
		return false
	}
	return e.Name == oe.Name
}

// Compatible accepts the same enum or a string (weak mode).
func (e *TypeEnum) Compatible(other Type) bool {
	if e == nil || other == nil {
		return false
	}
	switch o := other.(type) {
	case *TypeEnum:
		return e.Name == o.Name
	case TypeString:
		return true
	}
	return false
}

// HasValue reports whether v is one of the declared enum values.
func (e *TypeEnum) HasValue(v string) bool {
	if e == nil {
		return false
	}
	for _, ev := range e.Values {
		if ev == v {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// TypeRegistry
// ---------------------------------------------------------------------------

// TypeRegistry stores user-defined type aliases and enums. It is the symbol
// table used by the type checker to resolve symbolic type names appearing in
// the AST (e.g. InputParam.Type == "port"). The zero value is not ready —
// use NewTypeRegistry.
type TypeRegistry struct {
	aliases map[string]*TypeAlias
	enums   map[string]*TypeEnum
}

// NewTypeRegistry returns an empty registry preloaded with the eight basic
// types as self-resolving entries. Basic types are not stored as aliases;
// Resolve returns the canonical Type instance for them.
func NewTypeRegistry() *TypeRegistry {
	return &TypeRegistry{
		aliases: make(map[string]*TypeAlias),
		enums:   make(map[string]*TypeEnum),
	}
}

// RegisterAlias registers a type alias. It overwrites any previous alias with
// the same name. Returns an error when name is empty or target is nil.
func (r *TypeRegistry) RegisterAlias(name string, target Type) error {
	if r == nil {
		return errRegistryNil
	}
	if name == "" {
		return errAliasNameEmpty
	}
	if target == nil {
		return errAliasTargetNil
	}
	r.aliases[name] = &TypeAlias{Name: name, Target: target}
	return nil
}

// RegisterEnum registers an enum type. It overwrites any previous enum with
// the same name. Returns an error when name is empty or no values are given.
func (r *TypeRegistry) RegisterEnum(name string, values []string) error {
	if r == nil {
		return errRegistryNil
	}
	if name == "" {
		return errEnumNameEmpty
	}
	if len(values) == 0 {
		return errEnumValuesEmpty
	}
	r.enums[name] = &TypeEnum{Name: name, Values: append([]string(nil), values...)}
	return nil
}

// Alias returns the alias with the given name, or nil when not registered.
func (r *TypeRegistry) Alias(name string) *TypeAlias {
	if r == nil {
		return nil
	}
	return r.aliases[name]
}

// Enum returns the enum with the given name, or nil when not registered.
func (r *TypeRegistry) Enum(name string) *TypeEnum {
	if r == nil {
		return nil
	}
	return r.enums[name]
}

// Aliases returns a snapshot of all registered aliases. The order is
// non-deterministic; callers that need a stable order should sort the keys.
func (r *TypeRegistry) Aliases() map[string]*TypeAlias {
	if r == nil {
		return nil
	}
	out := make(map[string]*TypeAlias, len(r.aliases))
	for k, v := range r.aliases {
		out[k] = v
	}
	return out
}

// Enums returns a snapshot of all registered enums.
func (r *TypeRegistry) Enums() map[string]*TypeEnum {
	if r == nil {
		return nil
	}
	out := make(map[string]*TypeEnum, len(r.enums))
	for k, v := range r.enums {
		out[k] = v
	}
	return out
}

// Resolve resolves a symbolic type name to a Type instance. Basic type names
// return the canonical basic type; registered aliases and enums are returned
// from the registry; unknown names return nil.
func (r *TypeRegistry) Resolve(name string) Type {
	if t := basicTypeByName(name); t != nil {
		return t
	}
	if r == nil {
		return nil
	}
	if a, ok := r.aliases[name]; ok {
		return a
	}
	if e, ok := r.enums[name]; ok {
		return e
	}
	return nil
}

// basicTypeByName maps a basic type name to its canonical Type instance.
// Returns nil for unknown names.
func basicTypeByName(name string) Type {
	switch name {
	case "string":
		return TypeString{}
	case "int":
		return TypeInt{}
	case "float":
		return TypeFloat{}
	case "bool":
		return TypeBool{}
	case "duration":
		return TypeDuration{}
	case "percent":
		return TypePercent{}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// Sentinel errors for the registry. They are defined as package-level vars so
// callers can compare with errors.Is.
var (
	errRegistryNil     = newTypeError("", 0, 0, "type registry is nil")
	errAliasNameEmpty  = newTypeError("", 0, 0, "alias name is empty")
	errAliasTargetNil  = newTypeError("", 0, 0, "alias target is nil")
	errEnumNameEmpty   = newTypeError("", 0, 0, "enum name is empty")
	errEnumValuesEmpty = newTypeError("", 0, 0, "enum values are empty")
)