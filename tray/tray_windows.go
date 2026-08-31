package tray

import (
	"fmt"

	"github.com/getlantern/systray"
)

// Tray provides a minimal system-tray interface for the proxy.
type Tray struct {
	onConnect    func()
	onDisconnect func()
	onExit       func()

	statusItem     *systray.MenuItem
	connectItem    *systray.MenuItem
	disconnectItem *systray.MenuItem
}

// New creates a Tray with the given callbacks.
func New(onConnect, onDisconnect, onExit func()) *Tray {
	return &Tray{
		onConnect:    onConnect,
		onDisconnect: onDisconnect,
		onExit:       onExit,
	}
}

// Run starts the tray event loop. It blocks until Exit is called.
func (t *Tray) Run() {
	systray.Run(t.onReady, t.onExit)
}

// Exit stops the tray event loop.
func (t *Tray) Exit() {
	systray.Quit()
}

func (t *Tray) onReady() {
	systray.SetTitle("PN")
	systray.SetTooltip("proxyNet - click to control")

	t.statusItem = systray.AddMenuItem("Status: disconnected", "Current status")
	t.statusItem.Disable()

	systray.AddSeparator()

	t.connectItem = systray.AddMenuItem("Connect", "Start proxy tunnel")
	t.disconnectItem = systray.AddMenuItem("Disconnect", "Stop proxy tunnel")
	t.disconnectItem.Hide()

	mExit := systray.AddMenuItem("Exit", "Quit proxyNet")

	go func() {
		for {
			select {
			case <-t.connectItem.ClickedCh:
				if t.onConnect != nil {
					go t.onConnect()
				}
			case <-t.disconnectItem.ClickedCh:
				if t.onDisconnect != nil {
					go t.onDisconnect()
				}
			case <-mExit.ClickedCh:
				if t.onExit != nil {
					go t.onExit()
				}
				return
			}
		}
	}()
}

// SetStatus updates the status text shown in the tray menu.
func (t *Tray) SetStatus(status string) {
	if t.statusItem != nil {
		t.statusItem.SetTitle(fmt.Sprintf("Status: %s", status))
	}
	systray.SetTooltip(fmt.Sprintf("proxyNet - %s", status))
}

// SetConnected updates the menu so that Disconnect is visible when connected.
func (t *Tray) SetConnected(connected bool) {
	if connected {
		t.connectItem.Hide()
		t.disconnectItem.Show()
	} else {
		t.disconnectItem.Hide()
		t.connectItem.Show()
	}
}
