// Command homeport is the Homeport desktop app: one window to sign in, then
// Share this Mac's home line or Use another member's. Share keeps running in
// the menu bar when the window is closed; Quit stops it.
package main

import (
	"embed"
	"log"
	"log/slog"
	"os"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"github.com/jermainee/homeport/desktop-app/internal/apiclient"
	"github.com/jermainee/homeport/desktop-app/internal/profile"
	"github.com/jermainee/homeport/desktop-app/internal/tray"
	"github.com/jermainee/homeport/desktop-app/services"
)

// The built frontend. Git tracks frontend/dist/.gitkeep, so this compiles
// on a fresh clone before the frontend is built.
//
//go:embed all:frontend/dist
var assets embed.FS

// ShareStatusEvent carries every change of Share's status to the window.
const ShareStatusEvent = "share:status"

func init() {
	application.RegisterEvent[services.ShareStatus](ShareStatusEvent)
}

func main() {
	dir, err := profile.Dir(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	prof, err := profile.Open(dir)
	if err != nil {
		log.Fatal(err)
	}
	api := apiclient.New(apiclient.BaseURL(os.Getenv), nil)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	auth := services.NewAuthService(api, prof)
	share := services.NewShareService(api, prof, services.ShareOptions{Logger: logger})
	auth.BeforeSignOut(share.SignOut)
	auth.AfterSignIn(share.SignIn)

	app := application.New(application.Options{
		Name:        "Homeport",
		Description: "Share your home internet line, or use another member's.",
		Services: []application.Service{
			application.NewService(auth),
			application.NewService(share),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Mac: application.MacOptions{
			// Share keeps running in the menu bar without a window.
			ApplicationShouldTerminateAfterLastWindowClosed: false,
		},
	})

	window := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:     "Homeport",
		Width:     460,
		Height:    640,
		MinWidth:  380,
		MinHeight: 520,
		Mac: application.MacWindow{
			InvisibleTitleBarHeight: 44,
			TitleBar:                application.MacTitleBarHiddenInset,
		},
		// The --wall colour, so the window never flashes white or black.
		BackgroundColour: application.NewRGB(232, 236, 233),
		URL:              "/",
	})
	// Closing the window hides it; Share goes on in the menu bar.
	window.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		window.Hide()
		e.Cancel()
	})
	// A click on the Dock icon brings the window back.
	app.Event.OnApplicationEvent(events.Mac.ApplicationShouldHandleReopen, func(*application.ApplicationEvent) {
		tray.Show(app, window)
	})

	share.OnStatus(func(st services.ShareStatus) {
		app.Event.Emit(ShareStatusEvent, st)
	})
	tray.New(app, window, share)

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
