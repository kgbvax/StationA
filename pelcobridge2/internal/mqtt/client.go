// Package mqtt is pelcobridge2's MQTT slot (muehle/uhf/rotator) following the
// station integration model: four planes (/meta /state /status /cmd), a single
// retained JSON /state snapshot, and a stop-only /cmd.
//
// The paho foot-guns are respected via shared/mqtt: Connect is ctx-aware (paho's
// Wait() ignores ctx), and the /cmd handler only enqueues — the single RunJobs
// worker publishes, never a paho dispatch goroutine (hadiscovery deadlocked
// live on exactly that).
//
// Motion over MQTT does not exist: /cmd accepts ONLY {"action":"stop"} and no
// code path from here to ArmIntent exists. Arming stays a TUI act.
package mqtt

import (
	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	schema "codeberg.org/kgbvax/stationa/shared/schema"
)

// Config is the MQTT slice of the application config. Password comes from the
// environment (PELCOBRIDGE2_MQTT_PASSWORD), never a flag.
type Config struct {
	Enabled  bool
	Broker   string // e.g. tcp://192.168.1.50:1883
	ClientID string // defaults to <site>-<station>-<slot>
	User     string
	Password string
	Site     string // e.g. "muehle"
	Station  string // e.g. "uhf"
	Slot     string // e.g. "rotator"

	// /meta birth certificate identity.
	DeviceModel string // e.g. "PTS-303Z/3050DZ"
	DeviceName  string // e.g. "UHF Rotator"
	DeviceLink  string // e.g. "rs485"
	Host        string // compute node, e.g. "shack-pc"
}

// ClientID returns the configured or derived client id.
func (c Config) ClientIDOrDefault() string {
	if c.ClientID != "" {
		return c.ClientID
	}
	return c.Site + "-" + c.Station + "-" + c.Slot
}

// Topics built once from the slot address.
func (c Config) MetaTopic() string   { return schema.MetaTopic(c.Site, c.Station, c.Slot) }
func (c Config) StateTopic() string  { return schema.StateTopic(c.Site, c.Station, c.Slot) }
func (c Config) StatusTopic() string { return schema.StatusTopic(c.Site, c.Station, c.Slot) }
func (c Config) CmdTopic() string    { return schema.CmdTopic(c.Site, c.Station, c.Slot) }

// NewClient builds the paho client: LWT offline on /status, auto-reconnect,
// retrying connect. onConnect (typically the Slot's OnConnect) publishes the
// online birth + retained /meta and subscribes /cmd on every (re)connect.
func NewClient(cfg Config, onConnect pahomqtt.OnConnectHandler) pahomqtt.Client {
	opts := pahomqtt.NewClientOptions()
	opts.AddBroker(cfg.Broker)
	opts.SetClientID(cfg.ClientIDOrDefault())
	if cfg.User != "" {
		opts.SetUsername(cfg.User)
		opts.SetPassword(cfg.Password)
	}
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(5e9)
	if onConnect != nil {
		opts.SetOnConnectHandler(onConnect)
	}
	// The bridge LWT: /status is the COMPONENT's availability, distinct from
	// /state.device_online (the HEAD's serial link).
	opts.SetWill(cfg.StatusTopic(), "offline", 1, true)
	return pahomqtt.NewClient(opts)
}
