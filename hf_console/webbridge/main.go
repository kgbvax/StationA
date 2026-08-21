// Command hf-console-web serves the Flutter hf_console web build over HTTP and
// forwards MQTT between WebSocket clients and the station broker. The browser
// cannot open raw TCP sockets, so this bridge provides a WebSocket endpoint at
// /mqtt that is byte-forwarded to the MQTT broker's TCP port.
package main

import (
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

var (
	listenAddr = flag.String("listen", "0.0.0.0:8091", "HTTP listen address")
	brokerAddr = flag.String("mqtt-broker", "192.168.1.50:1883", "MQTT broker TCP address")
	webRoot    = flag.String("web-root", "/opt/hf-console-web/build/web", "directory containing the Flutter web build")
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		// Served on the LAN; allow any origin so the browser can load the page
		// from shari and connect back to the same host.
		return true
	},
}

func main() {
	flag.Parse()

	fs := http.FileServer(http.Dir(*webRoot))
	http.Handle("/", fs)
	http.HandleFunc("/mqtt", handleMQTT)

	log.Printf("hf-console-web listening on %s, serving %s, broker %s", *listenAddr, *webRoot, *brokerAddr)
	if err := http.ListenAndServe(*listenAddr, nil); err != nil {
		log.Fatalf("listen: %v", err)
	}
}

// handleMQTT upgrades the HTTP request to a WebSocket and then copies bytes
// bidirectionally to the MQTT broker. No MQTT framing is performed here; the
// browser's mqtt_client sends raw MQTT packets over the WebSocket and the
// broker speaks raw MQTT over TCP.
func handleMQTT(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("websocket upgrade: %v", err)
		return
	}
	defer ws.Close()

	conn, err := net.Dial("tcp", *brokerAddr)
	if err != nil {
		log.Printf("broker connect %s: %v", *brokerAddr, err)
		return
	}
	defer conn.Close()

	var wg sync.WaitGroup
	wg.Add(2)

	// WebSocket -> broker
	go func() {
		defer wg.Done()
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				if !isCloseError(err) {
					log.Printf("websocket read: %v", err)
				}
				return
			}
			if mt == websocket.CloseMessage {
				return
			}
			if mt != websocket.BinaryMessage && mt != websocket.TextMessage {
				continue
			}
			if _, err := conn.Write(data); err != nil {
				log.Printf("broker write: %v", err)
				return
			}
		}
	}()

	// Broker -> WebSocket
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				if err != io.EOF {
					log.Printf("broker read: %v", err)
				}
				return
			}
			if n == 0 {
				continue
			}
			if err := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
				if !isCloseError(err) {
					log.Printf("websocket write: %v", err)
				}
				return
			}
		}
	}()

	wg.Wait()
}

func isCloseError(err error) bool {
	if err == nil {
		return false
	}
	return websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) ||
		strings.Contains(err.Error(), "use of closed network connection") ||
		strings.Contains(err.Error(), "broken pipe")
}
