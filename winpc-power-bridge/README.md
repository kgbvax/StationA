# winpc-power-bridge

Sleep agent for the Windows shack PC (`bwpc`) in the Mühle station automation
ecosystem. It publishes the station integration-model slot
`muehle/power/shack-pc/{meta,state,status,cmd}` and puts the PC to sleep (S3)
on a non-retained `/cmd` `{"action":"sleep"}` — but only when nobody has used
keyboard or mouse for `min_user_idle_minutes` (default 30). `powerseq` wakes
the PC again with Wake-on-LAN.

It runs as a hidden Task Scheduler logon task in the interactive user session
(not a service: the idle time is per session).

See `CLAUDE.md` for architecture, deployment and the bwpc prerequisites, and
`docs/winpc-power-bridge-mqtt-api.md` for the on-the-wire contract. Shared
conventions live in `../docs/`.

---

## Build / test / deploy

```bash
go test ./...
GOOS=windows GOARCH=amd64 go build ./...
./deploy.sh
```

## License

AGPL-3.0-or-later — see `LICENSE`.
