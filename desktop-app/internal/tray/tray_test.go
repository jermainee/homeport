package tray

import (
	"reflect"
	"slices"
	"testing"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/jermainee/homeport/desktop-app/services"
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

type fakeToggle struct{ checked []bool }

func (f *fakeToggle) SetChecked(checked bool) *application.MenuItem {
	f.checked = append(f.checked, checked)
	return nil
}

// A click has already ticked the box when Share fails to start, and the
// status that follows still says Share is off, so nothing else unticks it.
func TestToggledUnticksTheSwitchWhenShareFailsToStart(t *testing.T) {
	toggle := &fakeToggle{}
	shown := false

	toggled(func(on bool) services.ShareStatus {
		return services.ShareStatus{State: services.ShareError, Error: "signed_out"}
	}, true, toggle, func() { shown = true })

	if !slices.Equal(toggle.checked, []bool{false}) {
		t.Errorf("switch set to %v, want [false]", toggle.checked)
	}
	if !shown {
		t.Error("the window did not open to say why")
	}
}

func TestToggledKeepsTheSwitchOnWhileShareStarts(t *testing.T) {
	toggle := &fakeToggle{}
	shown := false

	toggled(func(on bool) services.ShareStatus {
		return services.ShareStatus{State: services.ShareConnecting, Sharing: true}
	}, true, toggle, func() { shown = true })

	if !slices.Equal(toggle.checked, []bool{true}) {
		t.Errorf("switch set to %v, want [true]", toggle.checked)
	}
	if shown {
		t.Error("the window opened although nothing went wrong")
	}
}

// show's test passes its own dispatcher, so this pins what Show passes.
func TestShowDispatchesThroughWails(t *testing.T) {
	if reflect.ValueOf(onMainThread).Pointer() != reflect.ValueOf(application.InvokeSync).Pointer() {
		t.Fatal("Show must run on the main thread through application.InvokeSync")
	}
}
