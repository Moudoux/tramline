// Package notify sends notifications to a Switchboard server, which passes each one on to
// wherever its owner has set up: Discord, a phone, webhooks. See notes/notifications.md.
//
// The settings are kept in the server's data folder, the address and which events send in
// notify.json, and the token in its own file, readable by the server only, like hue.key.
// Nothing here ever gives the token back: the API only says whether one is saved.
//
// Sending never holds up the work it reports on. Notifications go into a queue, and one
// goroutine sends them, retrying a failure that might pass, as Switchboard's guide says.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The events that can send a notification.
const (
	PrintFinished  = "print_finished"
	PrintFailed    = "print_failed"
	PrintCancelled = "print_cancelled"
	NeedsAdjusting = "needs_adjusting"
	ScheduleEnded  = "schedule_ended"
	Problem        = "problem"
	// Test is a notification asked for by hand. It sends whatever the events say.
	Test = "test"
)

// Event describes one kind of notification, for the CLI and the GUI to list.
type Event struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// Events is every event, in the order they're shown.
var Events = []Event{
	{PrintFinished, "Print finished", "A print ran to its end."},
	{PrintFailed, "Print failed", "A print or its tram check failed, or ended where it couldn't be seen."},
	{PrintCancelled, "Print cancelled", "A print was cancelled, from here, from the LCD, or because a scheduled print's check failed at its time."},
	{NeedsAdjusting, "Bed needs adjusting", "A print's tram check failed and it's waiting for the knobs and print --continue."},
	{ScheduleEnded, "Scheduled print didn't start", "A scheduled print ended before it started: missed, the bed used since, the printer busy, or failing to power on."},
	{Problem, "Heater or power problem", "The heaters couldn't be turned off when they should be, the plug was cut for that, or the printer was left on."},
}

func knownEvent(name string) bool {
	return slices.ContainsFunc(Events, func(e Event) bool { return e.Name == name })
}

func allEvents() []string {
	out := make([]string, len(Events))
	for i, e := range Events {
		out[i] = e.Name
	}
	return out
}

const (
	settingsFile = "notify.json"
	tokenFile    = "switchboard.token"

	// MaxTokenBytes is far more than Switchboard's 67 characters, and catches a file
	// pasted by mistake.
	MaxTokenBytes = 512
	maxURLBytes   = 2048

	// Limits from Switchboard's guide, in bytes.
	maxTitle = 1000
	maxBody  = 32000

	sendTimeout = 10 * time.Second
	queueSize   = 64
)

// retryWaits is the backoff between attempts after a failure that might pass.
var retryWaits = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}

// Settings is what's saved in notify.json. Nothing in it is secret.
type Settings struct {
	// Enabled is the switch for every event. A test sends either way.
	Enabled bool `json:"enabled"`
	// URL is the Switchboard server, without a trailing slash.
	URL string `json:"url"`
	// Link goes in each notification, for a person to follow, such as the read-only page.
	Link string `json:"link,omitempty"`
	// Events is which events send.
	Events []string `json:"events"`
}

// Message is one notification.
type Message struct {
	Event    string
	Title    string
	Body     string
	Priority int
	Tags     []string
	Data     map[string]any
}

// Result is how a send went.
type Result struct {
	At    time.Time `json:"at"`
	Event string    `json:"event"`
	Title string    `json:"title"`
	OK    bool      `json:"ok"`
	// ID is Switchboard's id for it, on success.
	ID    string `json:"id,omitempty"`
	Error string `json:"error,omitempty"`
	// Dropped counts notifications that never went because the queue was full.
	Dropped int `json:"dropped,omitempty"`
}

// View is GET /v1/notify. It says whether a token is saved, never what it is.
type View struct {
	Settings
	TokenSet bool `json:"token_set"`
	// Ready means notifications go out: enabled, with an address and a token.
	Ready bool `json:"ready"`
	// Problem says why the saved settings couldn't be read, if they couldn't.
	Problem string `json:"problem,omitempty"`
	// Last is the most recent send, or attempt to send.
	Last      *Result `json:"last,omitempty"`
	Available []Event `json:"available"`
}

// Update is PUT /v1/notify. A field left out stays as it is.
type Update struct {
	Enabled *bool     `json:"enabled"`
	URL     *string   `json:"url"`
	Link    *string   `json:"link"`
	Events  *[]string `json:"events"`
	// Token replaces the saved one, and an empty one removes it.
	Token *string `json:"token"`
}

// ErrInvalid wraps a mistake in an update, which the API answers with 400.
var ErrInvalid = errors.New("invalid")

// ErrNotSet means there's no address or token to send to.
var ErrNotSet = errors.New("notifications need a Switchboard address and token first")

// Notifier keeps the settings and sends.
type Notifier struct {
	dir  string
	host string
	http *http.Client
	// Now is the clock, replaced in tests.
	Now func() time.Time
	// waits replaces retryWaits in tests.
	waits []time.Duration

	mu       sync.Mutex
	settings Settings
	token    string
	problem  string
	last     *Result
	dropped  int

	queue chan Message
}

// New loads the settings from dir. A file that can't be read is reported in the view and
// the log, and the defaults are used until the settings are saved again.
func New(dir string) *Notifier {
	host, _ := os.Hostname()
	n := &Notifier{
		dir:  dir,
		host: host,
		http: &http.Client{
			Timeout: sendTimeout,
			// A redirect could carry the token somewhere else, and Switchboard never sends one.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		settings: Settings{Events: allEvents()},
		queue:    make(chan Message, queueSize),
	}
	if err := n.load(); err != nil {
		n.problem = err.Error()
		log.Printf("notify: %v", err)
	}
	return n
}

func (n *Notifier) now() time.Time {
	if n.Now != nil {
		return n.Now()
	}
	return time.Now()
}

func (n *Notifier) load() error {
	data, err := os.ReadFile(filepath.Join(n.dir, settingsFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("couldn't read %s: %w", settingsFile, err)
	default:
		var s Settings
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s); err != nil {
			return fmt.Errorf("couldn't read %s, so notifications are off until they're saved again: %w", settingsFile, err)
		}
		s.Events = slices.DeleteFunc(s.Events, func(e string) bool { return !knownEvent(e) })
		n.settings = s
	}
	token, err := os.ReadFile(filepath.Join(n.dir, tokenFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("couldn't read the token file: %w", err)
	default:
		n.token = strings.TrimSpace(string(token))
	}
	return nil
}

// View says what's set, without the token.
func (n *Notifier) View() View {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.viewLocked()
}

func (n *Notifier) viewLocked() View {
	s := n.settings
	s.Events = slices.Clone(s.Events)
	v := View{
		Settings:  s,
		TokenSet:  n.token != "",
		Ready:     s.Enabled && s.URL != "" && n.token != "",
		Problem:   n.problem,
		Available: Events,
	}
	if n.last != nil {
		last := *n.last
		last.Dropped = n.dropped
		v.Last = &last
	}
	return v
}

// Apply checks and saves an update.
func (n *Notifier) Apply(u Update) (View, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	s := n.settings
	s.Events = slices.Clone(s.Events)
	if u.URL != nil {
		clean, err := CheckURL(*u.URL, "url")
		if err != nil {
			return View{}, err
		}
		s.URL = strings.TrimRight(clean, "/")
	}
	if u.Link != nil {
		clean, err := CheckURL(*u.Link, "link")
		if err != nil {
			return View{}, err
		}
		s.Link = clean
	}
	if u.Events != nil {
		events := []string{}
		for _, e := range *u.Events {
			if !knownEvent(e) {
				return View{}, fmt.Errorf("%w: events: %q isn't one, they are %s", ErrInvalid, e, strings.Join(allEvents(), ", "))
			}
			if !slices.Contains(events, e) {
				events = append(events, e)
			}
		}
		s.Events = events
	}
	if u.Enabled != nil {
		s.Enabled = *u.Enabled
	}
	token := n.token
	if u.Token != nil {
		token = strings.TrimSpace(*u.Token)
		if err := CheckToken(token); token != "" && err != nil {
			return View{}, err
		}
	}
	if s.URL == "" || token == "" {
		// Removing the address or the token switches them off, unless this asks for them on.
		if u.Enabled != nil && *u.Enabled {
			return View{}, fmt.Errorf("%w: %v", ErrInvalid, ErrNotSet)
		}
		s.Enabled = false
	}
	if err := n.saveLocked(s, token); err != nil {
		return View{}, err
	}
	n.settings, n.token, n.problem = s, token, ""
	return n.viewLocked(), nil
}

// CheckURL accepts an http or https address with nothing in it that could go wrong:
// no login, query or fragment. Empty is allowed, and clears it.
func CheckURL(raw, what string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if len(raw) > maxURLBytes {
		return "", fmt.Errorf("%w: %s is too long", ErrInvalid, what)
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return "", fmt.Errorf("%w: %s: %v", ErrInvalid, what, err)
	case u.Scheme != "http" && u.Scheme != "https":
		return "", fmt.Errorf("%w: %s must start with http:// or https://", ErrInvalid, what)
	case u.Host == "":
		return "", fmt.Errorf("%w: %s has no host", ErrInvalid, what)
	case u.User != nil:
		return "", fmt.Errorf("%w: %s can't hold a user name or password", ErrInvalid, what)
	case what == "url" && (u.RawQuery != "" || u.Fragment != "" || u.ForceQuery):
		return "", fmt.Errorf("%w: url is the server's address only, such as https://notify.example.lan", ErrInvalid)
	}
	return raw, nil
}

// CheckToken accepts printable ASCII with no spaces. The token's own format is
// Switchboard's business, and may change.
func CheckToken(token string) error {
	if token == "" {
		return fmt.Errorf("%w: the token is empty", ErrInvalid)
	}
	if len(token) > MaxTokenBytes {
		return fmt.Errorf("%w: the token is over %d bytes, which doesn't look like a Switchboard token", ErrInvalid, MaxTokenBytes)
	}
	for _, c := range []byte(token) {
		if c <= ' ' || c > '~' {
			return fmt.Errorf("%w: the token can only hold printable ASCII, with no spaces", ErrInvalid)
		}
	}
	return nil
}

func (n *Notifier) saveLocked(s Settings, token string) error {
	if n.dir == "" {
		return errors.New("the server has no data folder to keep the settings in")
	}
	if token != n.token {
		path := filepath.Join(n.dir, tokenFile)
		if token == "" {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		} else if err := writeFile(path, []byte(token+"\n"), 0o600); err != nil {
			return fmt.Errorf("couldn't save the token: %w", err)
		}
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFile(filepath.Join(n.dir, settingsFile), append(data, '\n'), 0o640); err != nil {
		return fmt.Errorf("couldn't save the settings: %w", err)
	}
	return nil
}

// writeFile replaces a file in one step, with its mode set before it has any content.
func writeFile(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	err = tmp.Chmod(mode)
	if err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Wants says whether an event would send now.
func (n *Notifier) Wants(event string) bool {
	if n == nil {
		return false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.settings.Enabled && n.settings.URL != "" && n.token != "" && slices.Contains(n.settings.Events, event)
}

// Send queues a notification if its event is on. It never waits: with the queue full,
// the notification is dropped and counted.
func (n *Notifier) Send(m Message) {
	if !n.Wants(m.Event) {
		return
	}
	select {
	case n.queue <- m:
	default:
		n.mu.Lock()
		n.dropped++
		n.mu.Unlock()
		log.Printf("notify: the queue is full, so %q wasn't sent", m.Title)
	}
}

// Run sends what's queued until ctx ends.
func (n *Notifier) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-n.queue:
			// Turned off or emptied since it was queued.
			if !n.Wants(m.Event) {
				continue
			}
			n.deliver(ctx, m, true)
		}
	}
}

// Test sends a notification now and says how it went. It sends even when notifications
// are switched off, so the address and token can be tried first.
func (n *Notifier) Test(ctx context.Context) (string, error) {
	m := Message{
		Event:    Test,
		Title:    "Tramline test",
		Body:     "Notifications from Tramline reach you.",
		Priority: 2,
		Tags:     []string{"tramline", "test"},
	}
	return n.deliver(ctx, m, false)
}

// permanent is an answer that a retry wouldn't change.
type permanent struct{ error }

func (n *Notifier) deliver(ctx context.Context, m Message, retry bool) (string, error) {
	n.mu.Lock()
	base, token, link := n.settings.URL, n.token, n.settings.Link
	n.mu.Unlock()
	if base == "" || token == "" {
		return "", ErrNotSet
	}
	body, err := n.payload(m, link)
	if err != nil {
		return "", err
	}
	waits := retryWaits
	if n.waits != nil {
		waits = n.waits
	}
	var id string
	for attempt := 0; ; attempt++ {
		id, err = n.post(ctx, base, token, body)
		var perm permanent
		if err == nil || !retry || errors.As(err, &perm) || attempt >= len(waits) || ctx.Err() != nil {
			break
		}
		log.Printf("notify: %q: %v, trying again in %s", m.Title, err, waits[attempt])
		select {
		case <-time.After(waits[attempt]):
		case <-ctx.Done():
		}
	}
	res := &Result{At: n.now(), Event: m.Event, Title: m.Title, OK: err == nil, ID: id}
	if err != nil {
		res.Error = err.Error()
		log.Printf("notify: %q not sent: %v", m.Title, err)
	}
	n.mu.Lock()
	n.last = res
	n.mu.Unlock()
	return id, err
}

func (n *Notifier) payload(m Message, link string) ([]byte, error) {
	data := map[string]any{}
	for k, v := range m.Data {
		data[k] = v
	}
	if n.host != "" {
		data["host"] = n.host
	}
	data["event"] = m.Event
	out := struct {
		Title    string         `json:"title,omitempty"`
		Body     string         `json:"body,omitempty"`
		Priority int            `json:"priority"`
		Tags     []string       `json:"tags,omitempty"`
		URL      string         `json:"url,omitempty"`
		Data     map[string]any `json:"data"`
	}{
		Title:    cut(m.Title, maxTitle),
		Body:     cut(m.Body, maxBody),
		Priority: 3,
		Tags:     m.Tags,
		URL:      link,
		Data:     data,
	}
	if m.Priority != 0 {
		out.Priority = min(max(m.Priority, 1), 5)
	}
	return json.Marshal(out)
}

// cut shortens text to at most limit bytes, on a character boundary.
func cut(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	end := limit - len("…")
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + "…"
}

func (n *Notifier) post(ctx context.Context, base, token string, body []byte) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/notify", bytes.NewReader(body))
	if err != nil {
		return "", permanent{err}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "tramline")
	resp, err := n.http.Do(req)
	if err != nil {
		// The error names the address but never the headers, so not the token.
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	var answer struct {
		ID    string `json:"id"`
		Error string `json:"error"`
	}
	json.Unmarshal(raw, &answer)
	switch {
	case resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated:
		return answer.ID, nil
	case resp.StatusCode == http.StatusUnauthorized:
		return "", permanent{errors.New("Switchboard didn't accept the token (401): it may be wrong, or its source removed")}
	case resp.StatusCode >= 500:
		return "", fmt.Errorf("Switchboard answered %s%s", resp.Status, detail(answer.Error))
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return "", permanent{fmt.Errorf("Switchboard answered %s, a redirect, which isn't followed: check the address", resp.Status)}
	}
	return "", permanent{fmt.Errorf("Switchboard answered %s%s", resp.Status, detail(answer.Error))}
}

func detail(msg string) string {
	if msg == "" {
		return ""
	}
	return ": " + cut(msg, 300)
}
