package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const testKey = "0123456789abcdef"

func testVacuum(host, model string) *Vacuum {
	v := &Vacuum{ID: "t", Name: "Test", Host: host, DeviceID: "dev123", LocalKey: testKey, Version: "3.3", Model: model}
	if err := v.applyModel(); err != nil {
		panic(err)
	}
	return v
}

// deviceFrame builds a frame the way the vacuum sends it: return code, then
// (for pushes) the version header, then the encrypted JSON.
func deviceFrame(seq, cmd uint32, v any, header bool) []byte {
	plain, _ := json.Marshal(v)
	payload := binary.BigEndian.AppendUint32(nil, 0)
	if header {
		payload = append(payload, version33Header...)
	}
	payload = append(payload, ecbEncrypt([]byte(testKey), plain)...)
	return packFrame(seq, cmd, payload)
}

func TestCodecRoundTrip(t *testing.T) {
	c, err := newTuyaCodec("dev123", testKey)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := c.Encode(5, cmdControl, map[string]any{"3": "smart"})
	if err != nil {
		t.Fatal(err)
	}
	f, err := readFrame(bytes.NewReader(frame), c.key)
	if err != nil {
		t.Fatal(err)
	}
	if f.Seq != 5 || f.Cmd != cmdControl {
		t.Fatalf("got seq %d cmd %d", f.Seq, f.Cmd)
	}
	dps, err := parseDPs(f.Payload)
	if err != nil || dps["3"] != "smart" {
		t.Fatalf("dps %v, err %v", dps, err)
	}

	// A corrupted byte fails the CRC
	frame[20] ^= 0xFF
	if _, err := readFrame(bytes.NewReader(frame), c.key); err == nil || !strings.Contains(err.Error(), "CRC") {
		t.Fatalf("want CRC error, got %v", err)
	}
	if _, err := newTuyaCodec("x", "short"); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestDeviceFrameWithHeader(t *testing.T) {
	frame := deviceFrame(1, cmdStatus, map[string]any{"dps": map[string]any{"5": "charging"}}, true)
	f, err := readFrame(bytes.NewReader(frame), []byte(testKey))
	if err != nil {
		t.Fatal(err)
	}
	dps, err := parseDPs(f.Payload)
	if err != nil || dps["5"] != "charging" {
		t.Fatalf("dps %v, err %v", dps, err)
	}
}

func TestDecodeRawStats(t *testing.T) {
	stats := `{"data":{"cleanArea":11,"cleanTime":570,"elec":82,"elecReal":82,"mode":"sweep","subMode":"smart"},"infoType":20001}`
	st, ok := decodeRawStats(base64.StdEncoding.EncodeToString([]byte(stats)))
	if !ok || st.Area != 11 || st.CleanTime != 570 || st.Battery != 82 || st.SubMode != "smart" {
		t.Fatalf("got %+v %v", st, ok)
	}
	pos := `{"data":{"phi":-3095,"pos":[3205,366]},"infoType":20001}`
	if _, ok := decodeRawStats(base64.StdEncoding.EncodeToString([]byte(pos))); ok {
		t.Fatal("position report taken as stats")
	}
	if _, ok := decodeRawStats("not base64!"); ok {
		t.Fatal("garbage accepted")
	}
}

type commandCase struct {
	action, value string
	want          map[string]any
}

func checkCommands(t *testing.T, v *Vacuum, cases []commandCase) {
	t.Helper()
	for _, c := range cases {
		got, err := v.Command(c.action, c.value)
		if err != nil {
			t.Fatalf("%s %s: %v", v.Model, c.action, err)
		}
		gj, _ := json.Marshal(got)
		wj, _ := json.Marshal(c.want)
		if string(gj) != string(wj) {
			t.Errorf("%s %s %s: got %s want %s", v.Model, c.action, c.value, gj, wj)
		}
	}
}

func TestCommand(t *testing.T) {
	nova := testVacuum("x", "nova")
	checkCommands(t, nova, []commandCase{
		{"start", "", map[string]any{"3": "smart", "2": true}},
		{"start", "spiral", map[string]any{"3": "spiral", "2": true}},
		{"pause", "", map[string]any{"2": false}},
		{"resume", "", map[string]any{"2": true}},
		{"dock", "", map[string]any{"3": "chargego"}},
		{"stop", "", map[string]any{"3": "standby"}},
		{"locate", "", map[string]any{"13": true}},
		{"suction", "strong", map[string]any{"14": "strong"}},
	})
	checkCommands(t, testVacuum("x", "eclipse"), []commandCase{
		{"start", "", map[string]any{"25": "smart"}},
		{"start", "sprial", map[string]any{"25": "sprial"}},
		{"pause", "", map[string]any{"26": "stop"}},
		{"resume", "", map[string]any{"25": "smart"}},
		{"resume", "wallfollow", map[string]any{"25": "wallfollow"}},
		{"dock", "", map[string]any{"25": "chargego"}},
		{"stop", "", map[string]any{"26": "stop"}},
		{"suction", "ECO", map[string]any{"27": "ECO"}},
	})
	for _, bad := range [][2]string{{"start", "turbo"}, {"suction", "max"}, {"selfdestruct", ""}} {
		if _, err := nova.Command(bad[0], bad[1]); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	// The Eclipse has no locate data point
	if _, err := testVacuum("x", "eclipse").Command("locate", ""); err == nil {
		t.Error("locate without a data point accepted")
	}
}

// Without named status values, the Eclipse's phase comes from its start
// switch and mode
// Replays a real iHRV2 start/pause/resume/dock cycle through the device and
// tracker. The Eclipse never reports its start switch (33) locally.
func TestEclipseCycle(t *testing.T) {
	v := testVacuum("x", "eclipse")
	dev, err := newDevice(v)
	if err != nil {
		t.Fatal(err)
	}
	var saved []CleanRun
	tr := newTracker("t", func(c CleanRun) { saved = append(saved, c) })
	dev.onChange = tr.Update
	dev.state.Connected = true

	steps := []struct {
		dps   map[string]any
		phase string
		label string
	}{
		{map[string]any{"1": true, "25": "chargego", "27": "normal", "38": "4", "39": 96.0, "41": 0.0, "42": 0.0}, "idle", "Returning to dock"},
		{map[string]any{"25": "smart"}, "idle", "Returning to dock"}, // mode arrives a second before the status
		{map[string]any{"38": "1"}, "cleaning", "Cleaning"},
		{map[string]any{"38": "7"}, "paused", "Paused"},
		{map[string]any{"41": 3.0, "39": 95.0}, "paused", "Paused"},
		// Stopped where it is (the module's pause), then resumed by setting the mode
		{map[string]any{"26": "stop", "25": "idle", "38": "0"}, "paused", "Stopped"},
		{map[string]any{"25": "smart"}, "paused", "Stopped"},
		{map[string]any{"38": "1"}, "cleaning", "Cleaning"},
		{map[string]any{"25": "chargego"}, "cleaning", "Cleaning"},
		{map[string]any{"38": "4", "39": 94.0}, "idle", "Returning to dock"},
		{map[string]any{"25": "idle"}, "idle", "Returning to dock"},
		{map[string]any{"38": "5"}, "idle", "On dock, charging"},
	}
	for i, s := range steps {
		dev.apply(s.dps)
		st := dev.State()
		if st.Phase != s.phase || st.StatusLabel != s.label {
			t.Errorf("step %d %v: got %q / %q, want %q / %q", i, s.dps, st.Phase, st.StatusLabel, s.phase, s.label)
		}
	}
	if len(saved) != 1 {
		t.Fatalf("saved %d cleans", len(saved))
	}
	c := saved[0]
	if c.Mode != "smart" || c.EndStatus != "Returning to dock" || c.Area == nil || *c.Area != 3 || *c.BatteryEnd != 94 {
		t.Fatalf("clean %+v", c)
	}

	// A status code not seen before counts as cleaning in a cleaning mode
	if p := v.Phase(State{Status: "3", Mode: "wallfollow"}); p != "cleaning" {
		t.Errorf("unknown status in a cleaning mode: %q", p)
	}
	if p := v.Phase(State{Status: "6", Mode: "idle"}); p != "idle" {
		t.Errorf("unknown status while idle: %q", p)
	}
}

func TestLoadConfigModel(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	write := func(s string) {
		if err := os.WriteFile(path, []byte(s), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("vacuums:\n  - {id: v, host: 1.2.3.4, device_id: d, local_key: \"" + testKey + "\"}\n")
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if v := c.Vacuums[0]; v.Model != "eclipse" || v.DPs.Mode != 25 || v.Pause["26"] != "stop" || c.Server.Port != 8085 {
		t.Fatalf("defaults %+v", v)
	}
	write("vacuums:\n  - {id: v, host: 1.2.3.4, device_id: d, local_key: \"" + testKey + "\", model: roomba}\n")
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("unknown model accepted")
	}
}

func TestTracker(t *testing.T) {
	var saved []CleanRun
	v := testVacuum("x", "nova")
	tr := newTracker("t", func(c CleanRun) { saved = append(saved, c) })
	st := func(status string, battery int, area int) State {
		s := State{Connected: true, Status: status, Mode: "smart", Battery: ptr(battery)}
		if area > 0 {
			s.Stats = &Stats{Area: area}
		}
		s.Phase = v.Phase(s)
		s.StatusLabel = v.Label(s)
		return s
	}

	tr.NoteStart("alice")
	tr.Update(st("charge_done", 100, 0), st("smart_clean", 100, 0))
	if c := tr.Current(); c == nil || c.StartedBy != "alice" {
		t.Fatalf("current %+v", c)
	}
	tr.Update(st("smart_clean", 100, 0), st("smart_clean", 90, 7))
	tr.Update(st("smart_clean", 90, 7), st("paused", 90, 7))                 // a pause keeps it open
	tr.Update(st("paused", 90, 7), State{Status: "paused", Phase: "paused"}) // so does a dropped connection
	tr.Update(st("paused", 90, 7), st("smart_clean", 88, 7))
	tr.Update(st("smart_clean", 88, 7), st("goto_charge", 80, 12))
	if len(saved) != 1 {
		t.Fatalf("saved %d cleans", len(saved))
	}
	c := saved[0]
	if c.StartedBy != "alice" || c.EndStatus != "Returning to dock" || *c.Area != 12 || *c.BatteryStart != 100 || *c.BatteryEnd != 80 {
		t.Fatalf("clean %+v", c)
	}

	// A clean nobody here asked for was started from the vacuum or its app
	tr.Update(st("charging", 80, 0), st("wall_clean", 80, 0))
	if c := tr.Current(); c == nil || c.StartedBy != "vacuum" {
		t.Fatalf("current %+v", c)
	}
}

func TestScheduleDue(t *testing.T) {
	v := testVacuum("x", "nova")
	sc := Schedule{Name: "Morning", Days: 1<<int(time.Monday) | 1<<int(time.Thursday), At: "09:30", Mode: "smart", Enabled: true}
	if err := sc.Validate(v); err != nil {
		t.Fatal(err)
	}
	mon := time.Date(2026, 11, 2, 9, 30, 15, 0, time.Local) // a Monday
	if !sc.Due(mon) {
		t.Fatal("not due on Monday 09:30")
	}
	if sc.Due(mon.Add(time.Minute)) || sc.Due(mon.AddDate(0, 0, 1)) {
		t.Fatal("due at the wrong time")
	}
	ran := mon.Add(-10 * time.Second)
	sc.LastRun = &ran
	if sc.Due(mon) {
		t.Fatal("due twice in one minute")
	}
	sc.LastRun, sc.Enabled = nil, false
	if sc.Due(mon) {
		t.Fatal("disabled schedule due")
	}

	for _, bad := range []Schedule{
		{Days: 0, At: "09:30", Mode: "smart"},
		{Days: 1, At: "9:30", Mode: "smart"},
		{Days: 1, At: "24:00", Mode: "smart"},
		{Days: 1, At: "09:30", Mode: "turbo"},
		{Days: 1, At: "09:30", Mode: "smart", Suction: "max"},
	} {
		if err := bad.Validate(v); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

func TestOnlyMembersAllowed(t *testing.T) {
	cfg = &Config{}
	h := newMux()
	for role, want := range map[string]int{"admin": 200, "user": 200, "guest": 403} {
		req := httptest.NewRequest("GET", "/api/vacuums", nil)
		req.Header.Set("X-Hub-Authenticated", "true")
		req.Header.Set("X-Hub-User", role)
		req.Header.Set("X-Hub-Role", role)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("%s: got %d want %d", role, rec.Code, want)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/vacuums", nil))
	if rec.Code != 401 {
		t.Errorf("direct request: got %d", rec.Code)
	}
}

// fakeVacuum is a Tuya 3.3 device on a local port: it answers queries with its
// data points and pushes the new values back after each control command.
type fakeVacuum struct {
	ln       net.Listener
	mu       sync.Mutex
	dps      map[string]any
	controls []map[string]any
}

func startFakeVacuum(t *testing.T) *fakeVacuum {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fv := &fakeVacuum{ln: ln, dps: map[string]any{"1": true, "5": "charge_done", "6": 0, "14": "gentle",
		"102": base64.StdEncoding.EncodeToString([]byte(`{"data":{"cleanArea":3,"cleanTime":60,"elec":95},"infoType":20001}`))}}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go fv.serve(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return fv
}

func (fv *fakeVacuum) serve(conn net.Conn) {
	defer conn.Close()
	key := []byte(testKey)
	for {
		f, err := readFrame(conn, key)
		if err != nil {
			return
		}
		switch f.Cmd {
		case cmdDPQuery:
			fv.mu.Lock()
			conn.Write(deviceFrame(f.Seq, cmdDPQuery, map[string]any{"devId": "dev123", "dps": fv.dps}, false))
			fv.mu.Unlock()
		case cmdHeartbeat:
			conn.Write(packFrame(f.Seq, cmdHeartbeat, binary.BigEndian.AppendUint32(nil, 0)))
		case cmdControl:
			dps, _ := parseDPs(f.Payload)
			fv.mu.Lock()
			fv.controls = append(fv.controls, dps)
			if dps["3"] == "smart" {
				dps["5"] = "smart_clean"
			}
			for k, v := range dps {
				fv.dps[k] = v
			}
			fv.mu.Unlock()
			conn.Write(packFrame(f.Seq, cmdControl, binary.BigEndian.AppendUint32(nil, 0)))
			conn.Write(deviceFrame(0, cmdStatus, map[string]any{"devId": "dev123", "dps": dps}, true))
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDeviceAgainstFakeVacuum(t *testing.T) {
	fv := startFakeVacuum(t)
	v := testVacuum("127.0.0.1", "nova")
	dev, err := newDevice(v)
	if err != nil {
		t.Fatal(err)
	}
	// Point the session at the fake's port
	_, port, _ := net.SplitHostPort(fv.ln.Addr().String())
	dev.addr = net.JoinHostPort("127.0.0.1", port)

	vac := &vacuum{cfg: v, dev: dev, track: newTracker("t", func(CleanRun) {})}
	dev.onChange = vac.track.Update

	stop := make(chan struct{})
	defer close(stop)
	go dev.Run(stop)

	waitFor(t, "initial state", func() bool { return dev.State().Status == "charge_done" })
	st := dev.State()
	if st.Battery == nil || *st.Battery != 95 || st.Suction != "gentle" || st.Stats.Area != 3 {
		t.Fatalf("state %+v", st)
	}

	if err := vac.run("bob", "start", "smart"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "cleaning", func() bool { return dev.State().Status == "smart_clean" })
	if c := vac.track.Current(); c == nil || c.StartedBy != "bob" {
		t.Fatalf("current clean %+v", c)
	}
	fv.mu.Lock()
	got := fv.controls[0]
	fv.mu.Unlock()
	if got["3"] != "smart" || got["2"] != true {
		t.Fatalf("vacuum got %v", got)
	}
}
