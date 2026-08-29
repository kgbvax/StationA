module ultrabridge

go 1.26.5

require (
	codeberg.org/kgbvax/stationa/shared v0.0.0
	github.com/eclipse/paho.mqtt.golang v1.5.1
	github.com/pelletier/go-toml/v2 v2.4.3
	go.bug.st/serial v1.8.0
)

require (
	github.com/gorilla/websocket v1.5.3 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

replace codeberg.org/kgbvax/stationa/shared => ../shared
