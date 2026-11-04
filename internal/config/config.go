// Package config loads the server's configuration file.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
)

// DefaultPath is where the server looks for its config.
const DefaultPath = "/etc/tramline/config.json"

// DefaultDataDir holds everything the server generates.
const DefaultDataDir = "/var/lib/tramline"

// MaxTramSoakSeconds leaves room for heating and a check before the 30-minute heater
// timer runs out.
const MaxTramSoakSeconds = 1200

type Config struct {
	// ListenInterfaces is the network interfaces to serve on, such as eth0 and
	// tailscale0, all on Port. Their addresses are looked up and followed, so none
	// is written down here, and one that doesn't exist yet doesn't stop the server.
	ListenInterfaces []string `json:"listen_interfaces"`
	Port             int      `json:"port"`
	// Listen is one fixed address and port, instead of those two, as the simulator uses.
	Listen  string `json:"listen"`
	DataDir string `json:"data_dir"`
	Serial  Serial `json:"serial"`
	Hue     Hue    `json:"hue"`
	// TramSoakSeconds is how long a print waits at temperature before its first tram
	// check. A thin plate needs none, and glass may need several minutes.
	TramSoakSeconds int `json:"tram_soak_seconds"`
}

type Serial struct {
	Device string `json:"device"`
	Baud   int    `json:"baud"`
}

type Hue struct {
	// Address is the bridge's LAN IP. BridgeID then proves it's the right device.
	Address  string `json:"address"`
	BridgeID string `json:"bridge_id"`
	// PlugLightID is the UUID of the plug's CLIP v2 light resource. It's empty
	// until the bridge has been paired and the UUID looked up.
	PlugLightID string `json:"plug_light_id"`
}

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Load reads and checks the file. Unknown fields are errors, so a typo in a
// setting can't be silently ignored.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	cfg := &Config{DataDir: DefaultDataDir}
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// validateListen requires each place to serve on to be named, so the server can't end up
// on a Docker bridge or a new interface by accident.
func (c *Config) validateListen() error {
	if c.Listen == "" {
		if len(c.ListenInterfaces) == 0 {
			return errors.New("listen_interfaces and port are required")
		}
		if c.Port < 1 || c.Port > 65535 {
			return fmt.Errorf("port must be 1 to 65535, got %d", c.Port)
		}
		seen := map[string]bool{}
		for _, name := range c.ListenInterfaces {
			if name == "" {
				return errors.New("listen_interfaces: a name can't be empty")
			}
			if seen[name] {
				return fmt.Errorf("listen_interfaces: %s is listed twice", name)
			}
			seen[name] = true
		}
		return nil
	}
	if len(c.ListenInterfaces) != 0 || c.Port != 0 {
		return errors.New("listen can't be used with listen_interfaces or port")
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("listen: %q must be an IP address", host)
	}
	if ip.IsUnspecified() {
		return errors.New("listen: must be one specific address, not all interfaces")
	}
	return nil
}

func (c *Config) validate() error {
	if err := c.validateListen(); err != nil {
		return err
	}
	if c.TramSoakSeconds < 0 || c.TramSoakSeconds > MaxTramSoakSeconds {
		return fmt.Errorf("tram_soak_seconds must be 0 to %d, got %d", MaxTramSoakSeconds, c.TramSoakSeconds)
	}
	if c.Serial.Device == "" {
		return errors.New("serial.device is required")
	}
	if c.Serial.Baud <= 0 {
		return errors.New("serial.baud must be positive")
	}
	if c.Hue.Address != "" && net.ParseIP(c.Hue.Address) == nil {
		return fmt.Errorf("hue.address: %q must be an IP address", c.Hue.Address)
	}
	if c.Hue.PlugLightID != "" && !uuidRE.MatchString(c.Hue.PlugLightID) {
		return fmt.Errorf("hue.plug_light_id: %q is not a UUID", c.Hue.PlugLightID)
	}
	return nil
}
