// What the graph says about stopping, and who may ask.
//
// The property is declared per state and read three ways: by the machine, for
// the caller holding one; by a graph query, for the guard that has a state and
// no machine; and across a MultiConfig, for the one table a kind keeps beside
// several actions.

package statemachine

import (
	"context"
	"slices"
	"testing"
)

type abortState string

const (
	abortPending  abortState = "Pending"
	abortWorking  abortState = "Working"
	abortFinished abortState = "Finished"
)

// abortGraph is a line of three: nothing has started, something has, and it is
// over. Only the first can be called off.
func abortGraph() Config[abortState] {
	return Config[abortState]{
		Initial: abortPending,
		States: map[abortState]StateDef[abortState]{
			abortPending:  {To: []abortState{abortWorking}, Abortable: true},
			abortWorking:  {To: []abortState{abortFinished}},
			abortFinished: {},
		},
	}
}

// The machine answers for the state it is in, which is the question a caller
// holding one actually has: it knows the action and the position, and wants to
// know whether the stop it was asked for can be honored.
func TestCanAbortAnswersForTheCurrentState(t *testing.T) {
	sm, err := New(context.Background(), abortGraph())
	if err != nil {
		t.Fatalf("build the machine: %v", err)
	}
	defer sm.Close()

	if !sm.CanAbort() {
		t.Error("the initial state declares Abortable and the machine says it cannot be aborted")
	}

	if err := sm.TransitionTo(context.Background(), abortWorking); err != nil {
		t.Fatalf("move to Working: %v", err)
	}
	if sm.CanAbort() {
		t.Error("Working declares no Abortable and the machine says it can be aborted")
	}
}

// A state that says nothing is not abortable. The default is what carries the
// rule, because a graph that has to opt out of stopping would make forgetting
// the field the dangerous direction.
func TestAStateThatSaysNothingIsNotAbortable(t *testing.T) {
	config := Config[abortState]{
		Initial: abortWorking,
		States:  map[abortState]StateDef[abortState]{abortWorking: {}},
	}
	sm, err := New(context.Background(), config)
	if err != nil {
		t.Fatalf("build the machine: %v", err)
	}
	defer sm.Close()

	if sm.CanAbort() {
		t.Error("a state declaring nothing reports itself abortable")
	}
}

// Being terminal and being abortable are different questions, and nothing
// derives one from the other: a terminal state is where work ended, which is not
// a place work can be called off from.
func TestATerminalStateIsNotAbortableByItself(t *testing.T) {
	if got := UnabortableStates(abortGraph()); !slices.Contains(got, abortFinished) {
		t.Errorf("the unabortable states are %v, want the terminal Finished among them", got)
	}
}

// The graph query is for a caller with a state and no machine, such as an
// admission webhook reading a step out of a status.
func TestUnabortableStatesListsWhatTheGraphWillNotStop(t *testing.T) {
	want := []abortState{abortFinished, abortWorking}
	if got := UnabortableStates(abortGraph()); !slices.Equal(got, want) {
		t.Errorf("the unabortable states are %v, want %v sorted", got, want)
	}
}

// Across a MultiConfig the answer is the union, because a table keyed on the
// state alone cannot hold two answers for one state. The union is the
// conservative half of that: a state one action cannot be stopped from is
// reported, so a guard reading this refuses rather than admits where the two
// actions disagree.
func TestUnabortableMultiStatesUnionsTheActions(t *testing.T) {
	graphs := MultiConfig[abortState]{
		"quick": {
			Initial: abortPending,
			States: map[abortState]StateDef[abortState]{
				abortPending: {Abortable: true},
			},
		},
		"slow": {
			Initial: abortPending,
			States: map[abortState]StateDef[abortState]{
				// The same state, and this action has already started something
				// by the time it is here.
				abortPending: {To: []abortState{abortWorking}},
				abortWorking: {},
			},
		},
	}

	want := []abortState{abortPending, abortWorking}
	if got := UnabortableMultiStates(graphs); !slices.Equal(got, want) {
		t.Errorf("the unabortable states are %v, want %v", got, want)
	}
}

// A graph that can be stopped from everywhere reports nothing, rather than
// reporting every state.
func TestAFullyAbortableGraphHasNoUnabortableStates(t *testing.T) {
	graphs := MultiConfig[abortState]{
		"discover": {
			Initial: abortPending,
			States: map[abortState]StateDef[abortState]{
				abortPending: {To: []abortState{abortWorking}, Abortable: true},
				abortWorking: {Abortable: true},
			},
		},
	}

	if got := UnabortableMultiStates(graphs); len(got) != 0 {
		t.Errorf("a graph abortable from every state reports %v as unabortable", got)
	}
}
