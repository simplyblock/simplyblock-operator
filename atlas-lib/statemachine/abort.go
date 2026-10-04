// Asking the graph whether the work can still be called off.
//
// [StateDef.Abortable] is where the answer is declared, and this file is the
// three ways it is read. A caller holding a machine asks [Machine.CanAbort],
// which is the precise question, because the machine was built for one action
// and knows which state it is in. A caller holding a state and no machine — an
// admission webhook reading a step out of a status — asks one of the graph
// queries instead.
//
// They live beside the field rather than in statemachine.go because the property
// is one concern, and a reader who has found any part of it has found all of it.

package statemachine

import "slices"

// CanAbort reports whether the state the machine is in declares itself
// abortable, which is the graph's answer to a caller asking to stop:
//
//	if !machine.CanAbort() {
//		// Not a failure of the operation: it carries on, and what the caller
//		// asked for is what could not be done.
//		return r.note(ctx, ops, "the abort arrived too late to be honored")
//	}
//
// It says nothing about how the stop is then performed. Unwinding whatever the
// earlier states started is the caller's, because only the caller knows what
// they were.
func (sm *Machine[S]) CanAbort() bool {
	return sm.states[sm.current].Abortable
}

// UnabortableStates returns the states a graph declares no abort from, sorted.
//
// It exists for the reader that has a state and no machine, and for the test
// that holds a table beside the graph honest. A guard refusing to withdraw the
// record of a running operation is both: it reads a step out of a status, and it
// keeps its own table of what each step is in the middle of, which only agrees
// with the graph if something makes it.
func UnabortableStates[S ~string](config Config[S]) []S {
	return unabortable(config.States)
}

// UnabortableMultiStates returns the states no action can be stopped from, as
// the union across every declared graph, sorted.
//
// The union is the answer a table keyed on the state alone can hold, since one
// state shared by two actions has one entry and may have two answers. Taking the
// union rather than the intersection is the conservative half of that: where two
// actions disagree, the state is reported, so a guard reading this refuses where
// it might have admitted rather than the other way round. A caller that needs
// the precise answer has the action in hand and should ask that graph, or ask
// [Machine.CanAbort] on the machine built from it.
func UnabortableMultiStates[S ~string](graphs MultiConfig[S]) []S {
	union := make(map[S]StateDef[S])
	for _, config := range graphs {
		for state, def := range config.States {
			if previous, seen := union[state]; seen && !previous.Abortable {
				// Already reported by another action. Keeping the stricter of
				// the two is what makes this a union of the unabortable rather
				// than a last-writer-wins over the map.
				continue
			}
			union[state] = def
		}
	}
	return unabortable(union)
}

// unabortable is the filter both queries end in.
func unabortable[S ~string](states map[S]StateDef[S]) []S {
	var refused []S
	for state, def := range states {
		if !def.Abortable {
			refused = append(refused, state)
		}
	}
	slices.Sort(refused)
	return refused
}
