package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"tramline/internal/notify"
	"tramline/internal/tram"
)

// Notifications go to Switchboard, see notes/notifications.md. Everything here only
// queues them, so it's safe to call with any lock held.

const (
	requestCancel = "cancelled on request"
	// notifyTestLimit covers one attempt with Switchboard's 10-second answer limit.
	notifyTestLimit = 15 * time.Second
)

func (s *Server) handleNotifyGet(w http.ResponseWriter, r *http.Request) {
	if s.Notify == nil {
		writeError(w, http.StatusServiceUnavailable, "the server has no data folder to keep notification settings in")
		return
	}
	writeJSON(w, http.StatusOK, s.Notify.View())
}

func (s *Server) handleNotifyPut(w http.ResponseWriter, r *http.Request) {
	if s.Notify == nil {
		writeError(w, http.StatusServiceUnavailable, "the server has no data folder to keep notification settings in")
		return
	}
	var in notify.Update
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("body must be JSON with any of enabled, url, link, events and token: %v", err))
		return
	}
	view, err := s.Notify.Apply(in)
	switch {
	case errors.Is(err, notify.ErrInvalid):
		writeError(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), notify.ErrInvalid.Error()+": "))
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	if s.Notify == nil {
		writeError(w, http.StatusServiceUnavailable, "the server has no data folder to keep notification settings in")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), notifyTestLimit)
	defer cancel()
	id, err := s.Notify.Test(ctx)
	switch {
	case errors.Is(err, notify.ErrNotSet):
		writeError(w, http.StatusPreconditionFailed, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusBadGateway, "the test wasn't sent: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}

func routeWords(job *PrintJob) string {
	switch {
	case job.Elsewhere:
		return "started elsewhere"
	case job.Mode == ModeStream:
		return "from the Pi"
	}
	return "from the card"
}

func jobName(job *PrintJob) string {
	if job.Name != "" {
		return job.Name
	}
	if job.SDName != "" {
		return job.SDName
	}
	return "a print"
}

// spanWords is a duration as a person says it, to the minute.
func spanWords(d time.Duration) string {
	d = d.Round(time.Minute)
	h, m := int(d.Hours()), int(d.Minutes())%60
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%d h %d min", h, m)
	case h > 0:
		return fmt.Sprintf("%d h", h)
	case m > 0:
		return fmt.Sprintf("%d min", m)
	}
	return "under a minute"
}

func endSentence(text string) string {
	text = strings.TrimSpace(text)
	if text == "" || strings.HasSuffix(text, ".") {
		return text
	}
	return text + "."
}

func jobData(job *PrintJob) map[string]any {
	data := map[string]any{
		"job_id": job.ID,
		"file":   jobName(job),
		"state":  job.State,
	}
	if !job.Elsewhere {
		from := FromSD
		if job.Mode == ModeStream {
			from = FromPi
		}
		data["from"] = from
	}
	if job.ScheduleID != 0 {
		data["schedule_id"] = job.ScheduleID
	}
	if job.SHA256 != "" {
		data["sha256"] = job.SHA256
	}
	if job.FinishedAt != nil && !job.StartedAt.IsZero() {
		data["seconds"] = int(job.FinishedAt.Sub(job.StartedAt).Seconds())
	}
	return data
}

// notifyJobEnd is called by closeJob as a print ends. The title stays the same for the
// same outcome, so Switchboard's dedupe can tell a repeat. Changing details go in data.
func (s *Server) notifyJobEnd(job *PrintJob) {
	if s.Notify == nil {
		return
	}
	name, why := jobName(job), job.Note
	if job.Error != "" {
		why = job.Error
	}
	m := notify.Message{Priority: 3, Tags: []string{"tramline", "print", job.State}, Data: jobData(job)}
	switch job.State {
	case PrintFinished:
		m.Event, m.Title = notify.PrintFinished, "Print finished"
		m.Body = fmt.Sprintf("%s, %s.", name, routeWords(job))
		if job.FinishedAt != nil && !job.StartedAt.IsZero() {
			m.Body = fmt.Sprintf("%s, %s, in %s with heating and the tram check.", name, routeWords(job), spanWords(job.FinishedAt.Sub(job.StartedAt)))
		}
		m.Body += " The printer turns off once the nozzle has cooled. Take the part off before the next print."
	case PrintFailed, PrintUnknown:
		m.Event, m.Title, m.Priority = notify.PrintFailed, "Print failed", 4
		if job.State == PrintUnknown {
			m.Title = "Print may not have finished"
		}
		if job.PreCheck {
			m.Title = "Scheduled print's tram check failed"
		}
		m.Body = fmt.Sprintf("%s, %s: %s", name, routeWords(job), endSentence(why))
	case PrintCancelled:
		m.Event, m.Title = notify.PrintCancelled, "Print cancelled"
		if job.PreCheck {
			m.Title = "Scheduled print cancelled"
		}
		if why == requestCancel {
			m.Priority = 2
		}
		m.Body = fmt.Sprintf("%s, %s: %s", name, routeWords(job), endSentence(why))
	default:
		// A check made when scheduling that passed: the user is there, and status says so.
		return
	}
	s.Notify.Send(m)
}

// notifyAdjusting is a print waiting for the knobs.
func (s *Server) notifyAdjusting(job *PrintJob, result *tram.Result, note string) {
	if s.Notify == nil {
		return
	}
	m := notify.Message{
		Event:    notify.NeedsAdjusting,
		Title:    "Bed needs adjusting",
		Priority: 4,
		Tags:     []string{"tramline", "print", "tram"},
		Data:     jobData(job),
	}
	if result != nil {
		m.Body = fmt.Sprintf("%s: the tram check is out, range %.3f mm against a limit of %.2f mm. %s the %s corner, then run tramline print --continue, or cancel. The heaters turn off after 30 minutes.",
			jobName(job), result.Range, tram.Tolerance, capitalise(result.Action), result.Corner)
		m.Data["range_mm"] = result.Range
		m.Data["corner"] = result.Corner
		m.Data["action"] = result.Action
	} else {
		m.Body = fmt.Sprintf("%s is waiting: %s Then run tramline print --continue, or cancel.", jobName(job), endSentence(note))
	}
	s.Notify.Send(m)
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// notifyScheduleEnd is a scheduled print ending before its job could, so closeJob says
// nothing about it. One cancelled on request isn't news to anyone.
func (s *Server) notifyScheduleEnd(p *ScheduledPrint) {
	if s.Notify == nil || p.Reason == requestCancel {
		return
	}
	title := "Scheduled print didn't start"
	if p.State == SchedMissed {
		title = "Scheduled print missed"
	}
	s.Notify.Send(notify.Message{
		Event:    notify.ScheduleEnded,
		Title:    title,
		Body:     fmt.Sprintf("%s, due %s: %s", p.Name, p.At.Local().Format("Mon 2 Jan 15:04 MST"), endSentence(p.Reason)),
		Priority: 4,
		Tags:     []string{"tramline", "schedule", p.State},
		Data: map[string]any{
			"schedule_id": p.ID,
			"file":        p.Name,
			"from":        p.From,
			"state":       p.State,
			"at":          p.At.UTC().Format(time.RFC3339),
		},
	})
}

// notifyProblem is a heater or power problem. One the heaters are in needs a person now.
func (s *Server) notifyProblem(title, problem string) {
	if s.Notify == nil {
		return
	}
	priority := 4
	if strings.Contains(problem, "heaters") {
		priority = 5
	}
	s.Notify.Send(notify.Message{
		Event:    notify.Problem,
		Title:    title,
		Body:     endSentence(capitalise(problem)),
		Priority: priority,
		Tags:     []string{"tramline", "problem"},
	})
}
