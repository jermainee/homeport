// Command homeport is the Homeport desktop app: one window to sign in, then
// Share this Mac's home line or Use another member's.
package main

import (
	"embed"
	"log"
	"os"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/jermainee/homeport/desktop-app/internal/apiclient"
	"github.com/jermainee/homeport/desktop-app/internal/profile"
	"github.com/jermainee/homeport/desktop-app/services"
)

// The built frontend. Git tracks frontend/dist/.gitkeep, so this compiles
// on a fresh clone before the frontend is built.
//
//go:embed all:frontend/dist
var assets embed.FS

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

	app := application.New(application.Options{
		Name:        "Homeport",
		Description: "Share your home internet line, or use another member's.",
		Services: []application.Service{
			application.NewService(services.NewAuthService(api, prof)),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	})

	app.Window.NewWithOptions(application.WebviewWindowOptions{
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

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
