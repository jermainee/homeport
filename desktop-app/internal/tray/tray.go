// Package tray puts Homeport in the menu bar (the system tray on Linux and
// Windows): what Share is doing, a Share switch, Open and Quit. Closing the
// window leaves Share running there; Quit stops it.
package tray

//go:generate sh -c "cd icons && go run gen.go"

import (
	"context"
	_ "embed"
	"runtime"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/jermainee/homeport/desktop-app/services"
)

// Template icons, tinted by macOS: a house outline while Share is off and a
// filled house while it is on. icons/gen.go draws them.
var (
	//go:embed icons/share-off.png
	iconOff []byte
	//go:embed icons/share-on.png
	iconOn []byte
)

// New adds the tray icon and its menu. window is the main window, which
// Open shows.
func New(app *application.App, window application.Window, share *services.ShareService) {
	menu := app.NewMenu()
	status := menu.Add(Label(share.Status())).SetEnabled(false)
	toggle := menu.AddCheckbox(switchLabel(), false)
	menu.AddSeparator()
	menu.Add("Open Homeport").OnClick(func(*application.Context) { Show(app, window) })
	menu.AddSeparator()
	menu.Add("Quit Homeport").OnClick(func(*application.Context) { app.Quit() })

	tray := app.SystemTray.New()
	tray.SetTemplateIcon(iconOff).SetMenu(menu)
	tray.SetTooltip("Homeport")

	toggle.OnClick(func(ctx *application.Context) {
		st := share.SetSharing(context.Background(), ctx.IsChecked())
		if st.State == services.ShareError {
			// The window explains what to do, such as verifying first.
			Show(app, window)
		}
	})
	share.OnStatus(func(st services.ShareStatus) {
		status.SetLabel(Label(st))
		toggle.SetChecked(st.Sharing)
		if st.Sharing {
			tray.SetTemplateIcon(iconOn)
		} else {
			tray.SetTemplateIcon(iconOff)
		}
	})
}

// Show brings the window to the front.
func Show(app *application.App, window application.Window) {
	app.Show()
	window.Show()
	window.Focus()
}

func switchLabel() string {
	if runtime.GOOS == "darwin" {
		return "Share This Mac's Line"
	}
	return "Share This Computer's Line"
}

// Label says what Share is doing, in the menu's first line.
func Label(st services.ShareStatus) string {
	switch st.State {
	case services.ShareConnecting:
		return "Share: connecting…"
	case services.ShareOnline:
		return "Share: online"
	case services.SharePaused:
		return "Share: paused while another VPN is on"
	case services.ShareOffline:
		if st.Error == services.ErrGatewayUnavailable {
			return "Share: waiting for a gateway…"
		}
		return "Share: offline, trying again…"
	case services.ShareError:
		if st.Error == services.ErrIdentityRequired {
			return "Share: verify your identity first"
		}
		return "Share stopped. Open Homeport to see why."
	default:
		return "Share is off"
	}
}
