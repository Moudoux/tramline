package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"tramline/internal/tram"
)

const (
	// A scheduled print goes checking, waiting, starting, printing, then one of the
	// ends. PrintFinished, PrintCancelled, PrintFailed and PrintUnknown are also ends.
	SchedChecking = "checking"
	SchedWaiting  = "waiting"
	SchedStarting = "starting"
	SchedPrinting = "printing"
	SchedMissed   = "missed"

	scheduleFile = "schedules.json"

	// ScheduleMinLead leaves time to heat, check and cool before the print is due.
	ScheduleMinLead = 15 * time.Minute
	// ScheduleMaxLead catches a mistyped date.
	ScheduleMaxLead = 30 * 24 * time.Hour
	// ScheduleGrace is how late a print may still start, if the server was down at its
	// time. Past that the user may be expecting the printer to be free.
	ScheduleGrace = 15 * time.Minute

	schedKeepEnded = 20
	schedEvery     = 5 * time.Second
	// The real printer takes 9 to 18 seconds to connect after power on.
	schedConnectLimit = 90 * time.Second
	// status shows the printer connected for about 3 seconds after its power goes.
	schedSettle   = 5 * time.Second
	schedPlugTry  = 3
	schedPlugWait = 10 * time.Second
	// How long a nozzle may take to cool before the plug is left on. It only takes minutes.
	autoOffLimit = 30 * time.Minute
)

// ScheduleRequest is the body of POST /v1/schedule. At is an absolute time, so it means
// the same whichever timezone the client and the server are in.
type ScheduleRequest struct {
	Name string `json:"name"`
	// From is where the file prints from: FromPi or FromSD.
	From     string    `json:"from"`
	At       time.Time `json:"at"`
	BedClear bool      `json:"bed_clear"`
}

// ScheduledPrint is one scheduled print, waiting or over.
type ScheduledPrint struct {
	ID    int       `json:"id"`
	Name  string    `json:"name"`
	From  string    `json:"from"`
	At    time.Time `json:"at"`
	State string    `json:"state"`
	// JobID is the print job now doing its work: the tram check when scheduling, then
	// the print itself.
	JobID int `json:"job_id,omitempty"`
	// Check is the check when scheduling, and FinalCheck the one at the scheduled time.
	Check      *tram.Result `json:"check,omitempty"`
	FinalCheck *tram.Result `json:"final_check,omitempty"`
	Reason     string       `json:"reason,omitempty"`
	// BedClearAt is when the user said the bed was clear. A print after this may have
	// left a part on it.
	BedClearAt time.Time  `json:"bed_clear_at"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
}

// Pending means it hasn't started printing or ended, so it still needs its file.
func (p *ScheduledPrint) pending() bool {
	return p.State == SchedChecking || p.State == SchedWaiting || p.State == SchedStarting
}

func (p *ScheduledPrint) ended() bool {
	return !p.pending() && p.State != SchedPrinting
}

// scheduleData is what's saved.
type scheduleData struct {
	NextID int               `json:"next_id"`
	Prints []*ScheduledPrint `json:"prints"`
	// BedUsedAt is when a print last started, so what's on the bed is no longer known.
	BedUsedAt time.Time `json:"bed_used_at,omitzero"`
	// OffPending means the plug is to be cut once the nozzle is cool, after any print,
	// and a restart should carry on with that.
	OffPending bool `json:"off_pending,omitempty"`
}

type scheduleState struct {
	// mu is taken inside print.mu, never the other way round, and isn't held across
	// anything that talks to the printer.
	mu   sync.Mutex
	data scheduleData
	// broken says why the saved prints couldn't be read.
	broken string
	// firing means one is powering on, so another can't start alongside it.
	firing bool
	// offRun identifies the auto-off that may still cut the plug. Anything that wants
	// the printer on moves it on.
	offRun int
	// These are the constants unless a test changes them.
	every, settle, connectLimit, plugWait, offEvery time.Duration
}

func orDefault(d, def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return d
}

// saveSchedulesLocked is called with sched.mu held.
func (s *Server) saveSchedulesLocked() error {
	if s.DataDir == "" {
		return errors.New("the server has no data folder to keep scheduled prints in")
	}
	data, err := json.MarshalIndent(&s.sched.data, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.DataDir, scheduleFile), append(data, '\n'))
}

// keepSchedulesLocked saves after a change that's already been made. Failing to only
// matters after a restart, so it mustn't stop anything.
func (s *Server) keepSchedulesLocked() {
	if err := s.saveSchedulesLocked(); err != nil {
		log.Printf("schedule: couldn't save: %v", err)
	}
}

// loadSchedules reads the saved prints, once at startup. Start this after loadPrintJob:
// what happened to any job in progress is worked out from it, in checkSchedules.
func (s *Server) loadSchedules() {
	if s.DataDir == "" {
		return
	}
	raw, err := os.ReadFile(filepath.Join(s.DataDir, scheduleFile))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var data scheduleData
	if err == nil {
		err = json.Unmarshal(raw, &data)
	}
	if err != nil {
		// Carrying on with none would mean they silently don't run, and the next save
		// would overwrite them, so scheduling refuses until this is looked at.
		log.Printf("schedule: couldn't read the saved prints, so none will run: %v", err)
		s.sched.mu.Lock()
		s.sched.broken = fmt.Sprintf("%s can't be read, so scheduled prints won't run: %v", scheduleFile, err)
		s.sched.mu.Unlock()
		return
	}
	s.sched.mu.Lock()
	s.sched.data = data
	for _, p := range data.Prints {
		// It hadn't got as far as a job, so nothing has happened that a retry would upset.
		if p.State == SchedStarting && p.JobID == 0 {
			p.State = SchedWaiting
		}
	}
	off := data.OffPending
	s.sched.mu.Unlock()
	// loadPrintJob has just failed a stream the restart cut off.
	if off || s.strayPending() {
		s.startAutoOff()
	}
}

func (s *Server) schedulesCopy() []ScheduledPrint {
	s.sched.mu.Lock()
	defer s.sched.mu.Unlock()
	out := make([]ScheduledPrint, 0, len(s.sched.data.Prints))
	for _, p := range s.sched.data.Prints {
		out = append(out, *p)
	}
	slices.SortStableFunc(out, func(a, b ScheduledPrint) int { return a.At.Compare(b.At) })
	return out
}

// pendingSchedules is what status shows.
func (s *Server) pendingSchedules() []ScheduledPrint {
	var out []ScheduledPrint
	for _, p := range s.schedulesCopy() {
		if !p.ended() {
			out = append(out, p)
		}
	}
	return out
}

// scheduleUsing says which scheduled print still needs a file, as changing the file
// would print something other than what was asked for.
func (s *Server) scheduleUsing(name string) *ScheduledPrint {
	s.sched.mu.Lock()
	defer s.sched.mu.Unlock()
	for _, p := range s.sched.data.Prints {
		if p.Name == name && p.pending() {
			out := *p
			return &out
		}
	}
	return nil
}

// noteBedUsed is called whenever a file starts, from here or the LCD.
func (s *Server) noteBedUsed() {
	s.sched.mu.Lock()
	defer s.sched.mu.Unlock()
	s.sched.data.BedUsedAt = s.now()
	s.keepSchedulesLocked()
}

func (s *Server) findScheduleLocked(id int) *ScheduledPrint {
	for _, p := range s.sched.data.Prints {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// endScheduleLocked records how it ended, and drops the oldest ended ones.
func (s *Server) endScheduleLocked(p *ScheduledPrint, state, why string) {
	now := s.now()
	p.State, p.Reason, p.EndedAt = state, why, &now
	log.Printf("schedule %d: %s, %s. %s", p.ID, p.Name, state, why)
	var ended []*ScheduledPrint
	for _, q := range s.sched.data.Prints {
		if q.ended() {
			ended = append(ended, q)
		}
	}
	if extra := len(ended) - schedKeepEnded; extra > 0 {
		slices.SortStableFunc(ended, func(a, b *ScheduledPrint) int { return a.EndedAt.Compare(*b.EndedAt) })
		for _, old := range ended[:extra] {
			s.sched.data.Prints = slices.DeleteFunc(s.sched.data.Prints, func(q *ScheduledPrint) bool { return q == old })
		}
	}
	s.keepSchedulesLocked()
}

func (s *Server) endSchedule(id int, state, why string) {
	s.sched.mu.Lock()
	defer s.sched.mu.Unlock()
	if p := s.findScheduleLocked(id); p != nil && !p.ended() {
		s.endScheduleLocked(p, state, why)
	}
}

// linkScheduleJob is called from beginPrep, before the job can end.
func (s *Server) linkScheduleJob(id, jobID int) {
	s.sched.mu.Lock()
	defer s.sched.mu.Unlock()
	if p := s.findScheduleLocked(id); p != nil && !p.ended() {
		p.JobID = jobID
		s.keepSchedulesLocked()
	}
}

// scheduleWanted says whether a job's scheduled print is still on. The file never
// starts if it isn't, however the cancel and the job's steps fell out.
func (s *Server) scheduleWanted(id int) bool {
	s.sched.mu.Lock()
	defer s.sched.mu.Unlock()
	p := s.findScheduleLocked(id)
	return p != nil && !p.ended()
}

// jobEndTurnsOff says whether the printer turns off when a job ends: any print started
// here, however it ended. A print from the LCD belongs to whoever started it, and after a
// check when scheduling that didn't pass, the user is at the printer.
func jobEndTurnsOff(job *PrintJob) bool {
	return !job.Elsewhere && (job.State == PrintScheduled || !job.PreCheck)
}

// applyJobEnd is told by closeJob, so with print.mu held, about any job that belongs to
// a scheduled print.
func (s *Server) applyJobEnd(job *PrintJob) {
	s.sched.mu.Lock()
	defer s.sched.mu.Unlock()
	p := s.findScheduleLocked(job.ScheduleID)
	if p == nil || p.ended() {
		return
	}
	why := job.Note
	if job.Error != "" {
		why = job.Error
	}
	if job.PreCheck {
		p.Check = job.Check
		if job.State == PrintScheduled {
			p.State, p.JobID = SchedWaiting, 0
			log.Printf("schedule %d: tram passed, %s will print at %s", p.ID, p.Name, p.At.Format(time.RFC3339))
			s.keepSchedulesLocked()
			return
		}
		if why == "" {
			why = "the tram check when scheduling didn't finish"
		}
		s.endScheduleLocked(p, job.State, why)
		return
	}
	if job.Check != nil {
		p.FinalCheck = job.Check
	}
	s.endScheduleLocked(p, job.State, why)
}

// handleSchedule checks tram now, as a normal print's preparation does, including
// waiting for tramline print --continue if the bed needs adjusting. On a pass the
// printer turns off and the print waits for its time.
func (s *Server) handleSchedule(w http.ResponseWriter, r *http.Request) {
	if why := s.scheduleBroken(); why != "" {
		writeError(w, http.StatusInternalServerError, why)
		return
	}
	var in ScheduleRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, `body must be {"name":"<file>","at":"<RFC 3339 time>","bed_clear":true}`)
		return
	}
	if err := validFileName(in.Name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := s.now()
	switch {
	case !in.BedClear:
		writeError(w, http.StatusBadRequest, "refusing to schedule: confirm the bed is clear with bed_clear")
		return
	case in.At.Before(now.Add(ScheduleMinLead)):
		writeError(w, http.StatusBadRequest, fmt.Sprintf("refusing to schedule: the time must be at least %s from now, to heat, check tram and cool first", ScheduleMinLead))
		return
	case in.At.After(now.Add(ScheduleMaxLead)):
		writeError(w, http.StatusBadRequest, fmt.Sprintf("refusing to schedule: the time must be within %d days", int(ScheduleMaxLead.Hours()/24)))
		return
	case s.Prints == nil || s.Card == nil || s.Prober == nil:
		writeError(w, http.StatusServiceUnavailable, "there's no printer to print on")
		return
	case s.Plug == nil:
		writeError(w, http.StatusServiceUnavailable, "no plug is configured, so the printer can't be turned on and off")
		return
	}
	if why := s.printBusy(); why != "" {
		writeError(w, http.StatusConflict, "refusing to schedule: "+why)
		return
	}
	if code, why := s.checkPrintable(in.Name, in.From); why != "" {
		writeError(w, code, "refusing to schedule: "+why)
		return
	}

	ctx := context.WithoutCancel(r.Context())
	s.keepPowerOn()
	if _, err := s.powerOnAndConnect(ctx, func() bool { return true }); err != nil {
		writeError(w, http.StatusConflict, "refusing to schedule: "+err.Error())
		return
	}

	// It's saved before anything starts, so it's refused if it can't be kept.
	s.sched.mu.Lock()
	s.sched.data.NextID++
	p := &ScheduledPrint{
		ID: s.sched.data.NextID, Name: in.Name, From: in.From, At: in.At.UTC(), State: SchedChecking, BedClearAt: now,
	}
	s.sched.data.Prints = append(s.sched.data.Prints, p)
	id := p.ID
	err := s.saveSchedulesLocked()
	if err != nil {
		s.sched.data.Prints = slices.DeleteFunc(s.sched.data.Prints, func(q *ScheduledPrint) bool { return q == p })
	}
	s.sched.mu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "refusing to schedule: "+err.Error())
		return
	}

	_, code, msg := s.beginPrint(ctx, in.Name, in.From, id, true)
	if msg != "" {
		s.sched.mu.Lock()
		s.sched.data.Prints = slices.DeleteFunc(s.sched.data.Prints, func(q *ScheduledPrint) bool { return q.ID == id })
		s.keepSchedulesLocked()
		s.sched.mu.Unlock()
		writeError(w, code, "refusing to schedule: "+msg)
		return
	}
	log.Printf("schedule %d: %s at %s, checking tram", id, in.Name, in.At.UTC().Format(time.RFC3339))
	s.sched.mu.Lock()
	out := *p
	s.sched.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) scheduleBroken() string {
	s.sched.mu.Lock()
	defer s.sched.mu.Unlock()
	return s.sched.broken
}

func (s *Server) handleScheduleList(w http.ResponseWriter, r *http.Request) {
	if why := s.scheduleBroken(); why != "" {
		writeError(w, http.StatusInternalServerError, why)
		return
	}
	writeJSON(w, http.StatusOK, s.schedulesCopy())
}

// handleScheduleCancel stops one that hasn't started printing.
func (s *Server) handleScheduleCancel(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "the id must be a number, see tramline schedule list")
		return
	}
	s.sched.mu.Lock()
	p := s.findScheduleLocked(id)
	if p == nil {
		s.sched.mu.Unlock()
		writeError(w, http.StatusNotFound, fmt.Sprintf("no scheduled print %d", id))
		return
	}
	state, jobID := p.State, p.JobID
	switch {
	case p.ended():
		s.sched.mu.Unlock()
		writeError(w, http.StatusConflict, fmt.Sprintf("scheduled print %d has already ended: %s", id, state))
		return
	case state == SchedPrinting:
		s.sched.mu.Unlock()
		writeError(w, http.StatusConflict, fmt.Sprintf("scheduled print %d is printing, tramline cancel stops it", id))
		return
	case jobID == 0:
		// Waiting, or powering on, which notices and stops. Nothing has heated or moved.
		s.endScheduleLocked(p, PrintCancelled, "cancelled on request")
		out := *p
		s.sched.mu.Unlock()
		writeJSON(w, http.StatusOK, out)
		return
	}
	s.sched.mu.Unlock()

	// A job is working on it, and ending the job ends this too.
	if job := s.printJob(); job.active() && job.ID == jobID {
		if err := s.cancelPrint("cancelled on request"); err != nil {
			writePrinterError(w, err)
			return
		}
	}
	s.endSchedule(id, PrintCancelled, "cancelled on request")
	s.sched.mu.Lock()
	var out ScheduledPrint
	if p := s.findScheduleLocked(id); p != nil {
		out = *p
	}
	s.sched.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

// watchSchedules goes by the wall clock every few seconds rather than a timer set for the
// time, as the Pi has no battery-backed clock and its time can jump after a boot.
func (s *Server) watchSchedules(ctx context.Context) {
	tick := time.NewTicker(orDefault(s.sched.every, schedEvery))
	defer tick.Stop()
	for {
		s.checkSchedules(ctx)
		select {
		case <-tick.C:
		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) checkSchedules(ctx context.Context) {
	s.reconcileSchedules()
	now := s.now()
	s.sched.mu.Lock()
	var due *ScheduledPrint
	var late []int
	for _, p := range s.sched.data.Prints {
		if p.State == SchedChecking && !now.Before(p.At) {
			late = append(late, p.JobID)
		}
		if p.State != SchedWaiting || now.Before(p.At) {
			continue
		}
		if now.Sub(p.At) > ScheduleGrace {
			s.endScheduleLocked(p, SchedMissed, fmt.Sprintf("the server wasn't running at its time, and it's now %s late", now.Sub(p.At).Round(time.Minute)))
			continue
		}
		if due == nil || p.At.Before(due.At) {
			due = p
		}
	}
	var fire ScheduledPrint
	if due != nil && !s.sched.firing {
		due.State = SchedStarting
		s.sched.firing = true
		s.keepSchedulesLocked()
		fire = *due
	}
	s.sched.mu.Unlock()

	for _, jobID := range late {
		if job := s.printJob(); job.active() && job.ID == jobID {
			if err := s.cancelPrint("its tram check hadn't passed by the scheduled time"); err != nil {
				log.Printf("schedule: cancelling a late check: %v", err)
			}
		}
	}
	if fire.ID != 0 {
		go func() {
			defer func() {
				s.sched.mu.Lock()
				defer s.sched.mu.Unlock()
				s.sched.firing = false
			}()
			s.fireSchedule(ctx, fire)
		}()
	}
}

// reconcileSchedules brings a schedule into line with its job. Nearly all of this happens
// as it goes, by way of closeJob, and what's left is what a restart skipped.
func (s *Server) reconcileSchedules() {
	job := s.printJob()
	s.print.mu.Lock()
	checked := s.print.linkChecked
	s.print.mu.Unlock()
	now := s.now()

	var jobEnded, restartStop bool
	s.sched.mu.Lock()
	for _, p := range s.sched.data.Prints {
		if p.ended() || p.JobID == 0 {
			continue
		}
		switch {
		case job == nil || job.ID != p.JobID:
			s.endScheduleLocked(p, PrintFailed, "the server restarted, and its job can't be found")
		case !job.active():
			jobEnded = true
		case job.PreCheck && job.running():
			s.endScheduleLocked(p, PrintCancelled, "its file was started from the LCD during the check")
		case job.running() && p.State != SchedPrinting:
			p.State = SchedPrinting
			s.keepSchedulesLocked()
		case !job.PreCheck && job.State == PrintAdjusting:
			// Only a restart leaves one of these waiting, as a scheduled print never waits
			// for the user. Nobody is there to continue it. It stays until the printer has
			// said whether its file started, so a print in progress is followed.
			restartStop = checked || now.After(p.At.Add(ScheduleGrace))
		}
	}
	s.sched.mu.Unlock()
	if jobEnded {
		s.applyJobEnd(job)
		if jobEndTurnsOff(job) {
			s.startAutoOff()
		}
	}
	if restartStop {
		if err := s.cancelPrint("the server restarted before its tram check finished"); err != nil {
			log.Printf("schedule: cancelling after a restart: %v", err)
		}
	}
}

// fireSchedule is its time arriving. It only reaches for the printer once it knows it's
// free and the bed is still as the user said, and from there it can only end in a pass
// and the file starting, or the printer off again.
func (s *Server) fireSchedule(ctx context.Context, p ScheduledPrint) {
	s.sched.mu.Lock()
	used := s.sched.data.BedUsedAt
	s.sched.mu.Unlock()
	if used.After(p.BedClearAt) {
		s.endSchedule(p.ID, PrintCancelled, fmt.Sprintf("a print started at %s, after the bed was confirmed clear, so a part may be on it", used.UTC().Format(time.RFC3339)))
		return
	}
	if why := s.printBusy(); why != "" {
		s.endSchedule(p.ID, PrintCancelled, "the printer was busy at its time: "+why)
		return
	}
	s.keepPowerOn()
	wanted := func() bool { return s.scheduleWanted(p.ID) }
	switchedOn, err := s.powerOnAndConnect(ctx, wanted)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.endSchedule(p.ID, PrintFailed, err.Error())
		if switchedOn {
			s.startAutoOff()
		}
		return
	}
	if !wanted() {
		if switchedOn {
			s.startAutoOff()
		}
		return
	}
	if _, _, msg := s.beginPrint(ctx, p.Name, p.From, p.ID, false); msg != "" {
		s.endSchedule(p.ID, PrintFailed, msg)
		if switchedOn {
			s.startAutoOff()
		}
		return
	}
	log.Printf("schedule %d: %s is due, heating for its tram check", p.ID, p.Name)
}

// powerOnAndConnect returns once the printer is on and answering, and says if it had
// to switch the plug to do that. wanted false stops the wait, with no error.
func (s *Server) powerOnAndConnect(ctx context.Context, wanted func() bool) (bool, error) {
	plug := s.plugStatus(ctx)
	switch {
	case plug == nil:
		return false, errors.New("no plug is configured")
	case plug.Error != "":
		return false, errors.New("the plug's state can't be read: " + plug.Error)
	}
	switched := false
	if !plug.On {
		var err error
		for try := range schedPlugTry {
			if try > 0 {
				select {
				case <-time.After(orDefault(s.sched.plugWait, schedPlugWait)):
				case <-ctx.Done():
					return false, ctx.Err()
				}
			}
			set, cancel := context.WithTimeout(ctx, plugTimeout)
			err = s.Plug.Set(set, true)
			cancel()
			if err == nil {
				break
			}
			log.Printf("schedule: switching the plug on: %v", err)
		}
		if err != nil {
			return false, fmt.Errorf("the plug couldn't be switched on: %w", err)
		}
		switched = true
		// What status says just after power off isn't to be trusted, so it gets a moment.
		select {
		case <-time.After(orDefault(s.sched.settle, schedSettle)):
		case <-ctx.Done():
			return true, ctx.Err()
		}
	}
	limit := orDefault(s.sched.connectLimit, schedConnectLimit)
	deadline := time.Now().Add(limit)
	for {
		if st := s.Printer(); st.Connected && st.Temps != nil {
			return switched, nil
		}
		if !wanted() {
			return switched, nil
		}
		if time.Now().After(deadline) {
			return switched, fmt.Errorf("the printer didn't connect within %s of its power being on", limit)
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return switched, ctx.Err()
		}
	}
}

// keepPowerOn stops any auto-off, as the printer is wanted.
func (s *Server) keepPowerOn() {
	s.sched.mu.Lock()
	defer s.sched.mu.Unlock()
	s.sched.offRun++
	if s.sched.data.OffPending {
		s.sched.data.OffPending = false
		s.keepSchedulesLocked()
	}
}

// startAutoOff turns the heaters off, waits for the nozzle to cool and cuts the plug.
func (s *Server) startAutoOff() {
	if s.Plug == nil {
		return
	}
	s.sched.mu.Lock()
	s.sched.offRun++
	run := s.sched.offRun
	s.sched.data.OffPending = true
	s.keepSchedulesLocked()
	s.sched.mu.Unlock()
	go s.autoOff(s.baseCtx(), run)
}

// autoOff stops at the first sign that the printer is being used. Unlike power off, it
// needs a connected printer to prove it's cool, as a link that's down says nothing about
// the nozzle. Leaving the plug on is the safe way to fail, as the fan keeps running.
func (s *Server) autoOff(ctx context.Context, run int) {
	done := func(why string) {
		log.Printf("schedule: auto-off: %s", why)
		s.sched.mu.Lock()
		defer s.sched.mu.Unlock()
		if s.sched.offRun == run {
			s.sched.data.OffPending = false
			s.keepSchedulesLocked()
		}
	}
	current := func() bool {
		s.sched.mu.Lock()
		defer s.sched.mu.Unlock()
		return s.sched.offRun == run
	}
	deadline := time.Now().Add(autoOffLimit)
	// A file without its own end leaves the heaters on, and only this first look can say
	// so: after this, a target means someone wants heat.
	s.heat.op.Lock()
	if t := s.Printer(); current() && s.Heaters != nil && t.Connected && !s.printRunning() && t.Temps != nil && (t.Temps.BedTarget > 0 || t.Temps.NozzleTarget > 0) {
		if err := s.Heaters.SetTargets(ctx, 0, 0); err != nil {
			log.Printf("schedule: auto-off: heaters: %v", err)
		} else {
			s.heatersOff()
		}
	}
	// The print's own mesh is only in the firmware until the plug is cut.
	if current() && !s.printRunning() {
		s.readPrintMesh(ctx)
	}
	s.heat.op.Unlock()

	for ctx.Err() == nil {
		if !current() {
			return
		}
		if why := s.printBusy(); why != "" {
			done("left on: " + why)
			return
		}
		if plug := s.plugStatus(ctx); plug != nil && plug.Error == "" && !plug.On {
			done("the plug is already off")
			return
		}
		if finished, why := s.tryAutoOff(ctx, current); finished {
			done(why)
			return
		}
		if time.Now().After(deadline) {
			done(fmt.Sprintf("left on: couldn't confirm the nozzle was below %.0f C within %s", CoolEnough, autoOffLimit))
			return
		}
		select {
		case <-time.After(orDefault(s.sched.offEvery, schedEvery)):
		case <-ctx.Done():
		}
	}
}

// tryAutoOff cuts the plug if that's safe now, and returns true if it's finished trying.
func (s *Server) tryAutoOff(ctx context.Context, current func() bool) (bool, string) {
	s.heat.op.Lock()
	defer s.heat.op.Unlock()
	st := s.Printer()
	switch {
	case !current():
		return false, ""
	case !st.Connected || st.Temps == nil:
		return false, ""
	case st.Temps.BedTarget > 0 || st.Temps.NozzleTarget > 0:
		return true, "left on: someone turned the heaters on"
	case st.Temps.Nozzle >= CoolEnough:
		return false, ""
	}
	if s.printRunning() || s.tramRunning() || s.copyRunning() {
		return true, "left on: the printer is in use"
	}
	set, cancel := context.WithTimeout(ctx, plugTimeout)
	defer cancel()
	if err := s.Plug.Set(set, false); err != nil {
		log.Printf("schedule: auto-off: cutting the plug: %v", err)
		return false, ""
	}
	return true, fmt.Sprintf("the nozzle is %.0f C, so the plug is off", st.Temps.Nozzle)
}
