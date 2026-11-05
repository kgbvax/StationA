// Package desktop shows the page in a native window (glaze: WebView2 on
// Windows, WKWebView on macOS, WebKitGTK on Linux; cgo-free). It is the only
// package that imports glaze. Like qslotter, the window loads the loopback
// server by URL, never through glaze's app:// scheme, which buffers SSE.
package desktop

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"

	"github.com/crgimenes/glaze"
)

// Window opens the native window on url and runs its loop until it is closed
// or ctx is done. It must be called on the main OS thread (the caller locks
// it in init). When no WebView is available it returns an error so the
// caller can fall back to the browser.
func Window(ctx context.Context, url, title string, width, height int) error {
	w, err := glaze.New(false)
	if err != nil {
		return fmt.Errorf("no native window: %w", err)
	}
	defer w.Destroy()
	w.SetTitle(title)
	setWindowIcon(w.Window())
	w.SetSize(width, height, glaze.HintNone)
	w.Navigate(url)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			w.Terminate()
		case <-done:
		}
	}()
	w.Run()
	return nil
}

// OpenBrowser opens url in the default browser.
func OpenBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
