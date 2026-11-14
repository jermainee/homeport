package tray

import (
	"slices"
	"testing"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// recorder notes each AppKit call and whether it ran on the main thread.
type recorder struct {
	onMain bool
	calls  []string
}

func (r *recorder) note(call string) {
	if r.onMain {
		call += " on main"
	}
	r.calls = append(r.calls, call)
}

func (r *recorder) invoke(fn func()) {
	r.onMain = true
	defer func() { r.onMain = false }()
	fn()
}

type fakeApp struct{ r *recorder }

func (a fakeApp) Show() { a.r.note("app.Show") }

// fakeWindow embeds the interface so it only has to answer the calls Show makes.
type fakeWindow struct {
	application.Window
	r *recorder
}

func (w fakeWindow) Show() application.Window { w.r.note("window.Show"); return w }
func (w fakeWindow) Focus()                   { w.r.note("window.Focus") }

// Menu clicks and app events arrive on their own goroutines, and AppKit
// aborts the app when it is touched off the main thread, as clicking the
// Share switch while signed out did.
func TestShowTouchesAppKitOnlyOnTheMainThread(t *testing.T) {
	r := &recorder{}

	show(r.invoke, fakeApp{r}, fakeWindow{r: r})

	want := []string{"app.Show on main", "window.Show on main", "window.Focus on main"}
	if !slices.Equal(r.calls, want) {
		t.Fatalf("calls = %q, want %q", r.calls, want)
	}
}
