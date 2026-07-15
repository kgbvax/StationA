package ui

import (
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
)

func newTestModel() *Model {
	inputs := make([]textinput.Model, fieldCount)
	for i := range inputs {
		inputs[i] = textinput.New()
	}
	return &Model{inputs: inputs, focus: focusNone}
}

func TestCycleFocusForward(t *testing.T) {
	m := newTestModel()
	want := []int{
		focusAddr, focusPan, focusTilt, focusEndpoint,
		focusGS232, focusGS232On, focusRotctld, focusRotctldOn, focusWrapLimit,
		focusNone, focusAddr,
	}
	for i, w := range want {
		m.cycleFocus(1)
		if m.focus != w {
			t.Fatalf("tab #%d: focus = %d want %d", i+1, m.focus, w)
		}
	}
}

func TestCycleFocusBackward(t *testing.T) {
	m := newTestModel()
	m.cycleFocus(-1)
	if m.focus != focusWrapLimit {
		t.Fatalf("shift+tab from none: focus = %d want %d", m.focus, focusWrapLimit)
	}
}
