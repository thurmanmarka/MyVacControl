package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// vacuum is a configured vacuum with its connection and clean tracker.
type vacuum struct {
	cfg   *Vacuum
	dev   *Device
	track *tracker

	// Manual driving: the direction last sent, and the timer that stops it
	mu         sync.Mutex
	driveDir   string
	driveGen   int
	driveTimer *time.Timer
}

var (
	cfg      *Config
	vacuums  = map[string]*vacuum{}
	commands = map[string]string{
		"start": "Start", "pause": "Pause", "resume": "Resume", "dock": "Return to dock",
		"stop": "Stop", "locate": "Locate", "suction": "Set suction", "drive": "Drive",
	}
	// driveTimeout stops a manual drive the page has stopped repeating, so a
	// closed tab or dropped Wi-Fi can't leave the vacuum driving into a wall
	driveTimeout = 2500 * time.Millisecond
)

func main() {
	configPath := flag.String("config", "config.yaml", "config file")
	discoverFlag := flag.Bool("discover", false, "listen for Tuya devices on the LAN for 10 seconds, print them and exit")
	dumpID := flag.String("dump", "", "connect to this vacuum id, print every data point it reports, and exit")
	dumpSeconds := flag.Int("seconds", 30, "how long -dump watches")
	dumpSet := flag.String("set", "", "with -dump: data points to send once connected, as JSON, e.g. '{\"26\":\"stop\"}'")
	flag.Parse()

	if *discoverFlag {
		runDiscover()
		return
	}

	var err error
	cfg, err = LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	if *dumpID != "" {
		runDump(*dumpID, time.Duration(*dumpSeconds)*time.Second, *dumpSet)
		return
	}

	if cfg.Database.Enabled {
		db, err = openStore(cfg.Database)
		if err != nil {
			log.Fatalf("Failed to open database: %v", err)
		}
		log.Printf("🗄️  History and schedules in MariaDB database %s", cfg.Database.Name)
	} else {
		log.Printf("Database disabled: no clean history, and schedules are off")
	}

	stop := make(chan struct{})
	for i := range cfg.Vacuums {
		v := &cfg.Vacuums[i]
		dev, err := newDevice(v)
		if err != nil {
			log.Fatalf("Vacuum %s: %v", v.ID, err)
		}
		t := newTracker(v.ID, db.RecordClean)
		dev.onChange = t.Update
		vacuums[v.ID] = &vacuum{cfg: v, dev: dev, track: t}
		go dev.Run(stop)
	}
	if db != nil {
		go runScheduler(stop)
	}

	addr := fmt.Sprintf("%s:%d", cfg.Server.Bind, cfg.Server.Port)
	log.Printf("🤖 MyVacControl starting on http://%s (%d vacuum(s))", addr, len(cfg.Vacuums))
	if cfg.Server.AllowDirect {
		log.Printf("⚠️  allow_direct is on: requests that bypass the hub are treated as admin")
	}
	log.Fatal(http.ListenAndServe(addr, newMux()))
}

// Admins and users may drive the vacuum; which of them see it at all is up
// to the service's allowed_roles in the hub.
func newMux() *http.ServeMux {
	member := func(h http.HandlerFunc) http.HandlerFunc { return requireRole(h, "admin", "user") }
	files := http.StripPrefix("/static/", http.FileServer(http.Dir("static")))
	// no-cache makes the browser check for a newer app.js on every load (a
	// cheap 304 when unchanged), so an update never runs with the old script
	static := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /vac/health", handleHealth)

	mux.HandleFunc("GET /", member(handleIndex))
	mux.HandleFunc("GET /static/", member(static))
	mux.HandleFunc("GET /api/me", member(handleMe))
	mux.HandleFunc("GET /api/vacuums", member(handleListVacuums))
	mux.HandleFunc("GET /api/vacuums/{id}/status", member(handleStatus))
	mux.HandleFunc("POST /api/vacuums/{id}/command", member(handleCommand))
	mux.HandleFunc("GET /api/vacuums/{id}/history", member(handleHistory))
	mux.HandleFunc("GET /api/vacuums/{id}/schedules", member(handleListSchedules))
	mux.HandleFunc("POST /api/vacuums/{id}/schedules", member(handleAddSchedule))
	mux.HandleFunc("PUT /api/schedules/{sid}", member(handleUpdateSchedule))
	mux.HandleFunc("DELETE /api/schedules/{sid}", member(handleDeleteSchedule))
	mux.HandleFunc("PUT /api/vacuums/{id}/quiet", member(handleSetQuiet))
	mux.HandleFunc("GET /api/vacuums/{id}/stats", member(handleStats))
	mux.HandleFunc("GET /api/vacuums/{id}/parts", member(handleParts))
	mux.HandleFunc("POST /api/vacuums/{id}/parts/{part}/reset", member(handleResetPart))
	return mux
}

type ctxKey string

// requireRole trusts the X-Hub-* headers set by MyHomeServicesHub. That is only
// safe because this service binds to 127.0.0.1 by default, so only the hub can
// reach it.
func requireRole(next http.HandlerFunc, roles ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, role := r.Header.Get("X-Hub-User"), r.Header.Get("X-Hub-Role")
		if r.Header.Get("X-Hub-Authenticated") != "true" {
			if !cfg.Server.AllowDirect {
				writeError(w, http.StatusUnauthorized, "open this page through MyHomeServicesHub")
				return
			}
			user, role = "direct", "admin"
		}
		if !contains(roles, role) {
			writeError(w, http.StatusForbidden, "your role ("+role+") cannot do this")
			return
		}
		ctx := context.WithValue(r.Context(), ctxKey("user"), user)
		ctx = context.WithValue(ctx, ctxKey("role"), role)
		next(w, r.WithContext(ctx))
	}
}

func currentUser(r *http.Request) string {
	return r.Context().Value(ctxKey("user")).(string)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	content, err := os.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "Failed to load page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(content)
}

func handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"user": currentUser(r),
		"role": r.Context().Value(ctxKey("role")).(string),
	})
}

func handleListVacuums(w http.ResponseWriter, r *http.Request) {
	type vacView struct {
		ID         string   `json:"id"`
		Name       string   `json:"name"`
		Modes      []string `json:"modes"`
		Suction    []string `json:"suction"`
		CanLocate  bool     `json:"can_locate"`
		CanSuction bool     `json:"can_suction"`
		CanPause   bool     `json:"can_pause"`
		CanDrive   bool     `json:"can_drive"`
	}
	list := []vacView{}
	for _, v := range cfg.Vacuums {
		list = append(list, vacView{v.ID, v.Name, v.Modes, v.Suction, v.DPs.Locate != 0, v.DPs.Suction != 0,
			v.DPs.Start != 0 || len(v.Pause) > 0, v.DPs.Direction != 0})
	}
	writeJSON(w, http.StatusOK, map[string]any{"vacuums": list, "database": db != nil})
}

func findVacuum(w http.ResponseWriter, id string) (*vacuum, bool) {
	if v, ok := vacuums[id]; ok {
		return v, true
	}
	writeError(w, http.StatusNotFound, "unknown vacuum "+id)
	return nil, false
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	v, ok := findVacuum(w, r.PathValue("id"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": v.dev.State(), "current": v.track.Current()})
}

func handleCommand(w http.ResponseWriter, r *http.Request) {
	v, ok := findVacuum(w, r.PathValue("id"))
	if !ok {
		return
	}
	var req struct {
		Action string `json:"action"`
		Value  string `json:"value"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if _, err := v.cfg.Command(req.Action, req.Value); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := v.run(currentUser(r), req.Action, req.Value); err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errOffline) {
			status = http.StatusServiceUnavailable
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// run sends a command and logs who sent it.
func (v *vacuum) run(by, action, value string) error {
	if action == "drive" {
		return v.drive(by, value)
	}
	// Resume in the paused clean's mode
	if action == "resume" && value == "" {
		if c := v.track.Current(); c != nil && contains(v.cfg.Modes, c.Mode) {
			value = c.Mode
		}
	}
	dps, err := v.cfg.Command(action, value)
	if err != nil {
		return err
	}
	if action == "start" {
		v.track.NoteStart(by)
	}
	err = v.dev.Set(dps)
	if err != nil && action == "start" {
		v.track.NoteStart("") // nothing was started
	}
	detail := commands[action]
	if value != "" {
		detail += ": " + value
	}
	entry := CommandEntry{User: by, VacuumID: v.cfg.ID, Action: action, Detail: detail, OK: err == nil}
	if err != nil {
		entry.Error = err.Error()
		log.Printf("❌ %s: %s by %s failed: %v", v.cfg.ID, detail, by, err)
	} else {
		log.Printf("▶️  %s: %s by %s", v.cfg.ID, detail, by)
	}
	db.RecordCommand(entry)
	return err
}

// drive sends a manual direction. The page repeats it every second while
// the button is held; if the repeats stop, the vacuum is stopped.
func (v *vacuum) drive(by, dir string) error {
	dps, err := v.cfg.Command("drive", dir)
	if err != nil {
		return err
	}
	v.mu.Lock()
	if v.driveTimer != nil {
		v.driveTimer.Stop()
		v.driveTimer = nil
	}
	v.driveGen++
	// Only the first press of a direction is logged, not every repeat
	changed := dir != "stop" && dir != v.driveDir
	v.driveDir = dir
	if dir != "stop" {
		gen := v.driveGen
		v.driveTimer = time.AfterFunc(driveTimeout, func() { v.driveTimedOut(gen) })
	}
	v.mu.Unlock()

	err = v.dev.Set(dps)
	if err != nil || changed {
		entry := CommandEntry{User: by, VacuumID: v.cfg.ID, Action: "drive", Detail: "Drive: " + dir, OK: err == nil}
		if err != nil {
			entry.Error = err.Error()
			log.Printf("❌ %s: drive %s by %s failed: %v", v.cfg.ID, dir, by, err)
		}
		db.RecordCommand(entry)
	}
	return err
}

func (v *vacuum) driveTimedOut(gen int) {
	v.mu.Lock()
	// A press that came in as the timer fired keeps driving
	if gen != v.driveGen {
		v.mu.Unlock()
		return
	}
	v.driveDir, v.driveTimer = "", nil
	v.mu.Unlock()
	dps, _ := v.cfg.Command("drive", "stop")
	if err := v.dev.Set(dps); err != nil {
		log.Printf("⚠️  %s: could not stop a drive the page let go of: %v", v.cfg.ID, err)
		return
	}
	log.Printf("🛑 %s: drive stopped, no word from the page", v.cfg.ID)
}

func handleHistory(w http.ResponseWriter, r *http.Request) {
	v, ok := findVacuum(w, r.PathValue("id"))
	if !ok {
		return
	}
	if db == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "cleans": []CleanRun{}, "commands": []CommandEntry{}})
		return
	}
	cleans, err := db.RecentCleans(v.cfg.ID, 50)
	if err != nil {
		log.Printf("History query failed: %v", err)
		writeError(w, http.StatusInternalServerError, "could not read clean history")
		return
	}
	cmds, err := db.RecentCommands(v.cfg.ID, 50)
	if err != nil {
		log.Printf("Command log query failed: %v", err)
		writeError(w, http.StatusInternalServerError, "could not read the command log")
		return
	}
	for i := range cleans {
		cleans[i].Faults = v.cfg.Faults(cleans[i].ErrorCode)
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "cleans": cleans, "commands": cmds})
}

func requireDB(w http.ResponseWriter) bool {
	if db == nil {
		writeError(w, http.StatusServiceUnavailable, "schedules need the database (database.enabled in config.yaml)")
		return false
	}
	return true
}

func handleListSchedules(w http.ResponseWriter, r *http.Request) {
	v, ok := findVacuum(w, r.PathValue("id"))
	if !ok || !requireDB(w) {
		return
	}
	scheds, err := db.Schedules(v.cfg.ID)
	if err != nil {
		log.Printf("Schedule query failed: %v", err)
		writeError(w, http.StatusInternalServerError, "could not read schedules")
		return
	}
	quiet, err := db.QuietHours(v.cfg.ID)
	if err != nil {
		log.Printf("Quiet hours query failed: %v", err)
		writeError(w, http.StatusInternalServerError, "could not read quiet hours")
		return
	}
	for i := range scheds {
		scheds[i].InQuiet = quiet.Covers(scheds[i].At)
	}
	now := time.Now()
	writeJSON(w, http.StatusOK, map[string]any{"schedules": scheds, "quiet": quiet,
		"timezone": now.Format("MST"), "today": now.Format("2006-01-02")})
}

func handleSetQuiet(w http.ResponseWriter, r *http.Request) {
	v, ok := findVacuum(w, r.PathValue("id"))
	if !ok || !requireDB(w) {
		return
	}
	var q QuietHours
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&q); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if err := q.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := db.SetQuietHours(v.cfg.ID, q); err != nil {
		log.Printf("Failed to save quiet hours: %v", err)
		writeError(w, http.StatusInternalServerError, "could not save quiet hours")
		return
	}
	detail := "Quiet hours off"
	if q.From != "" {
		detail = fmt.Sprintf("Quiet hours %s to %s", q.From, q.To)
	}
	db.RecordCommand(CommandEntry{User: currentUser(r), VacuumID: v.cfg.ID, Action: "quiet_hours", Detail: detail, OK: true})
	writeJSON(w, http.StatusOK, q)
}

// statsWeeks is how many weeks the statistics chart shows.
const statsWeeks = 12

func handleStats(w http.ResponseWriter, r *http.Request) {
	v, ok := findVacuum(w, r.PathValue("id"))
	if !ok || !requireDB(w) {
		return
	}
	cleans, err := db.CleansSince(v.cfg.ID, time.Time{})
	if err != nil {
		log.Printf("Stats query failed: %v", err)
		writeError(w, http.StatusInternalServerError, "could not read clean history")
		return
	}
	writeJSON(w, http.StatusOK, summarize(cleans, time.Now(), statsWeeks))
}

func handleParts(w http.ResponseWriter, r *http.Request) {
	v, ok := findVacuum(w, r.PathValue("id"))
	if !ok || !requireDB(w) {
		return
	}
	resets, err := db.PartResets(v.cfg.ID)
	if err != nil {
		log.Printf("Part query failed: %v", err)
		writeError(w, http.StatusInternalServerError, "could not read part replacements")
		return
	}
	cleans, err := db.CleansSince(v.cfg.ID, time.Time{})
	if err != nil {
		log.Printf("Clean query failed: %v", err)
		writeError(w, http.StatusInternalServerError, "could not read clean history")
		return
	}
	current, now := v.track.Current(), time.Now()
	parts := []PartStatus{}
	for _, p := range v.cfg.Parts {
		var reset *PartReset
		if rs, ok := resets[p.ID]; ok {
			reset = &rs
		}
		parts = append(parts, partStatus(p, reset, cleans, current, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{"parts": parts})
}

func handleResetPart(w http.ResponseWriter, r *http.Request) {
	v, ok := findVacuum(w, r.PathValue("id"))
	if !ok || !requireDB(w) {
		return
	}
	var part *Part
	for i := range v.cfg.Parts {
		if v.cfg.Parts[i].ID == r.PathValue("part") {
			part = &v.cfg.Parts[i]
		}
	}
	if part == nil {
		writeError(w, http.StatusNotFound, "unknown part "+r.PathValue("part"))
		return
	}
	if err := db.ResetPart(v.cfg.ID, part.ID, currentUser(r), time.Now()); err != nil {
		log.Printf("Failed to reset part: %v", err)
		writeError(w, http.StatusInternalServerError, "could not save the replacement")
		return
	}
	db.RecordCommand(CommandEntry{User: currentUser(r), VacuumID: v.cfg.ID, Action: "part_reset", Detail: "Replaced: " + part.Name, OK: true})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func decodeSchedule(w http.ResponseWriter, r *http.Request, v *Vacuum) (*Schedule, bool) {
	var sc Schedule
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&sc); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return nil, false
	}
	if err := sc.Validate(v); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return nil, false
	}
	return &sc, true
}

func handleAddSchedule(w http.ResponseWriter, r *http.Request) {
	v, ok := findVacuum(w, r.PathValue("id"))
	if !ok || !requireDB(w) {
		return
	}
	sc, ok := decodeSchedule(w, r, v.cfg)
	if !ok {
		return
	}
	sc.VacuumID, sc.CreatedBy = v.cfg.ID, currentUser(r)
	id, err := db.AddSchedule(*sc)
	if err != nil {
		log.Printf("Failed to add schedule: %v", err)
		writeError(w, http.StatusInternalServerError, "could not save the schedule")
		return
	}
	sc.ID = id
	db.RecordCommand(CommandEntry{User: sc.CreatedBy, VacuumID: v.cfg.ID, Action: "schedule_add", Detail: describeSchedule(*sc), OK: true})
	writeJSON(w, http.StatusOK, sc)
}

// scheduleFromPath loads the schedule named in the URL.
func scheduleFromPath(w http.ResponseWriter, r *http.Request) (Schedule, *vacuum, bool) {
	id, err := strconv.ParseInt(r.PathValue("sid"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad schedule id")
		return Schedule{}, nil, false
	}
	sc, err := db.Schedule(id)
	if errors.Is(err, errNotFound) {
		writeError(w, http.StatusNotFound, "no such schedule")
		return sc, nil, false
	}
	if err != nil {
		log.Printf("Schedule query failed: %v", err)
		writeError(w, http.StatusInternalServerError, "could not read schedules")
		return sc, nil, false
	}
	v, ok := findVacuum(w, sc.VacuumID)
	return sc, v, ok
}

func handleUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	if !requireDB(w) {
		return
	}
	old, v, ok := scheduleFromPath(w, r)
	if !ok {
		return
	}
	sc, ok := decodeSchedule(w, r, v.cfg)
	if !ok {
		return
	}
	sc.ID, sc.VacuumID = old.ID, old.VacuumID
	if err := db.UpdateSchedule(*sc); err != nil {
		log.Printf("Failed to update schedule: %v", err)
		writeError(w, http.StatusInternalServerError, "could not save the schedule")
		return
	}
	db.RecordCommand(CommandEntry{User: currentUser(r), VacuumID: v.cfg.ID, Action: "schedule_edit", Detail: describeSchedule(*sc), OK: true})
	writeJSON(w, http.StatusOK, sc)
}

func handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	if !requireDB(w) {
		return
	}
	sc, v, ok := scheduleFromPath(w, r)
	if !ok {
		return
	}
	if err := db.DeleteSchedule(sc.ID); err != nil {
		log.Printf("Failed to delete schedule: %v", err)
		writeError(w, http.StatusInternalServerError, "could not delete the schedule")
		return
	}
	db.RecordCommand(CommandEntry{User: currentUser(r), VacuumID: v.cfg.ID, Action: "schedule_delete", Detail: describeSchedule(sc), OK: true})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// describeSchedule summarises a schedule for the command log.
func describeSchedule(sc Schedule) string {
	var days []string
	for d := time.Sunday; d <= time.Saturday; d++ {
		if sc.Days&(1<<int(d)) != 0 {
			days = append(days, d.String()[:3])
		}
	}
	when := strings.Join(days, " ")
	if sc.Date != "" {
		when = "once on " + sc.Date
	}
	s := fmt.Sprintf("%s: %s at %s, %s", sc.Name, when, sc.At, sc.Mode)
	if sc.Suction != "" {
		s += ", " + sc.Suction
	}
	if !sc.Enabled {
		s += " (off)"
	}
	return s
}

// runDiscover prints the Tuya devices broadcasting on the LAN, to find the
// vacuum's IP address, device id and protocol version.
func runDiscover() {
	fmt.Println("Listening for Tuya devices on UDP 6666/6667 for 10 seconds...")
	devs, err := discover(10 * time.Second)
	if err != nil {
		log.Fatal(err)
	}
	if len(devs) == 0 {
		fmt.Println("Nothing heard. Is this machine on the same network as the vacuum, and is the vacuum awake?")
		return
	}
	sort.Slice(devs, func(i, j int) bool { return devs[i].IP < devs[j].IP })
	for _, d := range devs {
		fmt.Printf("%-16s id %-24s version %-4s product %s\n", d.IP, d.ID, d.Version, d.ProductKey)
	}
}

// runDump connects to one vacuum and prints every data point update, to see
// which data points a model uses.
func runDump(id string, d time.Duration, set string) {
	var setDPs map[string]any
	if set != "" {
		if err := json.Unmarshal([]byte(set), &setDPs); err != nil {
			log.Fatalf("-set: %v", err)
		}
	}
	var v *Vacuum
	for i := range cfg.Vacuums {
		if cfg.Vacuums[i].ID == id {
			v = &cfg.Vacuums[i]
		}
	}
	if v == nil {
		log.Fatalf("no vacuum %q in the config", id)
	}
	dev, err := newDevice(v)
	if err != nil {
		log.Fatal(err)
	}
	dev.onChange = func(before, after State) {
		changed := map[string]any{}
		for k, val := range after.DPs {
			if b, ok := before.DPs[k]; !ok || fmt.Sprint(b) != fmt.Sprint(val) {
				changed[k] = val
			}
		}
		if len(changed) > 0 {
			out, _ := json.MarshalIndent(changed, "", "  ")
			fmt.Printf("%s data points:\n%s\n", time.Now().Format("15:04:05"), out)
		}
		if after.Phase != before.Phase || after.StatusLabel != before.StatusLabel {
			fmt.Printf("  -> %s (phase %q)\n", after.StatusLabel, after.Phase)
		}
	}
	stop := make(chan struct{})
	go dev.Run(stop)
	fmt.Printf("Connecting to %s; printing updates for %s. Try the vacuum's app or buttons now.\n", v.Host, d)
	deadline := time.Now().Add(d)
	if setDPs != nil {
		for !dev.State().Connected && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		time.Sleep(time.Second) // let the first full report print first
		fmt.Printf("%s sending %s\n", time.Now().Format("15:04:05"), set)
		if err := dev.Set(setDPs); err != nil {
			fmt.Println("send failed:", err)
		}
	}
	time.Sleep(time.Until(deadline))
	close(stop)
	if st := dev.State(); !st.Connected && st.LastError != "" {
		fmt.Println("Last error:", st.LastError)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
