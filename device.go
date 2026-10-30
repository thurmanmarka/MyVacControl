package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"sync"
	"time"
)

const (
	dialTimeout       = 5 * time.Second
	heartbeatInterval = 10 * time.Second
	// The vacuum answers each heartbeat, so this long without a frame means
	// the connection is dead
	readTimeout = 3 * heartbeatInterval
)

// Stats is what the AutoVac reports in its raw-data point during and after a
// clean (infoType 20001).
type Stats struct {
	Area      int    `json:"area"`       // m²
	CleanTime int    `json:"clean_time"` // as reported by the vacuum
	Battery   int    `json:"battery"`
	Mode      string `json:"mode,omitempty"`
	SubMode   string `json:"sub_mode,omitempty"`
}

// State is the vacuum as last heard from.
type State struct {
	Connected bool       `json:"connected"`
	LastSeen  *time.Time `json:"last_seen,omitempty"`
	LastError string     `json:"last_error,omitempty"`
	Status    string     `json:"status"`
	// Phase is cleaning, paused or idle ("" until known); StatusLabel is
	// for people
	Phase       string         `json:"phase"`
	StatusLabel string         `json:"status_label"`
	Mode        string         `json:"mode"`
	Suction     string         `json:"suction"`
	Battery     *int           `json:"battery"`
	ErrorCode   int            `json:"error_code"`
	Faults      []string       `json:"faults,omitempty"` // ErrorCode for people
	Stats       *Stats         `json:"stats,omitempty"`
	DPs         map[string]any `json:"dps"`
}

// Device keeps one LAN connection open to a vacuum. Tuya devices accept very
// few local connections at once, so every command goes through this one.
type Device struct {
	cfg   *Vacuum
	codec *tuyaCodec
	addr  string // host:port of the vacuum

	mu    sync.Mutex
	conn  net.Conn
	seq   uint32
	state State

	// onChange is called (without mu held) after each update with the
	// state before and after it.
	onChange func(before, after State)
}

var errOffline = errors.New("the vacuum is not connected")

func newDevice(v *Vacuum) (*Device, error) {
	codec, err := newTuyaCodec(v.DeviceID, v.LocalKey)
	if err != nil {
		return nil, err
	}
	return &Device{
		cfg:   v,
		codec: codec,
		addr:  net.JoinHostPort(v.Host, strconv.Itoa(tuyaPort)),
		state: State{DPs: map[string]any{}},
	}, nil
}

// State returns a copy of the current state.
func (d *Device) State() State {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.snapshot()
}

// snapshot copies the state; mu must be held.
func (d *Device) snapshot() State {
	s := d.state
	s.DPs = make(map[string]any, len(d.state.DPs))
	for k, v := range d.state.DPs {
		s.DPs[k] = v
	}
	if d.state.Stats != nil {
		st := *d.state.Stats
		s.Stats = &st
	}
	return s
}

// Run keeps the connection up until stop is closed.
func (d *Device) Run(stop <-chan struct{}) {
	backoff := 5 * time.Second
	for {
		started := time.Now()
		err := d.session(stop)
		select {
		case <-stop:
			return
		default:
		}
		d.setDisconnected(err)
		if time.Since(started) > time.Minute {
			backoff = 5 * time.Second
		}
		log.Printf("🔌 %s: not connected (%v); retrying in %s", d.cfg.ID, err, backoff)
		select {
		case <-stop:
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

// session connects, reads every data point, then reads pushes and sends
// heartbeats until the connection fails.
func (d *Device) session(stop <-chan struct{}) error {
	conn, err := net.DialTimeout("tcp", d.addr, dialTimeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	d.mu.Lock()
	d.conn = conn
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.conn = nil
		d.mu.Unlock()
	}()

	if err := d.send(cmdDPQuery, nil); err != nil {
		return err
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(heartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-stop:
				conn.Close()
				return
			case <-t.C:
				if err := d.send(cmdHeartbeat, nil); err != nil {
					conn.Close()
					return
				}
			}
		}
	}()

	for {
		conn.SetReadDeadline(time.Now().Add(readTimeout))
		f, err := readFrame(conn, d.codec.key)
		if err != nil {
			return err
		}
		d.markSeen()
		if f.Cmd == cmdHeartbeat || len(f.Payload) == 0 {
			continue
		}
		if f.RetCode != 0 {
			log.Printf("⚠️  %s: command %d failed (code %d): %s", d.cfg.ID, f.Cmd, f.RetCode, truncate(string(f.Payload), 200))
			continue
		}
		dps, err := parseDPs(f.Payload)
		if err != nil {
			log.Printf("⚠️  %s: %v", d.cfg.ID, err)
			continue
		}
		if len(dps) > 0 {
			d.apply(dps)
		}
	}
}

func (d *Device) send(cmd uint32, dps map[string]any) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		return errOffline
	}
	d.seq++
	frame, err := d.codec.Encode(d.seq, cmd, dps)
	if err != nil {
		return err
	}
	d.conn.SetWriteDeadline(time.Now().Add(dialTimeout))
	_, err = d.conn.Write(frame)
	return err
}

// Set changes data points. The vacuum pushes the result back, which updates
// the state.
func (d *Device) Set(dps map[string]any) error {
	return d.send(cmdControl, dps)
}

// Refresh asks for every data point again.
func (d *Device) Refresh() error {
	return d.send(cmdDPQuery, nil)
}

func (d *Device) markSeen() {
	now := time.Now()
	d.mu.Lock()
	if !d.state.Connected {
		log.Printf("🤖 %s: connected to %s", d.cfg.ID, d.cfg.Host)
	}
	d.state.Connected, d.state.LastSeen, d.state.LastError = true, &now, ""
	d.mu.Unlock()
}

func (d *Device) setDisconnected(err error) {
	d.mu.Lock()
	before := d.snapshot()
	d.state.Connected = false
	if err != nil {
		d.state.LastError = err.Error()
	}
	after := d.snapshot()
	d.mu.Unlock()
	if d.onChange != nil && before.Connected {
		d.onChange(before, after)
	}
}

// apply merges changed data points into the state.
func (d *Device) apply(dps map[string]any) {
	d.mu.Lock()
	before := d.snapshot()
	dm := d.cfg.DPs
	for k, v := range dps {
		d.state.DPs[k] = v
		n, _ := strconv.Atoi(k)
		switch n {
		case 0:
		case dm.Status:
			d.state.Status = fmt.Sprint(v)
		case dm.Mode:
			d.state.Mode, _ = v.(string)
		case dm.Suction:
			d.state.Suction, _ = v.(string)
		case dm.Battery:
			// The Nova sends 0 here locally; its raw-data report is used then
			if b, ok := v.(float64); ok && (b > 0 || dm.RawData == 0) {
				d.state.Battery = ptr(int(b))
			}
		case dm.Error:
			if e, ok := v.(float64); ok {
				d.state.ErrorCode = int(e)
				d.state.Faults = d.cfg.Faults(int(e))
			}
		case dm.RawData:
			if s, ok := v.(string); ok {
				if st, ok := decodeRawStats(s); ok {
					d.state.Stats = st
					if st.Battery > 0 {
						d.state.Battery = ptr(st.Battery)
					}
				}
			}
		case dm.Area, dm.CleanTime:
			if x, ok := v.(float64); ok {
				if d.state.Stats == nil {
					d.state.Stats = &Stats{}
				}
				if n == dm.Area {
					d.state.Stats.Area = int(x)
				} else {
					d.state.Stats.CleanTime = int(x)
				}
			}
		}
	}
	d.state.Phase = d.cfg.Phase(d.state)
	d.state.StatusLabel = d.cfg.Label(d.state)
	after := d.snapshot()
	d.mu.Unlock()
	if d.onChange != nil {
		d.onChange(before, after)
	}
}

// decodeRawStats reads the AutoVac's raw-data point: base64 JSON tagged with
// an infoType. Only the clean statistics report (20001 with cleanArea) is
// kept; position and path reports are ignored.
func decodeRawStats(s string) (*Stats, bool) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, false
	}
	var msg struct {
		InfoType int `json:"infoType"`
		Data     struct {
			CleanArea *int   `json:"cleanArea"`
			CleanTime int    `json:"cleanTime"`
			Elec      int    `json:"elec"`
			Mode      string `json:"mode"`
			SubMode   string `json:"subMode"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &msg) != nil || msg.Data.CleanArea == nil {
		return nil, false
	}
	return &Stats{
		Area:      *msg.Data.CleanArea,
		CleanTime: msg.Data.CleanTime,
		Battery:   msg.Data.Elec,
		Mode:      msg.Data.Mode,
		SubMode:   msg.Data.SubMode,
	}, true
}

func ptr[T any](v T) *T { return &v }

// dp formats a data point number as the key Tuya uses.
func dp(n int) string { return strconv.Itoa(n) }

// Command turns a request from the page into data points to set.
func (v *Vacuum) Command(action, value string) (map[string]any, error) {
	dm := v.DPs
	need := func(n int, what string) error {
		if n == 0 {
			return fmt.Errorf("this vacuum has no %s data point configured", what)
		}
		return nil
	}
	switch action {
	case "start":
		if err := need(dm.Mode, "mode"); err != nil {
			return nil, err
		}
		if value == "" {
			value = v.Modes[0]
		}
		if !contains(v.Modes, value) {
			return nil, fmt.Errorf("unknown mode %q", value)
		}
		dps := map[string]any{dp(dm.Mode): value}
		if dm.Start != 0 {
			dps[dp(dm.Start)] = true
		}
		return dps, nil
	case "pause":
		if len(v.Pause) > 0 {
			return v.Pause, nil
		}
		if err := need(dm.Start, "start/pause"); err != nil {
			return nil, err
		}
		return map[string]any{dp(dm.Start): false}, nil
	case "resume":
		if dm.Start != 0 && len(v.Pause) == 0 {
			return map[string]any{dp(dm.Start): true}, nil
		}
		// Paused by its own pause command: carry on in the clean's mode
		// (value), or the default one
		return v.Command("start", value)
	case "dock":
		if err := need(dm.Mode, "mode"); err != nil {
			return nil, err
		}
		return map[string]any{dp(dm.Mode): v.DockMode}, nil
	case "stop":
		if len(v.Stop) > 0 {
			return v.Stop, nil
		}
		if err := need(dm.Mode, "mode"); err != nil {
			return nil, err
		}
		return map[string]any{dp(dm.Mode): v.StopMode}, nil
	case "locate":
		if err := need(dm.Locate, "locate"); err != nil {
			return nil, err
		}
		return map[string]any{dp(dm.Locate): true}, nil
	case "suction":
		if err := need(dm.Suction, "suction"); err != nil {
			return nil, err
		}
		if !contains(v.Suction, value) {
			return nil, fmt.Errorf("unknown suction level %q", value)
		}
		return map[string]any{dp(dm.Suction): value}, nil
	case "drive":
		if err := need(dm.Direction, "direction"); err != nil {
			return nil, err
		}
		d, ok := v.Directions[value]
		if !ok {
			return nil, fmt.Errorf("unknown direction %q", value)
		}
		return map[string]any{dp(dm.Direction): d}, nil
	}
	return nil, fmt.Errorf("unknown action %q", action)
}

// Faults describes an error data point value: each set bit of a bitmap when
// FaultBits are known, otherwise the whole code.
func (v *Vacuum) Faults(code int) []string {
	if code == 0 {
		return nil
	}
	if len(v.FaultBits) == 0 {
		if l, ok := v.FaultCodes[strconv.Itoa(code)]; ok {
			return []string{l}
		}
		return []string{fmt.Sprintf("Fault code %d", code)}
	}
	var out []string
	for i := 0; i < 31; i++ {
		if code&(1<<i) == 0 {
			continue
		}
		if i < len(v.FaultBits) && v.FaultBits[i] != "" {
			out = append(out, v.FaultBits[i])
		} else {
			out = append(out, fmt.Sprintf("Fault bit %d", i))
		}
	}
	return out
}

// Phase says whether the vacuum is cleaning, paused or idle, or "" when
// that can't be told yet.
func (v *Vacuum) Phase(s State) string {
	if len(v.CleaningStatuses) > 0 {
		switch {
		case s.Status == "":
			return ""
		case contains(v.CleaningStatuses, s.Status):
			return "cleaning"
		case contains(v.PausedStatuses, s.Status):
			return "paused"
		}
		// A status not seen before (another cleaning mode's, perhaps) counts
		// as cleaning while the mode is a cleaning one
		if _, known := v.StatusLabels[s.Status]; !known && contains(v.Modes, s.Mode) {
			return "cleaning"
		}
		return "idle"
	}
	// Without known status values: a cleaning mode with the start switch on
	// is a clean, and off is a pause. Docking or stopping ends it.
	if s.Mode == "" {
		return ""
	}
	if !contains(v.Modes, s.Mode) {
		return "idle"
	}
	on, ok := s.DPs[dp(v.DPs.Start)].(bool)
	if !ok {
		return ""
	}
	if on {
		return "cleaning"
	}
	return "paused"
}

// Label describes the state for people.
func (v *Vacuum) Label(s State) string {
	if l, ok := v.StatusLabels[s.Status]; ok {
		return l
	}
	l := "Unknown"
	switch {
	case s.Phase == "cleaning":
		l = "Cleaning"
	case s.Phase == "paused":
		l = "Paused"
	case s.Mode == v.DockMode:
		l = "Docking / docked"
	case s.Phase == "idle":
		l = "Idle"
	}
	// Show the raw code so its meaning can be learned and added to status_labels
	if s.Status != "" {
		l += " (state " + s.Status + ")"
	}
	return l
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
