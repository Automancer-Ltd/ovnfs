package overlay

// opIndex finds the host operations in flight or queued by the names they
// touch, so a call's check for one on its names, their ancestors and their
// descendants costs a walk up its path, not a scan of every operation.
type opIndex struct {
	at    map[string][]*hostOp            // operations on each name
	below map[string]map[*hostOp]struct{} // operations on names below each directory
}

// add records op on paths.
func (x *opIndex) add(op *hostOp, paths []string) {
	if x.at == nil {
		x.at = make(map[string][]*hostOp)
		x.below = make(map[string]map[*hostOp]struct{})
	}
	for _, q := range paths {
		x.at[q] = append(x.at[q], op)
		for a := q; a != ""; {
			a = parent(a)
			if x.below[a] == nil {
				x.below[a] = make(map[*hostOp]struct{})
			}
			x.below[a][op] = struct{}{}
		}
	}
}

// remove forgets op on paths.
func (x *opIndex) remove(op *hostOp, paths []string) {
	for _, q := range paths {
		ops := x.at[q]
		for i, o := range ops {
			if o == op {
				ops = append(ops[:i:i], ops[i+1:]...)
				break
			}
		}
		if len(ops) == 0 {
			delete(x.at, q)
		} else {
			x.at[q] = ops
		}
		for a := q; a != ""; {
			a = parent(a)
			delete(x.below[a], op)
			if len(x.below[a]) == 0 {
				delete(x.below, a)
			}
		}
	}
}

// each calls fn for the operations on p, an ancestor or a descendant of p,
// an operation on several of them possibly more than once, until fn
// returns false; it reports whether fn stopped it.
func (x *opIndex) each(p string, fn func(*hostOp) bool) bool {
	for a := p; ; a = parent(a) {
		for _, op := range x.at[a] {
			if !fn(op) {
				return true
			}
		}
		if a == "" {
			break
		}
	}
	for op := range x.below[p] {
		if !fn(op) {
			return true
		}
	}
	return false
}

// first returns an operation on any of paths, their ancestors or
// descendants that match accepts; nil when none does.
func (x *opIndex) first(paths []string, match func(*hostOp) bool) *hostOp {
	var found *hostOp
	for _, p := range paths {
		if x.each(p, func(op *hostOp) bool {
			if match(op) {
				found = op
				return false
			}
			return true
		}) {
			return found
		}
	}
	return nil
}
