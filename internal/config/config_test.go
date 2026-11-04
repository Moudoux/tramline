package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const good = `{
  "listen": "192.168.1.121:8726",
  "serial": {"device": "/dev/ttyUSB0", "baud": 250000},
  "hue": {"address": "192.168.1.128", "bridge_id": "001788FFFE123456", "plug_light_id": "3f2b8a10-6c1d-4e7a-9b52-0d4c8e1f7a36"}
}`

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad(t *testing.T) {
	cfg, err := Load(write(t, good))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Serial.Baud != 250000 || cfg.DataDir != DefaultDataDir {
		t.Fatalf("got %+v", cfg)
	}
}

func TestLoadRejects(t *testing.T) {
	tests := []struct{ name, old, new, want string }{
		{"all interfaces", "192.168.1.121:8726", "0.0.0.0:8726", "specific address"},
		{"no host", "192.168.1.121:8726", ":8726", "IP address"},
		{"hostname", "192.168.1.121:8726", "raspberrypi:8726", "IP address"},
		{"no port", "192.168.1.121:8726", "192.168.1.121", "listen"},
		{"bad uuid", "3f2b8a10-6c1d-4e7a-9b52-0d4c8e1f7a36", "Light plug", "not a UUID"},
		{"unknown field", `"listen"`, `"listn"`, "unknown field"},
		{"no baud", "250000", "0", "baud"},
		{"bad bridge address", "192.168.1.128", "my-bridge", "hue.address"},
		{"no device", "/dev/ttyUSB0", "", "device"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(write(t, strings.Replace(good, tt.old, tt.new, 1)))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want %q", err, tt.want)
			}
		})
	}
}

func TestLoadAllowsEmptyPlugBeforePairing(t *testing.T) {
	body := strings.Replace(good, "3f2b8a10-6c1d-4e7a-9b52-0d4c8e1f7a36", "", 1)
	if _, err := Load(write(t, body)); err != nil {
		t.Fatal(err)
	}
}

func TestListenInterfaces(t *testing.T) {
	load := func(listen string) (*Config, error) {
		return Load(write(t, strings.Replace(good, `"listen": "192.168.1.121:8726"`, listen, 1)))
	}
	cfg, err := load(`"listen_interfaces": ["eth0", "tailscale0"], "port": 8726`)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ListenInterfaces) != 2 || cfg.ListenInterfaces[1] != "tailscale0" || cfg.Port != 8726 || cfg.Listen != "" {
		t.Fatalf("got %+v", cfg)
	}
	tests := []struct{ listen, want string }{
		{`"port": 8726`, "required"},
		{`"listen_interfaces": []`, "required"},
		{`"listen_interfaces": ["eth0"]`, "port must be"},
		{`"listen_interfaces": ["eth0"], "port": 70000`, "port must be"},
		{`"listen_interfaces": [""], "port": 8726`, "can't be empty"},
		{`"listen_interfaces": ["eth0","eth0"], "port": 8726`, "listed twice"},
		{`"listen": "192.168.1.121:8726", "port": 8726`, "can't be used with"},
		{`"listen": "192.168.1.121:8726", "listen_interfaces": ["eth0"]`, "can't be used with"},
		{`"also_listen_interfaces": ["eth0"], "port": 8726`, "unknown field"},
	}
	for _, tt := range tests {
		if _, err := load(tt.listen); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: got %v, want %q", tt.listen, err, tt.want)
		}
	}
}

func TestTramSoakSeconds(t *testing.T) {
	base := `{"listen":"192.168.1.121:8726","serial":{"device":"/dev/x","baud":250000}`
	load := func(extra string) (*Config, error) {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(base+extra+"}"), 0o600); err != nil {
			t.Fatal(err)
		}
		return Load(path)
	}
	if cfg, err := load(""); err != nil || cfg.TramSoakSeconds != 0 {
		t.Errorf("default: %+v, %v", cfg, err)
	}
	if cfg, err := load(`,"tram_soak_seconds":300`); err != nil || cfg.TramSoakSeconds != 300 {
		t.Errorf("300: %+v, %v", cfg, err)
	}
	for _, bad := range []string{"-1", "1201"} {
		if _, err := load(`,"tram_soak_seconds":` + bad); err == nil {
			t.Errorf("%s should be refused", bad)
		}
	}
}
