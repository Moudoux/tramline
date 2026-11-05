// Package client talks to a Tramline server and stores the login.
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tramline/internal/hue"
	"tramline/internal/secure"
	"tramline/internal/server"
)

// Login is what the client remembers about a server. Nothing in it is secret: the
// private key is in its own file, and is only ever used through TLS.
type Login struct {
	URL         string `json:"url"`
	Fingerprint string `json:"fingerprint"`
}

// Dir is where the client keeps its login and key: ~/.config/tramline, or under
// XDG_CONFIG_HOME if that's set. It's the same on every system, as os.UserConfigDir
// ignores XDG_CONFIG_HOME on macOS, and sim-web.sh relies on it to keep off the real login.
func Dir() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	} else if !filepath.IsAbs(dir) {
		return "", errors.New("XDG_CONFIG_HOME is not an absolute path")
	}
	return filepath.Join(dir, "tramline"), nil
}

// Save writes the login, replacing any earlier one.
func Save(dir string, l Login) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".client-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(append(data, '\n'))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, "client.json"))
}

// Load reads the saved login.
func Load(dir string) (Login, error) {
	var l Login
	data, err := os.ReadFile(filepath.Join(dir, "client.json"))
	if errors.Is(err, os.ErrNotExist) {
		return l, errors.New("not logged in, run: tramline login <url> <server-fingerprint>")
	}
	if err != nil {
		return l, err
	}
	return l, json.Unmarshal(data, &l)
}

// WebURL is the address of the server's read-only web page: the same host and port as
// the API, over plain HTTP.
func (l Login) WebURL() (string, error) {
	u, err := url.Parse(l.URL)
	if err != nil {
		return "", err
	}
	if u.Scheme != "https" || u.Host == "" {
		return "", fmt.Errorf("server URL must be https://host:port, got %q", l.URL)
	}
	return "http://" + u.Host + "/", nil
}

const (
	requestTimeout = 30 * time.Second
	// coolTimeout has to outlast a move, because the printer can't act on cool
	// until the one it's running is done, which is capped at 3 minutes.
	coolTimeout = 4 * time.Minute
	// uploadTimeout is plenty for the largest file allowed over a home network.
	uploadTimeout = 30 * time.Minute
	// printTimeout covers the checks and M23, which can wait for a slow card.
	printTimeout = 2 * time.Minute
	estopTimeout = 30 * time.Second
)

// ErrNotFound means the server doesn't have what was asked for.
var ErrNotFound = errors.New("not found")

// Client sends requests authenticated by its certificate.
type Client struct {
	login Login
	http  *http.Client
}

// New checks the URL is HTTPS, and sets up TLS to trust only the saved server.
func New(l Login, cert tls.Certificate) (*Client, error) {
	u, err := url.Parse(l.URL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("server URL must be https://host:port, got %q", l.URL)
	}
	if l.Fingerprint == "" {
		return nil, errors.New("login needs the server's fingerprint")
	}
	return &Client{
		login: l,
		http: &http.Client{
			Transport:     &http.Transport{TLSClientConfig: secure.ClientTLS(l.Fingerprint, cert)},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// Status fetches the server's status.
func (c *Client) Status() (server.StatusResponse, error) {
	var out server.StatusResponse
	return out, c.do(http.MethodGet, "/v1/status", nil, &out)
}

// Power switches the printer's plug.
func (c *Client) Power(on bool) error {
	return c.do(http.MethodPost, "/v1/power", map[string]bool{"on": on}, nil)
}

// Preheat starts heating, and returns when the heaters will turn themselves off.
func (c *Client) Preheat(bed, nozzle int) (server.PreheatResponse, error) {
	var out server.PreheatResponse
	return out, c.do(http.MethodPost, "/v1/preheat", server.PreheatRequest{Bed: &bed, Nozzle: &nozzle}, &out)
}

// Cool turns both heaters off, which during a print means cancelling it.
func (c *Client) Cool() (server.CoolResponse, error) {
	var out server.CoolResponse
	return out, c.doWithin(coolTimeout, http.MethodPost, "/v1/cool", nil, &out)
}

// Print turns the printer on if needed, heats it, checks tram and starts a file once that
// passes, from server.FromPi or server.FromSD. Follow it through Status.
func (c *Client) Print(name, from string, bedClear bool) (server.PrintJob, error) {
	var out server.PrintJob
	return out, c.doWithin(printTimeout, http.MethodPost, "/v1/print", server.PrintRequest{Name: name, From: from, BedClear: bedClear}, &out)
}

// ContinuePrint says the bed has been adjusted, so the print's tram check runs again.
func (c *Client) ContinuePrint() (server.PrintJob, error) {
	var out server.PrintJob
	return out, c.doWithin(printTimeout, http.MethodPost, "/v1/print/continue", struct{}{}, &out)
}

// CancelPrint asks the firmware to stop the print. Follow it through Status.
func (c *Client) CancelPrint() error {
	return c.doWithin(coolTimeout, http.MethodPost, "/v1/cancel", nil, nil)
}

// timeLayouts are the forms a scheduled time can take. A full date is always needed, so
// what's stored can be read back and checked.
var timeLayouts = []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02T15:04:05"}

// ParseTime reads a date and time. Without an offset it's in loc, normally this
// machine's timezone, which may not be the server's.
func ParseTime(text string, loc *time.Location) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, text); err == nil {
		return t, nil
	}
	for _, layout := range timeLayouts {
		if t, err := time.ParseInLocation(layout, text, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q isn't a date and time, use YYYY-MM-DD HH:MM", text)
}

// Schedule checks tram now, and on a pass the printer turns off until the print's time.
func (c *Client) Schedule(name, from string, at time.Time, bedClear bool) (server.ScheduledPrint, error) {
	var out server.ScheduledPrint
	return out, c.doWithin(printTimeout, http.MethodPost, "/v1/schedule", server.ScheduleRequest{Name: name, From: from, At: at, BedClear: bedClear}, &out)
}

// Schedules lists the scheduled prints, soonest first, with the ones that have ended.
func (c *Client) Schedules() ([]server.ScheduledPrint, error) {
	var out []server.ScheduledPrint
	return out, c.do(http.MethodGet, "/v1/schedule", nil, &out)
}

// CancelSchedule stops a scheduled print that hasn't started printing.
func (c *Client) CancelSchedule(id int) (server.ScheduledPrint, error) {
	var out server.ScheduledPrint
	return out, c.doWithin(coolTimeout, http.MethodPost, fmt.Sprintf("/v1/schedule/%d/cancel", id), nil, &out)
}

// Estop stops the printer and cuts its power. The answer says what was done.
func (c *Client) Estop() (server.EstopResponse, error) {
	var out server.EstopResponse
	return out, c.doWithin(estopTimeout, http.MethodPost, "/v1/estop", nil, &out)
}

// Tram starts a tramming run and returns straight away. Follow it through Status.
func (c *Client) Tram(req server.TramRequest) (server.TramJob, error) {
	var out server.TramJob
	return out, c.do(http.MethodPost, "/v1/tram", req, &out)
}

// TramInfo fetches the firmware's raw answers about probing.
func (c *Client) TramInfo() ([]string, error) {
	var out []string
	return out, c.do(http.MethodGet, "/v1/tram/info", nil, &out)
}

// HuePair asks the server to pair with the bridge. The key stays on the server.
func (c *Client) HuePair() error {
	return c.do(http.MethodPost, "/v1/hue/pair", nil, nil)
}

// HuePlugs lists the lights on the bridge.
func (c *Client) HuePlugs() ([]hue.Light, error) {
	var out []hue.Light
	return out, c.do(http.MethodGet, "/v1/hue/plugs", nil, &out)
}

// File fetches what the server knows about one file, or returns ErrNotFound.
func (c *Client) File(name string) (server.FileRecord, error) {
	var out server.FileRecord
	return out, c.do(http.MethodGet, "/v1/files/"+url.PathEscape(name), nil, &out)
}

// Files lists the server's files, and the card's if the printer is free to say.
func (c *Client) Files() (server.FilesResponse, error) {
	var out server.FilesResponse
	return out, c.do(http.MethodGet, "/v1/files", nil, &out)
}

// PutFile sends a file to the server, to keep on the Pi. It stays there whatever
// happens to this client afterwards.
func (c *Client) PutFile(name string, content io.Reader, size int64, sha256 string) (server.FileRecord, error) {
	var out server.FileRecord
	ctx, cancel := context.WithTimeout(context.Background(), uploadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.url("/v1/files/"+url.PathEscape(name)), content)
	if err != nil {
		return out, err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set(server.HashHeader, sha256)
	return out, c.send(req, &out)
}

// Copy starts copying a file on the server to the SD card, and returns straight
// away. With no job in the reply, it was already there.
func (c *Client) Copy(name string, window int) (server.CopyResponse, error) {
	var out server.CopyResponse
	return out, c.do(http.MethodPost, "/v1/files/"+url.PathEscape(name)+"/copy", server.CopyRequest{Window: window}, &out)
}

// CancelCopy stops the running copy.
func (c *Client) CancelCopy() error {
	return c.do(http.MethodPost, "/v1/copy/cancel", nil, nil)
}

// DeleteFile removes a file from the server, and from the card if it went there.
func (c *Client) DeleteFile(name string) error {
	return c.do(http.MethodDelete, "/v1/files/"+url.PathEscape(name), nil, nil)
}

func (c *Client) url(path string) string {
	return strings.TrimRight(c.login.URL, "/") + path
}

func (c *Client) do(method, path string, in, out any) error {
	return c.doWithin(requestTimeout, method, path, in, out)
}

func (c *Client) doWithin(limit time.Duration, method, path string, in, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(path), body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.send(req, out)
}

func (c *Client) send(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		if strings.Contains(err.Error(), "remote error: tls") {
			return errors.New("the server refused this client, which must be allowed on the server: tramline allow <client-fingerprint>")
		}
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if resp.StatusCode == http.StatusNotFound {
			if json.Unmarshal(data, &e) == nil && e.Error != "" {
				return fmt.Errorf("%w: %s", ErrNotFound, e.Error)
			}
			return ErrNotFound
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("server returned %s", resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}
