package acom

import (
	"fmt"
	"strings"
)

// BandOptions is the amplifier's own band list, ordered by the amp's internal
// band index (1..10). The ACOM 1200S has no 60m band and the band-change
// command only walks among these.
var BandOptions = []string{"160m", "80m", "40m", "30m", "20m", "17m", "15m", "12m", "10m", "6m"}

// bandNameMap maps a band label (or bare number) to the amp's band index 1..10.
var bandNameMap = map[string]int{
	"160m": 1, "160": 1,
	"80m": 2, "80": 2,
	"40m": 3, "40": 3,
	"30m": 4, "30": 4,
	"20m": 5, "20": 5,
	"17m": 6, "17": 6,
	"15m": 7, "15": 7,
	"12m": 8, "12": 8,
	"10m": 9, "10": 9,
	"6m": 10, "6": 10,
}

// BandNameToIndex resolves a band label (e.g. "20m" or "20") to the amp's band
// index. ok is false for unknown bands.
func BandNameToIndex(name string) (int, bool) {
	idx, ok := bandNameMap[strings.ToLower(strings.TrimSpace(name))]
	return idx, ok
}

// decodeBand maps the amp's band byte (low nibble) to a canonical band label.
// 0 and anything outside 1..10 yield "UNK".
func decodeBand(b byte) string {
	switch b & 0x0F {
	case 1:
		return "160m"
	case 2:
		return "80m"
	case 3:
		return "40m"
	case 4:
		return "30m"
	case 5:
		return "20m"
	case 6:
		return "17m"
	case 7:
		return "15m"
	case 8:
		return "12m"
	case 9:
		return "10m"
	case 10:
		return "6m"
	default:
		return "UNK"
	}
}

// decodeMode returns the raw firmware mode string for the mode byte. These are
// ACOM amplifier operational states (not stationa RF modes); the bridge
// canonicalizes them via CanonicalMode / CanonicalKeyed.
func decodeMode(b byte) string {
	switch b & 0xF0 {
	case 0x10:
		return "RESET"
	case 0x20:
		return "INIT"
	case 0x30:
		return "DEBUG"
	case 0x40:
		return "SERVICE"
	case 0x50:
		return "STANDBY"
	case 0x60:
		return "OPR/RX"
	case 0x70:
		return "OPR/TX"
	case 0x80:
		return "ATAC"
	case 0x90:
		return "MENU"
	case 0xA0:
		return "OFF"
	default:
		return "UNKNOWN"
	}
}

// decodeError returns the human-readable fault message for a fault byte. 0xFF
// means no fault.
func decodeError(b byte) string {
	if b == 0xFF {
		return "NONE"
	}
	if b == 0x00 {
		return "HOT SWITCHING ATTEMPT"
	}

	switch b {
	case 0x01:
		return "OUTPUT RELAY CLOSED (SHOULD BE OPEN)"
	case 0x02:
		return "OUTPUT RELAY OPEN (SHOULD BE CLOSED)"
	case 0x03:
		return "DRIVE POWER WRONG TIME"
	case 0x04:
		return "REFLECTED POWER WARNING"
	case 0x05:
		return "EXCESSIVE REFLECTED POWER"
	case 0x06:
		return "DRIVE POWER TOO HIGH"
	case 0x07:
		return "EXCESSIVE DRIVE POWER"
	case 0x08:
		return "HOT SWITCHING ATTEMPT (2)"
	case 0x09:
		return "DRIVE FREQUENCY OUT OF RANGE"
	case 0x0A:
		return "FREQUENCY VIOLATION"
	case 0x0B:
		return "OUTPUT DISBALANCE"
	case 0x0C:
		return "DETECTED RF POWER WRONG TIME"
	case 0x0D:
		return "PA LOAD SWR TOO HIGH"
	case 0x0E:
		return "STOP TRANSMISSION FIRST"
	case 0x0F:
		return "REMOVE DRIVE POWER IMMEDIATELY"
	case 0x10:
		return "5V TOO LOW"
	case 0x11:
		return "5V TOO HIGH"
	case 0x12:
		return "26V TOO LOW"
	case 0x13:
		return "26V TOO HIGH"
	case 0x14:
		return "ERROR 0x14"
	case 0x15:
		return "PAM1 FAN SPEED TOO LOW"
	case 0x16:
		return "PAM2 FAN SPEED TOO LOW"
	case 0x17:
		return "LPF FAN SPEED TOO LOW"
	case 0x18:
		return "PAM1 DISSIPATION TOO HIGH"
	case 0x19:
		return "PAM2 DISSIPATION TOO HIGH"
	case 0x1A:
		return "PAM1 DISSIPATION WARNING"
	case 0x1B:
		return "PAM2 DISSIPATION WARNING"
	case 0x1C:
		return "PAM1 TEMP TOO HIGH"
	case 0x1D:
		return "PAM2 TEMP TOO HIGH"
	case 0x1E:
		return "PAM1 EXCESSIVE TEMP"
	case 0x1F:
		return "PAM2 EXCESSIVE TEMP"
	case 0x20:
		return "PAM1 HV TOO LOW"
	case 0x21:
		return "PAM1 HV TOO HIGH"
	case 0x22:
		return "PAM1 CURRENT NON-ZERO"
	case 0x23:
		return "PAM1 IDLE CURRENT TOO LOW"
	case 0x24:
		return "PAM1 CURRENT WARNING"
	case 0x25:
		return "PAM1 EXCESSIVE CURRENT"
	case 0x26:
		return "BIAS_1A VOLTAGE ERROR"
	case 0x27:
		return "BIAS_1B VOLTAGE ERROR"
	case 0x28:
		return "BIAS_1C VOLTAGE ERROR"
	case 0x29:
		return "BIAS_1D VOLTAGE ERROR"
	case 0x2A:
		return "BIAS_1A SHOULD BE ZERO"
	case 0x2B:
		return "BIAS_1B SHOULD BE ZERO"
	case 0x2C:
		return "BIAS_1C SHOULD BE ZERO"
	case 0x2D:
		return "BIAS_1D SHOULD BE ZERO"
	case 0x2E:
		return "PAM1 GAIN TOO LOW"
	case 0x2F:
		return "PAM1 GAIN TOO HIGH"
	case 0x30:
		return "PAM1 HV SHOULD BE ZERO"
	case 0x31:
		return "PAM1 CURRENT SHOULD BE ZERO"
	case 0x32:
		return "PAM1 EXCESSIVE TEMP (3)"
	case 0x33:
		return "PAM1 TEMP TOO HIGH (3)"
	case 0x34:
		return "BIAS_1A SHOULD BE ZERO (3)"
	case 0x35:
		return "BIAS_1B SHOULD BE ZERO (3)"
	case 0x36:
		return "BIAS_1C SHOULD BE ZERO (3)"
	case 0x37:
		return "BIAS_1D SHOULD BE ZERO (3)"
	case 0x38:
		return "PSU1 EXCESSIVE TEMP"
	case 0x39:
		return "PAM1 EXCESSIVE CURRENT (CHECK SWR)"
	case 0x40:
		return "PAM2 HV TOO LOW"
	case 0x41:
		return "PAM2 HV TOO HIGH"
	case 0x42:
		return "PAM2 CURRENT NON-ZERO"
	case 0x43:
		return "PAM2 IDLE CURRENT TOO LOW"
	case 0x44:
		return "PAM2 CURRENT WARNING"
	case 0x45:
		return "PAM2 EXCESSIVE CURRENT"
	case 0x46:
		return "BIAS_2A VOLTAGE ERROR"
	case 0x47:
		return "BIAS_2B VOLTAGE ERROR"
	case 0x48:
		return "BIAS_2C VOLTAGE ERROR"
	case 0x49:
		return "BIAS_2D VOLTAGE ERROR"
	case 0x4A:
		return "BIAS_2A SHOULD BE ZERO"
	case 0x4B:
		return "BIAS_2B SHOULD BE ZERO"
	case 0x4C:
		return "BIAS_2C SHOULD BE ZERO"
	case 0x4D:
		return "BIAS_2D SHOULD BE ZERO"
	case 0x4E:
		return "PAM2 GAIN TOO LOW"
	case 0x4F:
		return "PAM2 GAIN TOO HIGH"
	case 0x60:
		return "PSU1 CONTROL MALFUNCTION"
	case 0x61:
		return "PSU2 CONTROL MALFUNCTION"
	case 0x62:
		return "PSU1 EXCESSIVE TEMP"
	case 0x63:
		return "PSU2 EXCESSIVE TEMP"
	case 0x64:
		return "DISPLAY COMM ERROR"
	case 0x65:
		return "ATU MODEM TEMP"
	case 0x66:
		return "ATU POWER SWITCH ALARM"
	case 0x67:
		return "ATU POWER SWITCH ALARM (ON)"
	case 0x68:
		return "ETHERNET NOT RESPONDING"
	case 0x69:
		return "AUDIO MEMORY ERROR"
	case 0x6C:
		return "LOSS OF AUDIO DATA"
	case 0x6D:
		return "LOSS OF ETHERNET DATA"
	case 0x6E:
		return "LOSS OF EEPROM DATA (WARN)"
	case 0x6F:
		return "LOSS OF EEPROM DATA (SOFT)"
	case 0x70:
		return "CAT ERROR"
	case 0x80:
		return "ATU NOT RESPONDING / BIAS 1A ERR"
	case 0x81:
		return "ATU-AMP COMM ERROR"
	case 0x82:
		return "AMP-ATU COMM ERROR"
	case 0x83:
		return "ASEL NOT RESPONDING"
	case 0x84:
		return "ASEL-AMP COMM ERROR"
	case 0x85:
		return "AMP-ASEL COMM ERROR"
	case 0x86:
		return "NO TUNING SETTINGS"
	case 0x87:
		return "NO ANTENNA SETTINGS"
	case 0x88:
		return "ATU CANNOT RETUNE (RF PRESENT)"
	case 0x89:
		return "ANTENNA CANNOT CHANGE (RF PRESENT)"
	case 0x8A:
		return "ATU TUNING UNSUCCESSFUL"
	case 0x8B:
		return "ATU MEMORY FAIL"
	case 0xA0:
		return "ATU DC VOLT TOO HIGH"
	case 0xA1:
		return "ATU DC VOLT TOO LOW"
	case 0xA2:
		return "ATU 5V TOO LOW"
	case 0xA3:
		return "ATU 5V TOO HIGH"
	case 0xA4:
		return "ANTENNA VOLT TOO HIGH (PWR)"
	case 0xA5:
		return "ANTENNA VOLT TOO HIGH (dmg)"
	case 0xA6:
		return "ANTENNA CURRENT TOO HIGH (PWR)"
	case 0xA7:
		return "ANTENNA CURRENT TOO HIGH (dmg)"
	case 0xA8:
		return "ANT REFL PWR TOO HIGH (SOFT)"
	case 0xA9:
		return "ANT REFL PWR TOO HIGH (HARD)"
	case 0xAA:
		return "ATU INPUT PWR TOO HIGH"
	case 0xAB:
		return "ATU INPUT PWR TOO HIGH (dmg)"
	case 0xAC:
		return "ANTENNA SWR TOO HIGH"
	case 0xAD:
		return "ANTENNA SWR TOO HIGH (dmg)"
	case 0xAE:
		return "ATU TEMP TOO HIGH"
	case 0xAF:
		return "ATU TEMP TOO LOW"
	default:
		return fmt.Sprintf("UNKNOWN ERROR (0x%02X)", b)
	}
}

// Canonical fault buckets (integration model §7.1: fault {none|swr|temp|reflected}).
// The long tail of ACOM fault codes (relays, HV, bias, fans, PSU, comms, CAT,
// EEPROM, ATU DC, ...) has no canonical home, so it collapses to "other"; the
// precise message is preserved verbatim in the /state `error` field.
var (
	faultTemp      = map[byte]bool{0x18: true, 0x19: true, 0x1A: true, 0x1B: true, 0x1C: true, 0x1D: true, 0x1E: true, 0x1F: true, 0x32: true, 0x33: true, 0x38: true, 0x62: true, 0x63: true, 0x65: true, 0xAE: true, 0xAF: true}
	faultSWR       = map[byte]bool{0x0D: true, 0x39: true, 0xAC: true, 0xAD: true}
	faultReflected = map[byte]bool{0x04: true, 0x05: true, 0xA8: true, 0xA9: true}
)

// CanonicalFault maps a fault byte to the §7.1 fault enum. 0xFF -> "none";
// temperature-family -> "temp"; SWR-family -> "swr"; reflected-family ->
// "reflected"; everything else -> "other". errMsg is accepted for symmetry
// with CanonicalMode/CanonicalKeyed but not used (the bucket is byte-driven).
func CanonicalFault(errByte byte, errMsg string) string {
	switch {
	case errByte == 0xFF:
		return "none"
	case faultTemp[errByte]:
		return "temp"
	case faultSWR[errByte]:
		return "swr"
	case faultReflected[errByte]:
		return "reflected"
	default:
		return "other"
	}
}

// CanonicalMode maps a raw firmware mode to the §7.1 PA mode enum
// {operate|standby|bypass}. Operating states (OPR/RX, OPR/TX) -> "operate";
// standby/off and all maintenance/transient states -> "standby". "bypass" is
// never produced (the ACOM protocol has no bypass state).
func CanonicalMode(raw string) string {
	switch raw {
	case "OPR/RX", "OPR/TX":
		return "operate"
	default:
		return "standby"
	}
}

// CanonicalKeyed maps a raw firmware mode to the §7.1 keyed enum
// {rx|tx|inhibited}. OPR/TX -> "tx"; OPR/RX -> "rx"; everything else (standby,
// off, maintenance) -> "inhibited" (the amp will not key).
func CanonicalKeyed(raw string) string {
	switch raw {
	case "OPR/TX":
		return "tx"
	case "OPR/RX":
		return "rx"
	default:
		return "inhibited"
	}
}

// CanonicalPower maps a raw firmware mode to the PA power enum {on|off}. The
// amplifier is only "off" when it reports the OFF state (firmware mode 0xA0,
// i.e. fully powered down); every other state — including STANDBY, where the
// amp is powered but not transmitting — is "on". This reflects *actual* power
// state from telemetry, not the desired/intended state the bridge is driving.
func CanonicalPower(raw string) string {
	if raw == "OFF" {
		return "off"
	}
	return "on"
}
