# Validation Log — m5dualkey-hf-antctrl (ex m5btn2)

Date: 2026-06-07
Firmware branch/commit: ea79ba9 (live-verified 2026-10-04 against ultrabridge; device at 192.168.1.122 / m5dualkey-antctrl-1.local)
Tester: _TBD_

## Build
- [x] `pio run -e m5stack_chain_dualkey` completed successfully
- Notes:
	- Build date: 2026-06-07
	- Result: SUCCESS
	- RAM: 13.7% (44800 / 327680 bytes)
	- Flash: 21.1% (704221 / 3342336 bytes)

## Functional checks
- [ ] Startup banner appears on serial within 2s
- [x] Button A short press emits serial event and publishes `{"action":"direction","value":"forward"}` to `muehle/hf/ant-ctrl/cmd`
- [x] Button B short press emits serial event and publishes direction `reverse`
- [x] A+B combo press emits serial event and publishes direction `bidirectional`
- [ ] Long press on A emits `LONG_PRESS`
- [ ] Long press on B emits `LONG_PRESS`
- [ ] Rapid tapping does not create bounce duplicates

## MQTT + LED checks
- [x] Subscribe handling from `muehle/hf/ant-ctrl/state` + `/status` works
- [ ] Bridge `/status` offline or `device_online:false` -> LEDs off, presses not sent
- [ ] `direction=forward` -> LED A green
- [ ] `direction=reverse` -> LED B red
- [ ] `direction=bidirectional` -> both LEDs orange

## OTA checks
- [ ] Serial shows `OTA,LISTENING,host=m5dualkey-antctrl-1.local`
- [ ] `./deploy.sh` (OTA) flashes and the key reboots into the new image
- [ ] OTA with a wrong password is rejected

## Reconnect checks
- [ ] Wi-Fi reconnect after AP outage
- [ ] MQTT reconnect after broker restart

## Stability
- [ ] Extended run completed (target: 24h)
- Notes:

## Issues found
- None / details:
