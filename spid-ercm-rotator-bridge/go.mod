// SPDX-License-Identifier: AGPL-3.0-or-later

module spid-ercm-rotator-bridge

go 1.26.5

require (
	codeberg.org/kgbvax/stationa/shared v0.0.0
	github.com/BurntSushi/toml v1.6.0
)

require (
	go.bug.st/serial v1.8.0 // indirect
	golang.org/x/sys v0.43.0 // indirect
)

replace codeberg.org/kgbvax/stationa/shared => ../shared
