// SPDX-License-Identifier: AGPL-3.0-or-later

module chase-queue

go 1.26.5

require (
	codeberg.org/kgbvax/stationa/shared v0.0.0-00010101000000-000000000000
	github.com/BurntSushi/toml v1.6.0
	github.com/eclipse/paho.mqtt.golang v1.5.1
	modernc.org/sqlite v1.58.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	modernc.org/libc v1.75.6 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)

replace codeberg.org/kgbvax/stationa/shared => ../shared
