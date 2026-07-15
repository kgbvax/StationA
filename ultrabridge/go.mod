module ultrabridge

go 1.26.5

require (
	codeberg.org/kgbvax/stationa/shared v0.0.0
	github.com/eclipse/paho.mqtt.golang v1.5.1
	github.com/pelletier/go-toml/v2 v2.4.2
	go.bug.st/serial v1.6.4
)

require (
	github.com/creack/goselect v0.1.2 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	golang.org/x/net v0.44.0 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/sys v0.36.0 // indirect
)

replace codeberg.org/kgbvax/stationa/shared => ../shared
