// Package hue switches one Philips Hue plug through the bridge's local API.
package hue

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	keyFile  = "hue.key"
	certFile = "hue.cert.sha256"

	requestTimeout = 10 * time.Second
	settleTimeout  = 5 * time.Second
	settlePoll     = 250 * time.Millisecond

	// errLinkButton is the bridge's error type for "link button not pressed".
	errLinkButton = 101
)

var (
	ErrNotConfigured = errors.New("hue.address and hue.bridge_id must be set in the config")
	ErrNoPlug        = errors.New("hue.plug_light_id isn't set in the config")
	ErrNotPaired     = errors.New("not paired with the Hue Bridge, run: tramline hue pair")
	ErrAlreadyPaired = errors.New("already paired with the Hue Bridge")
	ErrLinkButton    = errors.New("the bridge's link button wasn't pressed: press it, then try again within 30 seconds")
)

// Light is one light resource. Hue plugs are exposed as lights.
type Light struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Archetype string `json:"archetype"`
	On        bool   `json:"on"`
}

// Client talks to one bridge. The application key stays inside this package and
// the data directory: it's never logged or returned.
type Client struct {
	Address  string
	BridgeID string
	PlugID   string
	DataDir  string
}

// Configured reports whether a bridge and a plug are both set.
func (c *Client) Configured() bool {
	return c.Address != "" && c.BridgeID != "" && c.PlugID != ""
}

func certFingerprint(cs tls.ConnectionState) (string, error) {
	if len(cs.PeerCertificates) == 0 {
		return "", errors.New("bridge sent no certificate")
	}
	sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
	return hex.EncodeToString(sum[:]), nil
}

func newHTTP(verify func(tls.ConnectionState) error) *http.Client {
	return &http.Client{
		Timeout: requestTimeout,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			// The bridge's certificate isn't from a public CA, so verify does the
			// checking: the bridge ID when pairing, and the saved certificate after.
			InsecureSkipVerify: true,
			VerifyConnection:   verify,
		}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (c *Client) pinned() (*http.Client, string, error) {
	if c.Address == "" || c.BridgeID == "" {
		return nil, "", ErrNotConfigured
	}
	pin, err := os.ReadFile(filepath.Join(c.DataDir, certFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", ErrNotPaired
	}
	if err != nil {
		return nil, "", err
	}
	key, err := os.ReadFile(filepath.Join(c.DataDir, keyFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", ErrNotPaired
	}
	if err != nil {
		return nil, "", err
	}
	want := strings.TrimSpace(string(pin))
	return newHTTP(func(cs tls.ConnectionState) error {
		got, err := certFingerprint(cs)
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			return fmt.Errorf("bridge certificate %s doesn't match the one saved when pairing", got)
		}
		return nil
	}), strings.TrimSpace(string(key)), nil
}

// Pair asks the bridge for an application key and saves it. The saved
// certificate then identifies this bridge from here on. It refuses to replace
// an existing pairing, so a repeated command can't break a working setup.
func (c *Client) Pair(ctx context.Context) error {
	if c.Address == "" || c.BridgeID == "" {
		return ErrNotConfigured
	}
	keyPath := filepath.Join(c.DataDir, keyFile)
	if _, err := os.Stat(keyPath); err == nil {
		return ErrAlreadyPaired
	}

	var fingerprint string
	hc := newHTTP(func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("bridge sent no certificate")
		}
		cn := cs.PeerCertificates[0].Subject.CommonName
		if !strings.EqualFold(cn, c.BridgeID) {
			return fmt.Errorf("device at %s identifies as %q, not bridge %s", c.Address, cn, c.BridgeID)
		}
		var err error
		fingerprint, err = certFingerprint(cs)
		return err
	})

	body := []byte(`{"devicetype":"tramline#server","generateclientkey":true}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+c.Address+"/api", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var reply []struct {
		Success *struct {
			Username string `json:"username"`
		} `json:"success"`
		Error *struct {
			Type        int    `json:"type"`
			Description string `json:"description"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&reply); err != nil {
		return fmt.Errorf("unexpected reply from bridge: %s", resp.Status)
	}
	if len(reply) == 0 {
		return errors.New("empty reply from bridge")
	}
	if e := reply[0].Error; e != nil {
		if e.Type == errLinkButton {
			return ErrLinkButton
		}
		return fmt.Errorf("bridge: %s", e.Description)
	}
	if reply[0].Success == nil || reply[0].Success.Username == "" {
		return errors.New("bridge didn't return a key")
	}

	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(c.DataDir, certFile), []byte(fingerprint+"\n"), 0o644); err != nil {
		return err
	}
	f, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(reply[0].Success.Username + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

type resource struct {
	ID       string `json:"id"`
	Metadata struct {
		Name      string `json:"name"`
		Archetype string `json:"archetype"`
	} `json:"metadata"`
	On struct {
		On bool `json:"on"`
	} `json:"on"`
}

func (r resource) light() Light {
	return Light{ID: r.ID, Name: r.Metadata.Name, Archetype: r.Metadata.Archetype, On: r.On.On}
}

func (c *Client) call(ctx context.Context, method, path string, in []byte) ([]resource, error) {
	hc, key, err := c.pinned()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://"+c.Address+path, bytes.NewReader(in))
	if err != nil {
		return nil, err
	}
	req.Header.Set("hue-application-key", key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var reply struct {
		Errors []struct {
			Description string `json:"description"`
		} `json:"errors"`
		Data []resource `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&reply); err != nil {
		return nil, fmt.Errorf("unexpected reply from bridge: %s", resp.Status)
	}
	if len(reply.Errors) > 0 {
		return nil, fmt.Errorf("bridge: %s", reply.Errors[0].Description)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bridge returned %s", resp.Status)
	}
	return reply.Data, nil
}

// Lights lists every light on the bridge, to find the plug's UUID.
func (c *Client) Lights(ctx context.Context) ([]Light, error) {
	data, err := c.call(ctx, http.MethodGet, "/clip/v2/resource/light", nil)
	if err != nil {
		return nil, err
	}
	lights := make([]Light, 0, len(data))
	for _, r := range data {
		lights = append(lights, r.light())
	}
	return lights, nil
}

// State reads the configured plug.
func (c *Client) State(ctx context.Context) (Light, error) {
	if c.PlugID == "" {
		return Light{}, ErrNoPlug
	}
	data, err := c.call(ctx, http.MethodGet, "/clip/v2/resource/light/"+c.PlugID, nil)
	if err != nil {
		return Light{}, err
	}
	if len(data) != 1 || data[0].ID != c.PlugID {
		return Light{}, fmt.Errorf("bridge has no light %s", c.PlugID)
	}
	return data[0].light(), nil
}

// Set switches the plug and reads it back until the bridge reports the new
// state. An accepted request alone doesn't prove the plug actually changed.
func (c *Client) Set(ctx context.Context, on bool) error {
	if c.PlugID == "" {
		return ErrNoPlug
	}
	body := fmt.Appendf(nil, `{"on":{"on":%t}}`, on)
	if _, err := c.call(ctx, http.MethodPut, "/clip/v2/resource/light/"+c.PlugID, body); err != nil {
		return err
	}
	deadline := time.Now().Add(settleTimeout)
	for {
		state, err := c.State(ctx)
		if err != nil {
			return err
		}
		if state.On == on {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("plug still reports on=%t after %s", state.On, settleTimeout)
		}
		select {
		case <-time.After(settlePoll):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
