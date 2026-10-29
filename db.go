package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/go-sql-driver/mysql"
)

// CleanRun is one clean, from the vacuum leaving standby until it heads home
// or stops.
type CleanRun struct {
	ID           int64      `json:"id"`
	VacuumID     string     `json:"vacuum"`
	StartedAt    time.Time  `json:"started_at"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	StartedBy    string     `json:"started_by"` // user, "schedule: <name>", or "vacuum" (its own app or button)
	Mode         string     `json:"mode"`
	EndStatus    string     `json:"end_status"`
	Area         *int       `json:"area,omitempty"` // m², as reported by the vacuum
	BatteryStart *int       `json:"battery_start,omitempty"`
	BatteryEnd   *int       `json:"battery_end,omitempty"`
	ErrorCode    int        `json:"error_code"`
}

// CommandEntry is one command sent to the vacuum, by a person or a schedule.
type CommandEntry struct {
	At       time.Time `json:"at"`
	User     string    `json:"user"`
	VacuumID string    `json:"vacuum"`
	Action   string    `json:"action"`
	Detail   string    `json:"detail,omitempty"`
	OK       bool      `json:"ok"`
	Error    string    `json:"error,omitempty"`
}

// Schedule starts a clean at a set local time on chosen weekdays.
type Schedule struct {
	ID        int64      `json:"id"`
	VacuumID  string     `json:"vacuum"`
	Name      string     `json:"name"`
	Days      int        `json:"days"` // bit per time.Weekday: 1 = Sunday ... 64 = Saturday
	At        string     `json:"at"`   // "HH:MM", Pi local time
	Mode      string     `json:"mode"`
	Suction   string     `json:"suction"` // "" = leave as is
	Enabled   bool       `json:"enabled"`
	CreatedBy string     `json:"created_by"`
	LastRun   *time.Time `json:"last_run,omitempty"`
}

var errNotFound = errors.New("not found")

// store keeps history and schedules in MariaDB. Record methods on a nil
// *store (database disabled) do nothing, so callers don't need to check.
type store struct {
	db *sql.DB
}

var db *store

func openStore(c DatabaseConfig) (*store, error) {
	mc := mysql.NewConfig()
	mc.User = c.User
	mc.Passwd = c.Password
	mc.Net = "tcp"
	mc.Addr = fmt.Sprintf("%s:%d", c.Host, c.Port)
	mc.DBName = c.Name
	mc.ParseTime = true
	mc.Loc = time.UTC
	mc.Params = map[string]string{"time_zone": "'+00:00'"}
	// Report rows matched rather than changed, so saving a schedule unchanged
	// isn't mistaken for it not existing
	mc.ClientFoundRows = true

	conn, err := sql.Open("mysql", mc.FormatDSN())
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(4)
	conn.SetConnMaxLifetime(30 * time.Minute)
	if err := conn.Ping(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("connect to %s as %s: %w", mc.Addr, c.User, err)
	}
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS cleans (
			id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
			vacuum_id     VARCHAR(64) NOT NULL,
			started_at    DATETIME NOT NULL,
			ended_at      DATETIME NULL,
			started_by    VARCHAR(96) NOT NULL,
			mode          VARCHAR(32) NOT NULL DEFAULT '',
			end_status    VARCHAR(32) NOT NULL DEFAULT '',
			area_m2       INT NULL,
			battery_start TINYINT UNSIGNED NULL,
			battery_end   TINYINT UNSIGNED NULL,
			error_code    INT NOT NULL DEFAULT 0,
			KEY idx_vacuum_started (vacuum_id, started_at)
		) ENGINE=InnoDB`,
		`CREATE TABLE IF NOT EXISTS command_log (
			id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
			created_at DATETIME NOT NULL,
			user       VARCHAR(96)  NOT NULL,
			vacuum_id  VARCHAR(64)  NOT NULL,
			action     VARCHAR(32)  NOT NULL,
			detail     VARCHAR(255) NOT NULL DEFAULT '',
			ok         BOOLEAN NOT NULL,
			error      VARCHAR(512) NOT NULL DEFAULT '',
			KEY idx_vacuum_created (vacuum_id, created_at)
		) ENGINE=InnoDB`,
		`CREATE TABLE IF NOT EXISTS schedules (
			id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
			vacuum_id   VARCHAR(64) NOT NULL,
			name        VARCHAR(64) NOT NULL,
			days        TINYINT UNSIGNED NOT NULL,
			at_time     CHAR(5) NOT NULL,
			mode        VARCHAR(32) NOT NULL,
			suction     VARCHAR(32) NOT NULL DEFAULT '',
			enabled     BOOLEAN NOT NULL DEFAULT TRUE,
			created_by  VARCHAR(64) NOT NULL,
			last_run_at DATETIME NULL,
			KEY idx_vacuum (vacuum_id)
		) ENGINE=InnoDB`,
	} {
		if _, err := conn.Exec(ddl); err != nil {
			conn.Close()
			return nil, fmt.Errorf("create tables: %w", err)
		}
	}
	return &store{db: conn}, nil
}

// RecordCommand saves e. A failure is logged but never fails the request: the
// command has already been sent.
func (s *store) RecordCommand(e CommandEntry) {
	if s == nil {
		return
	}
	if _, err := s.db.Exec(`INSERT INTO command_log (created_at, user, vacuum_id, action, detail, ok, error)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UTC(), truncate(e.User, 96), truncate(e.VacuumID, 64), truncate(e.Action, 32),
		truncate(e.Detail, 255), e.OK, truncate(e.Error, 512)); err != nil {
		log.Printf("⚠️  could not write command log: %v", err)
	}
}

func (s *store) RecentCommands(vacuumID string, n int) ([]CommandEntry, error) {
	rows, err := s.db.Query(`SELECT created_at, user, vacuum_id, action, detail, ok, error
		FROM command_log WHERE vacuum_id = ? ORDER BY id DESC LIMIT ?`, vacuumID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CommandEntry{}
	for rows.Next() {
		var e CommandEntry
		if err := rows.Scan(&e.At, &e.User, &e.VacuumID, &e.Action, &e.Detail, &e.OK, &e.Error); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RecordClean saves a finished clean.
func (s *store) RecordClean(c CleanRun) {
	if s == nil {
		return
	}
	if _, err := s.db.Exec(`INSERT INTO cleans (vacuum_id, started_at, ended_at, started_by, mode, end_status,
			area_m2, battery_start, battery_end, error_code)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.VacuumID, c.StartedAt.UTC(), utcPtr(c.EndedAt), truncate(c.StartedBy, 96), truncate(c.Mode, 32),
		truncate(c.EndStatus, 32), c.Area, c.BatteryStart, c.BatteryEnd, c.ErrorCode); err != nil {
		log.Printf("⚠️  could not save clean: %v", err)
	}
}

func (s *store) RecentCleans(vacuumID string, n int) ([]CleanRun, error) {
	rows, err := s.db.Query(`SELECT id, vacuum_id, started_at, ended_at, started_by, mode, end_status,
			area_m2, battery_start, battery_end, error_code
		FROM cleans WHERE vacuum_id = ? ORDER BY started_at DESC LIMIT ?`, vacuumID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CleanRun{}
	for rows.Next() {
		var c CleanRun
		var ended sql.NullTime
		var area, bs, be sql.NullInt64
		if err := rows.Scan(&c.ID, &c.VacuumID, &c.StartedAt, &ended, &c.StartedBy, &c.Mode, &c.EndStatus,
			&area, &bs, &be, &c.ErrorCode); err != nil {
			return nil, err
		}
		if ended.Valid {
			c.EndedAt = &ended.Time
		}
		c.Area, c.BatteryStart, c.BatteryEnd = nullInt(area), nullInt(bs), nullInt(be)
		out = append(out, c)
	}
	return out, rows.Err()
}

// Schedules returns every schedule, or one vacuum's when vacuumID is set.
func (s *store) Schedules(vacuumID string) ([]Schedule, error) {
	q := `SELECT id, vacuum_id, name, days, at_time, mode, suction, enabled, created_by, last_run_at FROM schedules`
	args := []any{}
	if vacuumID != "" {
		q += ` WHERE vacuum_id = ?`
		args = append(args, vacuumID)
	}
	rows, err := s.db.Query(q+` ORDER BY at_time, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Schedule{}
	for rows.Next() {
		var sc Schedule
		var last sql.NullTime
		if err := rows.Scan(&sc.ID, &sc.VacuumID, &sc.Name, &sc.Days, &sc.At, &sc.Mode, &sc.Suction,
			&sc.Enabled, &sc.CreatedBy, &last); err != nil {
			return nil, err
		}
		if last.Valid {
			sc.LastRun = &last.Time
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

func (s *store) Schedule(id int64) (Schedule, error) {
	all, err := s.Schedules("")
	if err != nil {
		return Schedule{}, err
	}
	for _, sc := range all {
		if sc.ID == id {
			return sc, nil
		}
	}
	return Schedule{}, errNotFound
}

func (s *store) AddSchedule(sc Schedule) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO schedules (vacuum_id, name, days, at_time, mode, suction, enabled, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sc.VacuumID, sc.Name, sc.Days, sc.At, sc.Mode, sc.Suction, sc.Enabled, truncate(sc.CreatedBy, 64))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *store) UpdateSchedule(sc Schedule) error {
	res, err := s.db.Exec(`UPDATE schedules SET name = ?, days = ?, at_time = ?, mode = ?, suction = ?, enabled = ?
		WHERE id = ?`, sc.Name, sc.Days, sc.At, sc.Mode, sc.Suction, sc.Enabled, sc.ID)
	return checkAffected(res, err)
}

func (s *store) DeleteSchedule(id int64) error {
	res, err := s.db.Exec(`DELETE FROM schedules WHERE id = ?`, id)
	return checkAffected(res, err)
}

func (s *store) MarkScheduleRun(id int64, at time.Time) error {
	_, err := s.db.Exec(`UPDATE schedules SET last_run_at = ? WHERE id = ?`, at.UTC(), id)
	return err
}

func checkAffected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return errNotFound
	}
	return nil
}

func nullInt(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	return ptr(int(n.Int64))
}

func utcPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}
