package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server struct {
		Port int    `yaml:"port"`
		Bind string `yaml:"bind"`
		// AllowDirect lets requests that did not come through the hub act as admin.
		// Only for local testing; leave false in production.
		AllowDirect bool `yaml:"allow_direct"`
	} `yaml:"server"`
	Vacuums  []Vacuum       `yaml:"vacuums"`
	Database DatabaseConfig `yaml:"database"`
}

// DatabaseConfig is where clean history, the command log and schedules are
// kept. When disabled, the vacuum can still be controlled but nothing is
// remembered and schedules are off.
type DatabaseConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Name     string `yaml:"name"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
}

type Vacuum struct {
	ID   string `yaml:"id"`
	Name string `yaml:"name"`
	// Host is the vacuum's IP address. Give it a DHCP reservation so it
	// doesn't move.
	Host string `yaml:"host"`
	// DeviceID and LocalKey come from the Tuya IoT platform (see README).
	// Re-pairing the vacuum in its app changes the local key.
	DeviceID string `yaml:"device_id"`
	LocalKey string `yaml:"local_key"`
	// Only protocol 3.3 is supported; run with -discover to check yours.
	Version string `yaml:"version"`
	// Model picks the defaults for everything below: "eclipse" (iHRV1/iHRV2)
	// or "nova". Anything set here overrides the model's default.
	Model string `yaml:"model"`
	DPs   DPMap  `yaml:"dps"`
	// Values the mode and suction data points accept. The first mode is
	// what Start uses by default.
	Modes   []string `yaml:"modes"`
	Suction []string `yaml:"suction"`
	// Mode values that send it home and that stop it where it is
	DockMode string `yaml:"dock_mode"`
	StopMode string `yaml:"stop_mode"`
	// Data points to send to pause or stop, when the start switch or
	// stop_mode doesn't do it. Resuming then sends the cleaning mode again.
	Pause map[string]any `yaml:"pause"`
	Stop  map[string]any `yaml:"stop"`
	// What the status data point's values mean. Without cleaning_statuses,
	// a clean is worked out from the start switch and the mode instead.
	StatusLabels     map[string]string `yaml:"status_labels"`
	CleaningStatuses []string          `yaml:"cleaning_statuses"`
	PausedStatuses   []string          `yaml:"paused_statuses"`
}

// DPMap says which Tuya data point carries what (run with -dump to see
// yours). 0 means the vacuum doesn't have it.
type DPMap struct {
	Power     int `yaml:"power"`      // bool
	Start     int `yaml:"start"`      // bool: true starts/resumes, false pauses
	Mode      int `yaml:"mode"`       // enum: smart, chargego (dock), ...
	Status    int `yaml:"status"`     // enum, read-only
	Battery   int `yaml:"battery"`    // 0-100
	Locate    int `yaml:"locate"`     // bool: beep
	Suction   int `yaml:"suction"`    // enum
	RawData   int `yaml:"raw_data"`   // Nova: base64 JSON with clean area, time, battery
	Error     int `yaml:"error"`      // fault code or bitmap, 0 = none
	Area      int `yaml:"area"`       // m² cleaned this run
	CleanTime int `yaml:"clean_time"` // minutes cleaned this run
}

// models are the known AutoVacs' data points, from the Tuya cloud's thing
// model for each.
var models = map[string]Vacuum{
	"eclipse": {
		// The start switch (power_go, 33) is ignored over the LAN, so it's
		// left out: setting the mode starts a clean, and directioncontrol
		// (26) "stop" halts it where it is (status 0)
		DPs:      DPMap{Power: 1, Mode: 25, Status: 38, Battery: 39, Suction: 27, Error: 11, Area: 41, CleanTime: 42},
		Modes:    []string{"smart", "wallfollow", "sprial", "single", "mop"}, // "sprial" is spelled that way by the vacuum
		Suction:  []string{"ECO", "normal", "strong"},
		DockMode: "chargego",
		StopMode: "idle",
		Pause:    map[string]any{"26": "stop"},
		Stop:     map[string]any{"26": "stop"},
		// Status is a bare code "0".."8". These were seen on an iHRV2; 7 is
		// what the Smart Life app's pause gives, 0 what stopping gives. The
		// rest haven't been seen yet.
		StatusLabels: map[string]string{
			"0": "Stopped", "1": "Cleaning", "7": "Paused", "4": "Returning to dock", "5": "On dock, charging",
		},
		CleaningStatuses: []string{"1"},
		// A stopped clean can be resumed, so it stays open until it docks
		PausedStatuses: []string{"7", "0"},
	},
	"nova": {
		DPs:      DPMap{Power: 1, Start: 2, Mode: 3, Status: 5, Battery: 6, Locate: 13, Suction: 14, RawData: 102, Error: 110},
		Modes:    []string{"smart", "wall_follow", "spiral", "random", "mop"},
		Suction:  []string{"gentle", "normal", "strong"},
		DockMode: "chargego",
		StopMode: "standby",
		StatusLabels: map[string]string{
			"standby": "Standby", "smart_clean": "Cleaning", "wall_clean": "Cleaning edges",
			"spot_clean": "Spot cleaning", "mop_clean": "Mopping", "cleaning": "Cleaning", "paused": "Paused",
			"goto_charge": "Returning to dock", "charging": "Charging", "charge_done": "Docked, full", "sleep": "Asleep",
		},
		CleaningStatuses: []string{"smart_clean", "wall_clean", "spot_clean", "mop_clean", "cleaning"},
		PausedStatuses:   []string{"paused"},
	},
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8085
	}
	if cfg.Server.Bind == "" {
		cfg.Server.Bind = "127.0.0.1"
	}
	if cfg.Database.Host == "" {
		cfg.Database.Host = "127.0.0.1"
	}
	if cfg.Database.Port == 0 {
		cfg.Database.Port = 3306
	}
	if cfg.Database.Name == "" {
		cfg.Database.Name = "vac_db"
	}
	if cfg.Database.User == "" {
		cfg.Database.User = "vac_app"
	}

	seen := map[string]bool{}
	for i := range cfg.Vacuums {
		v := &cfg.Vacuums[i]
		if v.ID == "" || v.Host == "" || v.DeviceID == "" || v.LocalKey == "" {
			return nil, fmt.Errorf("vacuum %d: id, host, device_id and local_key are required", i+1)
		}
		if seen[v.ID] {
			return nil, fmt.Errorf("duplicate vacuum id %q", v.ID)
		}
		seen[v.ID] = true
		if len(v.LocalKey) != 16 {
			return nil, fmt.Errorf("vacuum %q: local_key must be 16 characters", v.ID)
		}
		if v.Name == "" {
			v.Name = v.ID
		}
		if v.Version == "" {
			v.Version = "3.3"
		}
		if v.Version != "3.3" {
			return nil, fmt.Errorf("vacuum %q: protocol version %s is not supported (only 3.3)", v.ID, v.Version)
		}
		if err := v.applyModel(); err != nil {
			return nil, err
		}
	}
	return &cfg, nil
}

// applyModel fills in what config.yaml left out from the model's defaults.
func (v *Vacuum) applyModel() error {
	if v.Model == "" {
		v.Model = "eclipse"
	}
	m, ok := models[v.Model]
	if !ok {
		return fmt.Errorf("vacuum %q: unknown model %q (eclipse or nova)", v.ID, v.Model)
	}
	if v.DPs == (DPMap{}) {
		v.DPs = m.DPs
	}
	if len(v.Modes) == 0 {
		v.Modes = m.Modes
	}
	if len(v.Suction) == 0 {
		v.Suction = m.Suction
	}
	if v.DockMode == "" {
		v.DockMode = m.DockMode
	}
	if v.StopMode == "" {
		v.StopMode = m.StopMode
	}
	if v.StatusLabels == nil {
		v.StatusLabels = m.StatusLabels
	}
	if v.CleaningStatuses == nil {
		v.CleaningStatuses = m.CleaningStatuses
	}
	if v.Pause == nil {
		v.Pause = m.Pause
	}
	if v.Stop == nil {
		v.Stop = m.Stop
	}
	if v.PausedStatuses == nil {
		v.PausedStatuses = m.PausedStatuses
	}
	return nil
}
