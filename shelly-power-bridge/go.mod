// SPDX-License-Identifier: AGPL-3.0-or-later

module shelly-power-bridge

go 1.26.5

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/eclipse/paho.mqtt.golang v1.5.1
)

require (
	codeberg.org/kgbvax/stationa/shared v0.0.0
	github.com/gorilla/websocket v1.5.3 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
)

replace codeberg.org/kgbvax/stationa/shared => ../shared
