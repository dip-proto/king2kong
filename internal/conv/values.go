package conv

import (
	"go/constant"
	"go/types"
)

// value is what the interpreter knows about an expression.
// It only tracks what matters to rebuild the kingpin grammar: constants,
// structs and their fields, kingpin builder handles, and callbacks.
// Anything else is unknown, which is fine as long as it never reaches a
// kingpin builder call.
type value interface{ isValue() }

type vUnknown struct{}

type vNil struct{}

// vConst is a constant, or a value the interpreter folded into one, such as a
// package variable that is never written or fmt.Sprintf over constants.
type vConst struct {
	c constant.Value
	t types.Type
}

// vObj is a struct value.
// Copying a struct of plain data gives a new object, as Go does.
// A struct holding grammar, such as a command, stays shared when copied,
// which keeps the field paths back to the command struct.
type vObj struct{ o *object }

// vPtr points at a struct object.
type vPtr struct{ o *object }

// structObj returns the object behind a struct value or struct pointer, or nil.
func structObj(v value) *object {
	switch v := v.(type) {
	case vObj:
		return v.o
	case vPtr:
		return v.o
	}
	return nil
}

// vFieldPtr points at a non-struct field of an object, the usual destination
// of a kingpin *Var binder.
type vFieldPtr struct {
	o *object
	f *types.Var
}

// vVarPtr points at a local or package variable.
type vVarPtr struct{ v *types.Var }

// vCell is a pointer to a fresh non-struct value, as made by new(string).
type vCell struct{ id int }

// vStorage is the pointer kingpin methods such as String() return, to storage
// kingpin allocates for the clause.
type vStorage struct{ c *clause }

// vClause is a kingpin builder handle: the application, a command, a flag or a
// positional argument.
type vClause struct{ c *clause }

// vFunc is a function or a method value bound to a receiver.
type vFunc struct {
	fn   *types.Func
	recv value
}

// vSlice is a slice or array literal, or the variadic tail of a call.
type vSlice struct{ elems []value }

func (vUnknown) isValue()  {}
func (vNil) isValue()      {}
func (vConst) isValue()    {}
func (vObj) isValue()      {}
func (vPtr) isValue()      {}
func (vFieldPtr) isValue() {}
func (vVarPtr) isValue()   {}
func (vClause) isValue()   {}
func (vCell) isValue()     {}
func (vStorage) isValue()  {}
func (vFunc) isValue()     {}
func (vSlice) isValue()    {}

// object is a struct value tracked by the interpreter.
// A known object comes from a composite literal or a zero declaration, so a
// field never written still holds its zero value.
// An unknown object came from code the interpreter did not see.
type object struct {
	id     int
	typ    types.Type
	st     *types.Struct
	known  bool
	fields map[*types.Var]value

	// parent and pfield link a struct embedded or nested by value to the
	// object that contains it.
	parent *object
	pfield *types.Var

	// site describes where the object was created, for diagnostics.
	site string
}

func (o *object) root() *object {
	for o.parent != nil {
		o = o.parent
	}
	return o
}

// path returns the fields leading from the outermost object down to o.
func (o *object) path() []*types.Var {
	var out []*types.Var
	for cur := o; cur.parent != nil; cur = cur.parent {
		out = append([]*types.Var{cur.pfield}, out...)
	}
	return out
}

func zeroValue(t types.Type) value {
	switch u := t.Underlying().(type) {
	case *types.Basic:
		switch {
		case u.Info()&types.IsBoolean != 0:
			return vConst{constant.MakeBool(false), t}
		case u.Info()&types.IsString != 0:
			return vConst{constant.MakeString(""), t}
		case u.Info()&types.IsInteger != 0:
			return vConst{constant.MakeInt64(0), t}
		case u.Info()&types.IsFloat != 0:
			return vConst{constant.MakeFloat64(0), t}
		}
	case *types.Pointer, *types.Interface, *types.Signature, *types.Slice, *types.Map, *types.Chan:
		return vNil{}
	}
	return vUnknown{}
}

func isNonNil(v value) (nonNil, known bool) {
	switch v := v.(type) {
	case vNil:
		return false, true
	case vPtr, vFieldPtr, vVarPtr, vClause, vObj, vCell, vStorage:
		return true, true
	case vFunc:
		return v.fn != nil, true
	case vSlice:
		return v.elems != nil, true
	}
	return false, false
}
