package registry

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type Rule struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	Project        string `json:"project,omitempty"`
	Source         int    `json:"source,omitempty"`
	Target         int    `json:"target,omitempty"`
	Resolve        string `json:"resolve,omitempty"`
	Proto          string `json:"proto,omitempty"`
	Action         string `json:"action"`
	Redirect       bool   `json:"redirect"`
	TargetExplicit bool   `json:"target_explicit"`
	CreatedAt      int64  `json:"created_at"`
}

type Route struct {
	ID            int64  `json:"id"`
	Host          string `json:"host"`
	ContainerID   string `json:"container_id,omitempty"`
	ContainerName string `json:"container_name,omitempty"`
	Project       string `json:"project,omitempty"`
	Service       string `json:"service,omitempty"`
	Image         string `json:"image,omitempty"`
	Proto         string `json:"proto"` // http | sni | tcp
	SourcePort    int    `json:"source_port,omitempty"`
	GatewayPort   int    `json:"gateway_port,omitempty"`
	TargetAddr    string `json:"target_addr,omitempty"`
	RuleID        int64  `json:"rule_id,omitempty"`
	Status        string `json:"status"` // pending | up | down
	Collision     bool   `json:"collision"`
	Redirect      bool   `json:"redirect"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

type Lease struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Host      string `json:"host"`
	PID       int    `json:"pid"`
	Port      int    `json:"port"`
	Dir       string `json:"dir,omitempty"`
	Command   string `json:"command,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

type Event struct {
	ID      int64          `json:"id"`
	TS      int64          `json:"ts"`
	Level   string         `json:"level"` // info | warn | error
	Kind    string         `json:"kind"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data,omitempty"`
}

type ContainerMeta struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Project   string `json:"project,omitempty"`
	Service   string `json:"service,omitempty"`
	Image     string `json:"image,omitempty"`
	Original  []byte `json:"-"`
	Rewritten []byte `json:"-"`
	Decision  []byte `json:"-"`
	CreatedAt int64  `json:"created_at"`
}

type Registry struct {
	DB  *sql.DB
	Bus *Bus
}

func Open(path string) (*Registry, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	r := &Registry{DB: db, Bus: NewBus()}
	if err := r.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return r, nil
}

func (r *Registry) Close() error {
	if r.Bus != nil {
		r.Bus.Close()
	}
	return r.DB.Close()
}

func (r *Registry) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS rules (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			project TEXT NOT NULL DEFAULT '',
			source_port INTEGER NOT NULL DEFAULT 0,
			target_port INTEGER NOT NULL DEFAULT 0,
			resolve TEXT NOT NULL DEFAULT '',
			proto TEXT NOT NULL DEFAULT '',
			action TEXT NOT NULL,
			redirect INTEGER NOT NULL DEFAULT 1,
			target_explicit INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS routes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			host TEXT NOT NULL,
			container_id TEXT NOT NULL DEFAULT '',
			container_name TEXT NOT NULL DEFAULT '',
			project TEXT NOT NULL DEFAULT '',
			service TEXT NOT NULL DEFAULT '',
			image TEXT NOT NULL DEFAULT '',
			proto TEXT NOT NULL DEFAULT 'http',
			source_port INTEGER NOT NULL DEFAULT 0,
			gateway_port INTEGER NOT NULL DEFAULT 0,
			target_addr TEXT NOT NULL DEFAULT '',
			rule_id INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'pending',
			collision INTEGER NOT NULL DEFAULT 0,
			redirect INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS routes_host_proto ON routes(host, proto)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS routes_container_port ON routes(container_id, proto, source_port) WHERE container_id <> ''`,
		`CREATE INDEX IF NOT EXISTS routes_container ON routes(container_id)`,
		`CREATE TABLE IF NOT EXISTS leases (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			host TEXT NOT NULL UNIQUE,
			pid INTEGER NOT NULL,
			port INTEGER NOT NULL,
			dir TEXT NOT NULL DEFAULT '',
			command TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts INTEGER NOT NULL,
			level TEXT NOT NULL,
			kind TEXT NOT NULL,
			message TEXT NOT NULL,
			data TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS container_meta (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			project TEXT NOT NULL DEFAULT '',
			service TEXT NOT NULL DEFAULT '',
			image TEXT NOT NULL DEFAULT '',
			original_json BLOB NOT NULL,
			rewritten_json BLOB NOT NULL,
			decision_json BLOB NOT NULL,
			created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS container_meta_name ON container_meta(name)`,
		`CREATE TABLE IF NOT EXISTS gateway_ports (
			port INTEGER PRIMARY KEY,
			key TEXT NOT NULL UNIQUE,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
	}
	for _, s := range stmts {
		if _, err := r.DB.Exec(s); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	if err := ensureColumn(r.DB, "rules", "target_explicit", "INTEGER NOT NULL DEFAULT 1"); err != nil {
		return fmt.Errorf("migrate rules.target_explicit: %w", err)
	}
	return nil
}

func ensureColumn(db *sql.DB, table, column, decl string) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			found = true
		}
	}
	if found {
		return nil
	}
	_, err = db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + decl)
	return err
}

func now() int64 { return time.Now().Unix() }

// ---- Rules ----

func (r *Registry) CreateRule(rule *Rule) error {
	if rule.Action == "" {
		return errors.New("rule action is required")
	}
	res, err := r.DB.Exec(`INSERT INTO rules(name,project,source_port,target_port,resolve,proto,action,redirect,target_explicit,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		rule.Name, rule.Project, rule.Source, rule.Target, rule.Resolve, rule.Proto, rule.Action, boolInt(rule.Redirect), boolInt(rule.TargetExplicit), now())
	if err != nil {
		return err
	}
	rule.ID, _ = res.LastInsertId()
	rule.CreatedAt = now()
	return nil
}

func (r *Registry) ListRules() ([]Rule, error) {
	rows, err := r.DB.Query(`SELECT id,name,project,source_port,target_port,resolve,proto,action,redirect,target_explicit,created_at FROM rules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		var x Rule
		var redirect, targetExplicit int
		if err := rows.Scan(&x.ID, &x.Name, &x.Project, &x.Source, &x.Target, &x.Resolve, &x.Proto, &x.Action, &redirect, &targetExplicit, &x.CreatedAt); err != nil {
			return nil, err
		}
		x.Redirect = redirect != 0
		x.TargetExplicit = targetExplicit != 0
		out = append(out, x)
	}
	return out, rows.Err()
}

func (r *Registry) GetRule(id int64) (*Rule, error) {
	var x Rule
	var redirect, targetExplicit int
	err := r.DB.QueryRow(`SELECT id,name,project,source_port,target_port,resolve,proto,action,redirect,target_explicit,created_at FROM rules WHERE id=?`, id).
		Scan(&x.ID, &x.Name, &x.Project, &x.Source, &x.Target, &x.Resolve, &x.Proto, &x.Action, &redirect, &targetExplicit, &x.CreatedAt)
	if err != nil {
		return nil, err
	}
	x.Redirect = redirect != 0
	x.TargetExplicit = targetExplicit != 0
	return &x, nil
}

func (r *Registry) DeleteRule(id int64) (bool, error) {
	res, err := r.DB.Exec(`DELETE FROM rules WHERE id=?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *Registry) ReplaceRules(rules []Rule) error {
	tx, err := r.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM rules`); err != nil {
		return err
	}
	for _, x := range rules {
		if _, err := tx.Exec(`INSERT INTO rules(name,project,source_port,target_port,resolve,proto,action,redirect,target_explicit,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?)`, x.Name, x.Project, x.Source, x.Target, x.Resolve, x.Proto, x.Action, boolInt(x.Redirect), boolInt(x.TargetExplicit), now()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- Routes ----

func (r *Registry) CreateRoute(x *Route) error {
	if x.Status == "" {
		x.Status = "pending"
	}
	ts := now()
	res, err := r.DB.Exec(`INSERT INTO routes(host,container_id,container_name,project,service,image,proto,source_port,gateway_port,target_addr,rule_id,status,collision,redirect,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		x.Host, x.ContainerID, x.ContainerName, x.Project, x.Service, x.Image, x.Proto, x.SourcePort, x.GatewayPort, x.TargetAddr, x.RuleID, x.Status, boolInt(x.Collision), boolInt(x.Redirect), ts, ts)
	if err != nil {
		return err
	}
	x.ID, _ = res.LastInsertId()
	x.CreatedAt, x.UpdatedAt = ts, ts
	return nil
}

// UpsertRoute updates the route identified by (container_id, proto,
// source_port) or inserts a new one. Host collisions surface as errors and
// callers allocate an alternative host first.
func (r *Registry) UpsertRoute(x *Route) error {
	if x.Status == "" {
		x.Status = "pending"
	}
	ts := now()
	var id int64
	err := r.DB.QueryRow(`SELECT id FROM routes WHERE container_id=? AND proto=? AND source_port=? AND container_id<>''`,
		x.ContainerID, x.Proto, x.SourcePort).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return r.CreateRoute(x)
	}
	if err != nil {
		return err
	}
	x.ID = id
	_, err = r.DB.Exec(`UPDATE routes SET host=?,container_name=?,project=?,service=?,image=?,gateway_port=?,target_addr=?,rule_id=?,status=?,collision=?,redirect=?,updated_at=? WHERE id=?`,
		x.Host, x.ContainerName, x.Project, x.Service, x.Image, x.GatewayPort, x.TargetAddr, x.RuleID, x.Status, boolInt(x.Collision), boolInt(x.Redirect), ts, id)
	x.UpdatedAt = ts
	return err
}

func (r *Registry) UpdateRouteStatus(id int64, status, targetAddr string) error {
	_, err := r.DB.Exec(`UPDATE routes SET status=?, target_addr=?, updated_at=? WHERE id=?`, status, targetAddr, now(), id)
	return err
}

func (r *Registry) UpdateRouteSource(id int64, sourcePort int) error {
	_, err := r.DB.Exec(`UPDATE routes SET source_port=?, updated_at=? WHERE id=?`, sourcePort, now(), id)
	return err
}

func (r *Registry) UpdateRouteTarget(id int64, targetAddr string) error {
	_, err := r.DB.Exec(`UPDATE routes SET target_addr=?, updated_at=? WHERE id=?`, targetAddr, now(), id)
	return err
}

func (r *Registry) SetRouteHost(id int64, host string, collision bool) error {
	_, err := r.DB.Exec(`UPDATE routes SET host=?, collision=?, updated_at=? WHERE id=?`, host, boolInt(collision), now(), id)
	return err
}

func (r *Registry) ListRoutes() ([]Route, error) {
	rows, err := r.DB.Query(`SELECT id,host,container_id,container_name,project,service,image,proto,source_port,gateway_port,target_addr,rule_id,status,collision,redirect,created_at,updated_at FROM routes ORDER BY host, proto`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Route
	for rows.Next() {
		var x Route
		var collision, redirect int
		if err := rows.Scan(&x.ID, &x.Host, &x.ContainerID, &x.ContainerName, &x.Project, &x.Service, &x.Image, &x.Proto, &x.SourcePort, &x.GatewayPort, &x.TargetAddr, &x.RuleID, &x.Status, &collision, &redirect, &x.CreatedAt, &x.UpdatedAt); err != nil {
			return nil, err
		}
		x.Collision = collision != 0
		x.Redirect = redirect != 0
		out = append(out, x)
	}
	return out, rows.Err()
}

func (r *Registry) GetRouteByHost(host, proto string) (*Route, error) {
	var x Route
	var collision, redirect int
	err := r.DB.QueryRow(`SELECT id,host,container_id,container_name,project,service,image,proto,source_port,gateway_port,target_addr,rule_id,status,collision,redirect,created_at,updated_at FROM routes WHERE host=? AND proto=?`, host, proto).
		Scan(&x.ID, &x.Host, &x.ContainerID, &x.ContainerName, &x.Project, &x.Service, &x.Image, &x.Proto, &x.SourcePort, &x.GatewayPort, &x.TargetAddr, &x.RuleID, &x.Status, &collision, &redirect, &x.CreatedAt, &x.UpdatedAt)
	if err != nil {
		return nil, err
	}
	x.Collision = collision != 0
	x.Redirect = redirect != 0
	return &x, nil
}

func (r *Registry) GetRoute(id int64) (*Route, error) {
	var x Route
	var collision, redirect int
	err := r.DB.QueryRow(`SELECT id,host,container_id,container_name,project,service,image,proto,source_port,gateway_port,target_addr,rule_id,status,collision,redirect,created_at,updated_at FROM routes WHERE id=?`, id).
		Scan(&x.ID, &x.Host, &x.ContainerID, &x.ContainerName, &x.Project, &x.Service, &x.Image, &x.Proto, &x.SourcePort, &x.GatewayPort, &x.TargetAddr, &x.RuleID, &x.Status, &collision, &redirect, &x.CreatedAt, &x.UpdatedAt)
	if err != nil {
		return nil, err
	}
	x.Collision = collision != 0
	x.Redirect = redirect != 0
	return &x, nil
}

func (r *Registry) RoutesForContainer(containerID string) ([]Route, error) {
	rows, err := r.DB.Query(`SELECT id,host,container_id,container_name,project,service,image,proto,source_port,gateway_port,target_addr,rule_id,status,collision,redirect,created_at,updated_at FROM routes WHERE container_id=?`, containerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Route
	for rows.Next() {
		var x Route
		var collision, redirect int
		if err := rows.Scan(&x.ID, &x.Host, &x.ContainerID, &x.ContainerName, &x.Project, &x.Service, &x.Image, &x.Proto, &x.SourcePort, &x.GatewayPort, &x.TargetAddr, &x.RuleID, &x.Status, &collision, &redirect, &x.CreatedAt, &x.UpdatedAt); err != nil {
			return nil, err
		}
		x.Collision = collision != 0
		x.Redirect = redirect != 0
		out = append(out, x)
	}
	return out, rows.Err()
}

func (r *Registry) DeleteRoute(id int64) (*Route, error) {
	x, err := r.GetRoute(id)
	if err != nil {
		return nil, err
	}
	_, err = r.DB.Exec(`DELETE FROM routes WHERE id=?`, id)
	return x, err
}

// ReassignRoute repoints an existing route at a new container. Docker Compose
// reuses container names across recreations; keeping the route (and hostname)
// stable avoids a spurious collision when destruction events lag.
func (r *Registry) ReassignRoute(id int64, x *Route) error {
	_, err := r.DB.Exec(`UPDATE routes SET container_id=?,container_name=?,project=?,service=?,image=?,proto=?,source_port=?,gateway_port=?,target_addr=?,rule_id=?,status=?,collision=?,redirect=?,updated_at=? WHERE id=?`,
		x.ContainerID, x.ContainerName, x.Project, x.Service, x.Image, x.Proto, x.SourcePort, x.GatewayPort, x.TargetAddr, x.RuleID, x.Status, boolInt(x.Collision), boolInt(x.Redirect), now(), id)
	x.ID = id
	return err
}

func (r *Registry) DeleteRoutesForContainer(containerID string) ([]Route, error) {
	routes, err := r.RoutesForContainer(containerID)
	if err != nil {
		return nil, err
	}
	_, err = r.DB.Exec(`DELETE FROM routes WHERE container_id=?`, containerID)
	return routes, err
}

// ---- Leases ----

func (r *Registry) CreateLease(x *Lease) error {
	if x.Host == "" {
		return errors.New("lease host is required")
	}
	res, err := r.DB.Exec(`INSERT INTO leases(name,host,pid,port,dir,command,created_at) VALUES(?,?,?,?,?,?,?)`,
		x.Name, x.Host, x.PID, x.Port, x.Dir, x.Command, now())
	if err != nil {
		return err
	}
	x.ID, _ = res.LastInsertId()
	x.CreatedAt = now()
	return nil
}

func (r *Registry) ListLeases() ([]Lease, error) {
	rows, err := r.DB.Query(`SELECT id,name,host,pid,port,dir,command,created_at FROM leases ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Lease
	for rows.Next() {
		var x Lease
		if err := rows.Scan(&x.ID, &x.Name, &x.Host, &x.PID, &x.Port, &x.Dir, &x.Command, &x.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (r *Registry) GetLease(id int64) (*Lease, error) {
	var x Lease
	err := r.DB.QueryRow(`SELECT id,name,host,pid,port,dir,command,created_at FROM leases WHERE id=?`, id).
		Scan(&x.ID, &x.Name, &x.Host, &x.PID, &x.Port, &x.Dir, &x.Command, &x.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &x, nil
}

func (r *Registry) DeleteLease(id int64) (*Lease, error) {
	x, err := r.GetLease(id)
	if err != nil {
		return nil, err
	}
	_, err = r.DB.Exec(`DELETE FROM leases WHERE id=?`, id)
	return x, err
}

func (r *Registry) DeleteLeaseByHost(host string) error {
	_, err := r.DB.Exec(`DELETE FROM leases WHERE host=?`, host)
	return err
}

// ---- Events ----

func (r *Registry) AddEvent(level, kind, message string, data map[string]any) Event {
	ev := Event{TS: now(), Level: level, Kind: kind, Message: message, Data: data}
	var payload string
	if data != nil {
		if b, err := json.Marshal(data); err == nil {
			payload = string(b)
		}
	}
	res, err := r.DB.Exec(`INSERT INTO events(ts,level,kind,message,data) VALUES(?,?,?,?,?)`, ev.TS, ev.Level, ev.Kind, ev.Message, payload)
	if err == nil {
		ev.ID, _ = res.LastInsertId()
	}
	if r.Bus != nil {
		r.Bus.Publish(ev)
	}
	return ev
}

func (r *Registry) ListEvents(afterID int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := r.DB.Query(`SELECT id,ts,level,kind,message,data FROM events WHERE id>? ORDER BY id DESC LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var x Event
		var data string
		if err := rows.Scan(&x.ID, &x.TS, &x.Level, &x.Kind, &x.Message, &data); err != nil {
			return nil, err
		}
		if data != "" {
			_ = json.Unmarshal([]byte(data), &x.Data)
		}
		out = append(out, x)
	}
	// oldest first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

func (r *Registry) TrimEvents(keep int) {
	if keep <= 0 {
		keep = 5000
	}
	_, _ = r.DB.Exec(`DELETE FROM events WHERE id NOT IN (SELECT id FROM events ORDER BY id DESC LIMIT ?)`, keep)
}

// ---- Container metadata ----

func (r *Registry) SaveContainerMeta(x *ContainerMeta) error {
	x.CreatedAt = now()
	_, err := r.DB.Exec(`INSERT INTO container_meta(id,name,project,service,image,original_json,rewritten_json,decision_json,created_at)
		VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name,project=excluded.project,service=excluded.service,image=excluded.image,
		original_json=excluded.original_json,rewritten_json=excluded.rewritten_json,decision_json=excluded.decision_json,created_at=excluded.created_at`,
		x.ID, x.Name, x.Project, x.Service, x.Image, x.Original, x.Rewritten, x.Decision, x.CreatedAt)
	return err
}

func (r *Registry) GetContainerMeta(id string) (*ContainerMeta, error) {
	var x ContainerMeta
	err := r.DB.QueryRow(`SELECT id,name,project,service,image,original_json,rewritten_json,decision_json,created_at FROM container_meta WHERE id=?`, id).
		Scan(&x.ID, &x.Name, &x.Project, &x.Service, &x.Image, &x.Original, &x.Rewritten, &x.Decision, &x.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &x, nil
}

func (r *Registry) GetContainerMetaByName(name string) (*ContainerMeta, error) {
	var x ContainerMeta
	err := r.DB.QueryRow(`SELECT id,name,project,service,image,original_json,rewritten_json,decision_json,created_at FROM container_meta WHERE name=? ORDER BY created_at DESC LIMIT 1`, name).
		Scan(&x.ID, &x.Name, &x.Project, &x.Service, &x.Image, &x.Original, &x.Rewritten, &x.Decision, &x.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &x, nil
}

func (r *Registry) ListContainerMeta() ([]ContainerMeta, error) {
	rows, err := r.DB.Query(`SELECT id,name,project,service,image,original_json,rewritten_json,decision_json,created_at FROM container_meta ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContainerMeta
	for rows.Next() {
		var x ContainerMeta
		if err := rows.Scan(&x.ID, &x.Name, &x.Project, &x.Service, &x.Image, &x.Original, &x.Rewritten, &x.Decision, &x.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (r *Registry) DeleteContainerMeta(id string) error {
	_, err := r.DB.Exec(`DELETE FROM container_meta WHERE id=?`, id)
	return err
}

// ---- Gateway ports ----

func (r *Registry) ReserveGatewayPort(port int, key string) error {
	var existingKey string
	err := r.DB.QueryRow(`SELECT key FROM gateway_ports WHERE port=?`, port).Scan(&existingKey)
	if err == nil {
		if existingKey == key {
			return nil
		}
		return fmt.Errorf("gateway port %d already reserved by %s", port, existingKey)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = r.DB.Exec(`INSERT INTO gateway_ports(port,key,created_at) VALUES(?,?,?)`, port, key, now())
	if err != nil {
		return fmt.Errorf("reserve gateway port %d: %w", port, err)
	}
	return nil
}

func (r *Registry) ReleaseGatewayPort(port int, key string) {
	_, _ = r.DB.Exec(`DELETE FROM gateway_ports WHERE port=? AND key=?`, port, key)
}

func (r *Registry) ReleaseGatewayPortByKey(key string) {
	_, _ = r.DB.Exec(`DELETE FROM gateway_ports WHERE key=?`, key)
}

func (r *Registry) GatewayPortFor(key string) (int, bool) {
	var p int
	err := r.DB.QueryRow(`SELECT port FROM gateway_ports WHERE key=?`, key).Scan(&p)
	return p, err == nil
}

func (r *Registry) ListGatewayPorts() (map[int]string, error) {
	rows, err := r.DB.Query(`SELECT port,key FROM gateway_ports`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var p int
		var k string
		if err := rows.Scan(&p, &k); err != nil {
			return nil, err
		}
		out[p] = k
	}
	return out, rows.Err()
}

// ReleaseUnusedGatewayPorts keeps only reservations whose key is in use.
func (r *Registry) ReleaseUnusedGatewayPorts(inUse map[string]bool) error {
	return r.ReleaseUnusedGatewayPortsBefore(inUse, 0)
}

// ReleaseUnusedGatewayPortsBefore also leaves reservations younger than
// before (unix seconds) alone, so a create racing a reconcile is not stolen.
func (r *Registry) ReleaseUnusedGatewayPortsBefore(inUse map[string]bool, before int64) error {
	rows, err := r.DB.Query(`SELECT port,key,created_at FROM gateway_ports`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type kv struct {
		p int
		k string
	}
	var drop []kv
	for rows.Next() {
		var p int
		var k string
		var created int64
		if err := rows.Scan(&p, &k, &created); err != nil {
			return err
		}
		if !inUse[k] && (before == 0 || created < before) {
			drop = append(drop, kv{p, k})
		}
	}
	for _, d := range drop {
		if _, err := r.DB.Exec(`DELETE FROM gateway_ports WHERE port=? AND key=?`, d.p, d.k); err != nil {
			return err
		}
	}
	return nil
}

// ---- Meta ----

func (r *Registry) SetMeta(key, value string) error {
	_, err := r.DB.Exec(`INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (r *Registry) GetMeta(key string) (string, error) {
	var v string
	err := r.DB.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
