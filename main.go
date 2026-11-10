package main

import (
	"embed"
	"log"
	"runtime"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	backend := newApp()
	var wailsApp *application.App
	wailsApp = application.New(application.Options{
		Name:        "Excalibur",
		Description: "The board of legend",
		ShouldQuit: func() bool {
			if backend.shutdownReady.Load() {
				return true
			}
			if backend.shutdownSaving.CompareAndSwap(false, true) {
				go func() {
					if err := backend.prepareShutdown(); err != nil {
						log.Printf("could not save changes before quit: %v", err)
						backend.shutdownSaving.Store(false)
						return
					}
					wailsApp.Quit()
				}()
			}
			return false
		},
		Services: []application.Service{
			application.NewService(backend),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	})
	backend.wailsApp = wailsApp

	menu := nativeMenu(backend)
	wailsApp.Menu.Set(menu)
	backend.window = wailsApp.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:              "Excalibur",
		Width:              1400,
		Height:             900,
		MinWidth:           760,
		MinHeight:          480,
		BackgroundColour:   application.NewRGB(248, 248, 248),
		URL:                "/",
		Zoom:               defaultApplicationZoom,
		ZoomControlEnabled: true,
	})
	// ServiceShutdown runs after the webview is gone, which is too late to ask
	// Canvas for its pending scene. Cancel the first close request, persist while
	// the renderer is alive, then close for real. A failed barrier leaves the
	// window open so the user can retry instead of silently losing changes.
	var removeCloseHook func()
	removeCloseHook = backend.window.RegisterHook(events.Common.WindowClosing, func(event *application.WindowEvent) {
		event.Cancel()
		if !backend.shutdownSaving.CompareAndSwap(false, true) {
			return
		}
		go func() {
			if err := backend.prepareShutdown(); err != nil {
				log.Printf("could not save changes before quit: %v", err)
				backend.shutdownSaving.Store(false)
				return
			}
			removeCloseHook()
			backend.window.Close()
		}()
	})
	backend.window.SetMenu(menu)

	if err := wailsApp.Run(); err != nil {
		log.Fatal(err)
	}
}

func nativeMenu(app *App) *application.Menu {
	root := application.NewMenu()
	emit := func(id string) func(*application.Context) {
		return func(*application.Context) { app.emit("menu:"+id, map[string]any{}) }
	}
	add := func(menu *application.Menu, label, accelerator, id string) {
		item := menu.Add(label).OnClick(emit(id))
		if accelerator != "" {
			item.SetAccelerator(accelerator)
		}
	}

	if runtime.GOOS == "darwin" {
		// The menu under the app name in the menu bar. macOS makes the first
		// top-level menu the application menu; roles resolve inside it, so we
		// build it by hand to place Settings (the native equivalent of the
		// sidebar gear) right after About, as standard macOS apps do.
		appMenu := root.AddSubmenu("Excalibur")
		appMenu.AddRole(application.About)
		appMenu.AddSeparator()
		add(appMenu, "Settings…", "CmdOrCtrl+,", "settings")
		appMenu.AddSeparator()
		appMenu.AddRole(application.ServicesMenu)
		appMenu.AddSeparator()
		appMenu.AddRole(application.Hide)
		appMenu.AddRole(application.HideOthers)
		appMenu.AddRole(application.UnHide)
		appMenu.AddSeparator()
		appMenu.AddRole(application.Quit)
	}

	file := root.AddSubmenu("File")
	add(file, "New Board…", "CmdOrCtrl+N", "new-board")
	add(file, "Import Boards…", "CmdOrCtrl+O", "import")
	add(file, "Choose Boards Folder…", "", "choose-folder")
	add(file, "Show in File Manager", "CmdOrCtrl+Shift+R", "reveal")
	file.AddSeparator()
	add(file, "Rename Board…", "", "rename")
	add(file, "Duplicate Board", "", "duplicate")
	add(file, "Move to Trash", "", "trash")
	if runtime.GOOS != "darwin" {
		file.AddSeparator()
		add(file, "Settings…", "CmdOrCtrl+,", "settings")
		file.Add("Quit").SetAccelerator("CmdOrCtrl+Q").OnClick(func(*application.Context) { app.window.Close() })
	}

	if runtime.GOOS == "darwin" {
		// macOS routes the native Delete role to the focused text control.
		root.AddRole(application.EditMenu)
	} else {
		// Native Undo/Redo/Delete roles consume their accelerators before GTK/WebView
		// can deliver them to Excalidraw. Route undo/redo back into the webview and
		// omit native Delete so canvas shortcuts remain owned by Excalidraw.
		edit := root.AddSubmenu("Edit")
		add(edit, "Undo", "CmdOrCtrl+Z", "undo")
		add(edit, "Redo", "CmdOrCtrl+Shift+Z", "redo")
		edit.AddSeparator()
		edit.AddRole(application.Cut)
		edit.AddRole(application.Copy)
		edit.AddRole(application.Paste)
		edit.AddSeparator()
		edit.AddRole(application.SelectAll)
	}
	view := root.AddSubmenu("View")
	view.AddRadio("Sort by Name", true).OnClick(emit("sort-name"))
	view.AddRadio("Sort by Date Edited", false).OnClick(emit("sort-date"))
	view.AddSeparator()
	view.AddCheckbox("Show Thumbnails", true).OnClick(emit("show-thumbnails"))
	view.AddSeparator()
	view.Add("Zoom In").SetAccelerator("CmdOrCtrl+plus").OnClick(func(*application.Context) { _ = app.ChangeApplicationZoom(applicationZoomStep) })
	view.Add("Zoom Out").SetAccelerator("CmdOrCtrl+-").OnClick(func(*application.Context) { _ = app.ChangeApplicationZoom(-applicationZoomStep) })
	view.Add("Actual Size").SetAccelerator("CmdOrCtrl+0").OnClick(func(*application.Context) { _ = app.ResetApplicationZoom() })
	view.AddSeparator()
	view.Add("Toggle Full Screen").OnClick(func(*application.Context) { app.ToggleFullscreen() })
	root.AddRole(application.WindowMenu)
	return root
}
