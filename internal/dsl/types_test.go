package dsl

// types_test.go — unit tests for the LEVEELang type system.
//
// Coverage:
//   - basic type String() / IsBasic() / Equals() / Compatible()
//   - TypeMap and TypeList structural equality and compatibility
//   - TypeAlias resolution and compatibility
//   - TypeEnum membership and compatibility
//   - TypeRegistry registration and resolution

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Basic types: String / IsBasic -----------------------------------------

func TestBasicTypeString(t *testing.T) {
	assert.Equal(t, "string", TypeString{}.String())
	assert.Equal(t, "int", TypeInt{}.String())
	assert.Equal(t, "float", TypeFloat{}.String())
	assert.Equal(t, "bool", TypeBool{}.String())
	assert.Equal(t, "duration", TypeDuration{}.String())
	assert.Equal(t, "percent", TypePercent{}.String())
}

func TestBasicTypeIsBasic(t *testing.T) {
	assert.True(t, TypeString{}.IsBasic())
	assert.True(t, TypeInt{}.IsBasic())
	assert.True(t, TypeFloat{}.IsBasic())
	assert.True(t, TypeBool{}.IsBasic())
	assert.True(t, TypeDuration{}.IsBasic())
	assert.True(t, TypePercent{}.IsBasic())
	assert.True(t, TypeMap{}.IsBasic())
	assert.True(t, TypeList{}.IsBasic())
}

// --- Basic types: Equals ---------------------------------------------------

func TestBasicTypeEquals(t *testing.T) {
	assert.True(t, TypeString{}.Equals(TypeString{}))
	assert.False(t, TypeString{}.Equals(TypeInt{}))
	assert.False(t, TypeString{}.Equals(nil))
	assert.True(t, TypeInt{}.Equals(TypeInt{}))
	assert.False(t, TypeInt{}.Equals(TypeFloat{}))
}

// --- Compatibility matrix --------------------------------------------------

func TestIntCompatibleWithFloat(t *testing.T) {
	// int is compatible with float (widening).
	assert.True(t, TypeFloat{}.Compatible(TypeInt{}))
	assert.True(t, TypeInt{}.Compatible(TypeFloat{}))
}

func TestStringCompatibleWithAll(t *testing.T) {
	// string accepts any type under weak compatibility.
	for _, other := range []Type{
		TypeString{}, TypeInt{}, TypeFloat{}, TypeBool{},
		TypeDuration{}, TypePercent{},
	} {
		assert.True(t, TypeString{}.Compatible(other),
			"string should be compatible with %s", other.String())
	}
}

func TestBoolNotCompatibleWithInt(t *testing.T) {
	assert.False(t, TypeBool{}.Compatible(TypeInt{}))
	assert.False(t, TypeInt{}.Compatible(TypeBool{}))
}

func TestDurationCompatibleWithString(t *testing.T) {
	assert.True(t, TypeDuration{}.Compatible(TypeString{}))
	assert.False(t, TypeDuration{}.Compatible(TypeInt{}))
}

func TestPercentCompatibleWithInt(t *testing.T) {
	assert.True(t, TypePercent{}.Compatible(TypeInt{}))
	assert.True(t, TypePercent{}.Compatible(TypeString{}))
	assert.False(t, TypePercent{}.Compatible(TypeBool{}))
}

func TestCompatibleNil(t *testing.T) {
	assert.False(t, TypeString{}.Compatible(nil))
	assert.False(t, TypeInt{}.Equals(nil))
}

// --- TypeMap ---------------------------------------------------------------

func TestTypeMapString(t *testing.T) {
	m := TypeMap{KeyType: TypeString{}, ValueType: TypeInt{}}
	assert.Equal(t, "map<string,int>", m.String())
}

func TestTypeMapEquals(t *testing.T) {
	m1 := TypeMap{KeyType: TypeString{}, ValueType: TypeInt{}}
	m2 := TypeMap{KeyType: TypeString{}, ValueType: TypeInt{}}
	m3 := TypeMap{KeyType: TypeString{}, ValueType: TypeFloat{}}
	assert.True(t, m1.Equals(m2))
	assert.False(t, m1.Equals(m3))
	assert.False(t, m1.Equals(TypeInt{}))
}

func TestTypeMapCompatible(t *testing.T) {
	// map<string,int> compatible with map<string,float> (int->float widening).
	m1 := TypeMap{KeyType: TypeString{}, ValueType: TypeFloat{}}
	m2 := TypeMap{KeyType: TypeString{}, ValueType: TypeInt{}}
	assert.True(t, m1.Compatible(m2))
	// map<bool,int> not compatible with map<int,int> (bool key not compatible with int).
	m3 := TypeMap{KeyType: TypeBool{}, ValueType: TypeInt{}}
	m4 := TypeMap{KeyType: TypeInt{}, ValueType: TypeInt{}}
	assert.False(t, m3.Compatible(m4))
}

// --- TypeList --------------------------------------------------------------

func TestTypeListString(t *testing.T) {
	l := TypeList{ElementType: TypeInt{}}
	assert.Equal(t, "list<int>", l.String())
}

func TestTypeListEquals(t *testing.T) {
	l1 := TypeList{ElementType: TypeInt{}}
	l2 := TypeList{ElementType: TypeInt{}}
	l3 := TypeList{ElementType: TypeFloat{}}
	assert.True(t, l1.Equals(l2))
	assert.False(t, l1.Equals(l3))
	assert.False(t, l1.Equals(TypeInt{}))
}

func TestTypeListCompatible(t *testing.T) {
	// list<float> compatible with list<int>.
	l1 := TypeList{ElementType: TypeFloat{}}
	l2 := TypeList{ElementType: TypeInt{}}
	assert.True(t, l1.Compatible(l2))
	// list<int> not compatible with list<bool>.
	l3 := TypeList{ElementType: TypeBool{}}
	assert.False(t, l2.Compatible(l3))
}

// --- TypeAlias -------------------------------------------------------------

func TestTypeAliasString(t *testing.T) {
	a := &TypeAlias{Name: "port", Target: TypeInt{}}
	assert.Equal(t, "port", a.String())
	assert.False(t, a.IsBasic())
}

func TestTypeAliasEquals(t *testing.T) {
	a1 := &TypeAlias{Name: "port", Target: TypeInt{}}
	a2 := &TypeAlias{Name: "port", Target: TypeInt{}}
	a3 := &TypeAlias{Name: "count", Target: TypeInt{}}
	assert.True(t, a1.Equals(a2))
	assert.False(t, a1.Equals(a3))
}

func TestTypeAliasCompatibleDelegatesToTarget(t *testing.T) {
	a := &TypeAlias{Name: "port", Target: TypeInt{}}
	// port (alias of int) should be compatible with float.
	assert.True(t, a.Compatible(TypeFloat{}))
	// port should not be compatible with bool.
	assert.False(t, a.Compatible(TypeBool{}))
}

func TestTypeAliasResolveBasic(t *testing.T) {
	// resolveBasic unwraps aliases for basic-type comparison.
	a := &TypeAlias{Name: "port", Target: TypeInt{}}
	assert.IsType(t, TypeInt{}, resolveBasic(a))
}

// --- TypeEnum --------------------------------------------------------------

func TestTypeEnumString(t *testing.T) {
	e := &TypeEnum{Name: "status", Values: []string{"ok", "warn", "crit"}}
	assert.Equal(t, "enum status", e.String())
	assert.False(t, e.IsBasic())
}

func TestTypeEnumEquals(t *testing.T) {
	e1 := &TypeEnum{Name: "status", Values: []string{"ok"}}
	e2 := &TypeEnum{Name: "status", Values: []string{"ok", "warn"}}
	e3 := &TypeEnum{Name: "level", Values: []string{"ok"}}
	assert.True(t, e1.Equals(e2)) // equality by name
	assert.False(t, e1.Equals(e3))
}

func TestTypeEnumHasValue(t *testing.T) {
	e := &TypeEnum{Name: "status", Values: []string{"ok", "warn", "crit"}}
	assert.True(t, e.HasValue("ok"))
	assert.True(t, e.HasValue("crit"))
	assert.False(t, e.HasValue("unknown"))
}

func TestTypeEnumCompatible(t *testing.T) {
	e := &TypeEnum{Name: "status", Values: []string{"ok"}}
	// same enum is compatible.
	assert.True(t, e.Compatible(e))
	// string is compatible (weak mode).
	assert.True(t, e.Compatible(TypeString{}))
	// different type is not.
	assert.False(t, e.Compatible(TypeInt{}))
}

// --- TypeRegistry ----------------------------------------------------------

func TestNewTypeRegistryEmpty(t *testing.T) {
	r := NewTypeRegistry()
	assert.NotNil(t, r)
	assert.Empty(t, r.Aliases())
	assert.Empty(t, r.Enums())
}

func TestRegistryRegisterAlias(t *testing.T) {
	r := NewTypeRegistry()
	require.NoError(t, r.RegisterAlias("port", TypeInt{}))
	a := r.Alias("port")
	require.NotNil(t, a)
	assert.Equal(t, "port", a.Name)
	assert.IsType(t, TypeInt{}, a.Target)
}

func TestRegistryRegisterAliasErrors(t *testing.T) {
	r := NewTypeRegistry()
	assert.Error(t, r.RegisterAlias("", TypeInt{}))
	assert.Error(t, r.RegisterAlias("port", nil))
}

func TestRegistryRegisterEnum(t *testing.T) {
	r := NewTypeRegistry()
	require.NoError(t, r.RegisterEnum("status", []string{"ok", "warn", "crit"}))
	e := r.Enum("status")
	require.NotNil(t, e)
	assert.Equal(t, "status", e.Name)
	assert.Len(t, e.Values, 3)
}

func TestRegistryRegisterEnumErrors(t *testing.T) {
	r := NewTypeRegistry()
	assert.Error(t, r.RegisterEnum("", []string{"ok"}))
	assert.Error(t, r.RegisterEnum("status", nil))
}

func TestRegistryResolveBasic(t *testing.T) {
	r := NewTypeRegistry()
	for _, name := range []string{"string", "int", "float", "bool", "duration", "percent"} {
		tt := r.Resolve(name)
		require.NotNil(t, tt, "should resolve %q", name)
		assert.True(t, tt.IsBasic(), "%q should be basic", name)
	}
}

func TestRegistryResolveAlias(t *testing.T) {
	r := NewTypeRegistry()
	require.NoError(t, r.RegisterAlias("port", TypeInt{}))
	tt := r.Resolve("port")
	require.NotNil(t, tt)
	assert.IsType(t, &TypeAlias{}, tt)
	assert.Equal(t, "port", tt.String())
}

func TestRegistryResolveEnum(t *testing.T) {
	r := NewTypeRegistry()
	require.NoError(t, r.RegisterEnum("status", []string{"ok"}))
	tt := r.Resolve("status")
	require.NotNil(t, tt)
	assert.IsType(t, &TypeEnum{}, tt)
}

func TestRegistryResolveUnknown(t *testing.T) {
	r := NewTypeRegistry()
	assert.Nil(t, r.Resolve("nonexistent"))
}

func TestRegistryAliasesSnapshot(t *testing.T) {
	r := NewTypeRegistry()
	require.NoError(t, r.RegisterAlias("port", TypeInt{}))
	snap := r.Aliases()
	require.Contains(t, snap, "port")
	// Mutating the snapshot does not affect the registry.
	delete(snap, "port")
	assert.NotNil(t, r.Alias("port"))
}