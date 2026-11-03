package conv

import "go/types"

// step is one field access on the way from a command struct to a flag
// destination.
type step struct {
	f   *types.Var
	ptr bool // the field is a pointer to follow
}

// pathFrom returns the shortest chain of fields leading from one object to
// another, going through nested structs and pointers.
func pathFrom(from, to *object) ([]step, bool) {
	type node struct {
		o    *object
		path []step
	}
	seen := map[*object]bool{from: true}
	queue := []node{{from, nil}}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if n.o == to {
			return n.path, true
		}
		if len(n.path) > 8 || n.o.st == nil {
			continue
		}
		for i := 0; i < n.o.st.NumFields(); i++ {
			f := n.o.st.Field(i)
			var next *object
			ptr := false
			switch v := n.o.fields[f].(type) {
			case vObj:
				next = v.o
			case vPtr:
				next, ptr = v.o, true
			}
			if next == nil || seen[next] {
				continue
			}
			seen[next] = true
			p := append(append([]step{}, n.path...), step{f, ptr})
			queue = append(queue, node{next, p})
		}
	}
	return nil, false
}

// fieldHolding finds the field reachable from root that holds cell, and the
// object it belongs to.
func fieldHolding(root *object, cell vCell) (*object, *types.Var, bool) {
	seen := map[*object]bool{}
	var walk func(o *object, depth int) (*object, *types.Var, bool)
	walk = func(o *object, depth int) (*object, *types.Var, bool) {
		if o == nil || o.st == nil || seen[o] || depth > 8 {
			return nil, nil, false
		}
		seen[o] = true
		for i := 0; i < o.st.NumFields(); i++ {
			f := o.st.Field(i)
			switch v := o.fields[f].(type) {
			case vCell:
				if v == cell {
					return o, f, true
				}
			case vObj:
				if h, hf, ok := walk(v.o, depth+1); ok {
					return h, hf, true
				}
			case vPtr:
				if h, hf, ok := walk(v.o, depth+1); ok {
					return h, hf, true
				}
			}
		}
		return nil, nil, false
	}
	return walk(root, 0)
}
