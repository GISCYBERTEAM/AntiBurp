// Copyright (C) 2026 GorillaHacker <gorillahacker@yandex.ru> https://t.me/gorillahacker
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"bufio"
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/shirou/gopsutil/v3/process"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/net/proxy"
	_ "modernc.org/sqlite"
)

//go:embed web/templates/*.html web/static/*
var webFS embed.FS

const (
	roleAdmin     = "admin"
	roleModerator = "moderator"
	roleUser      = "user"
)

type App struct {
	db              *sql.DB
	templates       *template.Template
	sessions        *SessionStore
	proxy           *ProxyManager
	interceptor     *Interceptor
	automator       *Automator
	repeater        *RepeaterManager
	ca              *CertAuthority
	serverURL       string
	passwordSalt    string
	proxyConnPool   *proxyConnPool
}

type pooledConn struct {
	conn  net.Conn
	putAt time.Time
}

type proxyConnPool struct {
	mu          sync.Mutex
	conns       map[string][]*pooledConn
	max         int
	dialSem     chan struct{}
	idleTimeout time.Duration
}

type User struct {
	ID       int64
	Username string
	Role     string
}

type Project struct {
	ID         int64
	Name       string
	OwnerID    int64
	OwnerName  string
	AccessList []string
}

func main() {
	db, err := sql.Open("sqlite", "file:antiburp.db?_pragma=foreign_keys(1)")
	if err != nil {
		log.Fatal(err)
	}
	// SQLite is sensitive to concurrent writes. Use a small pool + busy timeout.
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(5)
	if err := initDB(db); err != nil {
		log.Fatal(err)
	}

	funcs := template.FuncMap{
		"join": strings.Join,
	}
	tmpl := template.Must(template.New("base").Funcs(funcs).ParseFS(webFS, "web/templates/*.html"))
	app := &App{
		db:           db,
		templates:    tmpl,
		sessions:     NewSessionStore(db),
		interceptor:  NewInterceptor(),
		automator:    NewAutomator(),
		repeater:     NewRepeaterManager(),
		passwordSalt: "antiburp",
	}
	app.automator.app = app
	ca, err := NewCertAuthority("./certs")
	if err != nil {
		log.Fatal(err)
	}
	app.ca = ca
	app.proxy = NewProxyManager(app)
	app.proxyConnPool = &proxyConnPool{conns: make(map[string][]*pooledConn), max: 150, dialSem: make(chan struct{}, 120), idleTimeout: 2 * time.Second}

	if err := app.ensureDefaultAdmin(); err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	staticFS, err := fs.Sub(webFS, "web/static")
	if err != nil {
		log.Fatal(err)
	}
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	mux.HandleFunc("/login", app.handleLogin)
	mux.HandleFunc("/logout", app.requireAuth(app.handleLogout))
	mux.HandleFunc("/api/metrics", app.handleMetricsAPI)
	mux.HandleFunc("/ca/download", app.handleDownloadCA)
	mux.HandleFunc("/ca/help", app.requireAuth(app.handleCAHelp))
	mux.HandleFunc("/", app.requireAuth(app.handleIndex))
	mux.HandleFunc("/projects", app.requireAuth(app.handleProjects))
	mux.HandleFunc("/projects/create", app.requireAuth(app.handleCreateProject))
	mux.HandleFunc("/projects/delete", app.requireAuth(app.handleDeleteProject))
	mux.HandleFunc("/projects/view", app.requireAuth(app.handleProjectView))

	mux.HandleFunc("/admin/users", app.requireAuth(app.handleAdminUsers))
	mux.HandleFunc("/admin/users/create", app.requireAuth(app.handleAdminCreateUser))
	mux.HandleFunc("/admin/users/role", app.requireAuth(app.handleAdminSetRole))
	mux.HandleFunc("/admin/users/password", app.requireAuth(app.handleAdminSetPassword))
	mux.HandleFunc("/admin/users/delete", app.requireAuth(app.handleAdminDeleteUser))

	mux.HandleFunc("/api/projects/listeners", app.requireAuth(app.handleListenersAPI))
	mux.HandleFunc("/api/projects/history", app.requireAuth(app.handleHistoryAPI))
	mux.HandleFunc("/api/projects/history/detail", app.requireAuth(app.handleHistoryDetailAPI))
	mux.HandleFunc("/api/projects/targets", app.requireAuth(app.handleTargetsAPI))
	mux.HandleFunc("/api/projects/proxy-settings", app.requireAuth(app.handleProjectProxySettingsAPI))
	mux.HandleFunc("/api/projects/modules", app.requireAuth(app.handleModulesAPI))
	mux.HandleFunc("/api/projects/modules/toggle", app.requireAuth(app.handleModuleToggleAPI))
	mux.HandleFunc("/api/projects/modules/header-rules", app.requireAuth(app.handleHeaderRulesAPI))
	mux.HandleFunc("/api/projects/modules/header-rules/delete", app.requireAuth(app.handleHeaderRuleDeleteAPI))
	mux.HandleFunc("/api/projects/export", app.requireAuth(app.handleProjectExportAPI))
	mux.HandleFunc("/api/projects/import", app.requireAuth(app.handleProjectImportAPI))
	mux.HandleFunc("/api/projects/clear", app.requireAuth(app.handleProjectClearAPI))
	mux.HandleFunc("/api/projects/intercept", app.requireAuth(app.handleInterceptAPI))
	mux.HandleFunc("/api/projects/intercept/decision", app.requireAuth(app.handleInterceptDecisionAPI))
	mux.HandleFunc("/api/projects/settings", app.requireAuth(app.handleProjectSettingsAPI))
	mux.HandleFunc("/api/projects/repeater/send", app.requireAuth(app.handleRepeaterSendAPI))
	mux.HandleFunc("/api/projects/repeater/cancel", app.requireAuth(app.handleRepeaterCancelAPI))
	mux.HandleFunc("/api/projects/repeater/history", app.requireAuth(app.handleRepeaterHistoryAPI))
	mux.HandleFunc("/api/projects/repeater/tabs", app.requireAuth(app.handleRepeaterTabsAPI))
	mux.HandleFunc("/api/projects/repeater/tab", app.requireAuth(app.handleRepeaterTabAPI))
	mux.HandleFunc("/api/projects/repeater/tab/draft", app.requireAuth(app.handleRepeaterDraftAPI))
	mux.HandleFunc("/api/projects/repeater/tab/rename", app.requireAuth(app.handleRepeaterRenameAPI))
	mux.HandleFunc("/api/projects/repeater/tab/activate", app.requireAuth(app.handleRepeaterActivateAPI))
	mux.HandleFunc("/api/projects/repeater/tab/delete", app.requireAuth(app.handleRepeaterDeleteAPI))
	mux.HandleFunc("/api/projects/automator/run", app.requireAuth(app.handleAutomatorRunAPI))
	mux.HandleFunc("/api/projects/automator/status", app.requireAuth(app.handleAutomatorStatusAPI))
	mux.HandleFunc("/api/projects/automator/request", app.requireAuth(app.handleAutomatorRequestAPI))
	mux.HandleFunc("/api/projects/automator/stop", app.requireAuth(app.handleAutomatorStopAPI))
	mux.HandleFunc("/api/projects/automator/delete", app.requireAuth(app.handleAutomatorDeleteAPI))
	mux.HandleFunc("/api/projects/automator/tabs", app.requireAuth(app.handleAutomatorTabsAPI))
	mux.HandleFunc("/api/projects/automator/tab", app.requireAuth(app.handleAutomatorTabAPI))
	mux.HandleFunc("/api/projects/automator/tab/draft", app.requireAuth(app.handleAutomatorDraftAPI))
	mux.HandleFunc("/api/projects/automator/tab/rename", app.requireAuth(app.handleAutomatorRenameAPI))
	mux.HandleFunc("/api/projects/automator/tab/activate", app.requireAuth(app.handleAutomatorActivateAPI))
	mux.HandleFunc("/api/projects/automator/tab/delete", app.requireAuth(app.handleAutomatorTabDeleteAPI))
	mux.HandleFunc("/api/projects/automator/runs", app.requireAuth(app.handleAutomatorRunsAPI))

	addr := ":13444"
	app.serverURL = "http://localhost" + addr
	log.Printf("AntiBurp started at %s", app.serverURL)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func initDB(db *sql.DB) error {
	pragmas := []string{
		`PRAGMA foreign_keys = ON;`,
		`PRAGMA journal_mode = WAL;`,
		`PRAGMA busy_timeout = 5000;`,
	}
	for _, stmt := range pragmas {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}

	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			username TEXT UNIQUE NOT NULL,
			password_hash TEXT NOT NULL,
			role TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS sessions (
			token TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL,
			expires_at DATETIME NOT NULL,
			FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS projects (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			owner_id INTEGER NOT NULL,
			created_at DATETIME NOT NULL,
			FOREIGN KEY(owner_id) REFERENCES users(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS project_access (
			project_id INTEGER NOT NULL,
			user_id INTEGER NOT NULL,
			PRIMARY KEY (project_id, user_id),
			FOREIGN KEY(project_id) REFERENCES projects(id) ON DELETE CASCADE,
			FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS project_settings (
			project_id INTEGER PRIMARY KEY,
			encoding_req TEXT NOT NULL,
			encoding_resp TEXT NOT NULL,
			proxy_enabled INTEGER NOT NULL DEFAULT 0,
			proxy_type TEXT NOT NULL DEFAULT '',
			proxy_host TEXT NOT NULL DEFAULT '',
			proxy_port INTEGER NOT NULL DEFAULT 0,
			proxy_user TEXT NOT NULL DEFAULT '',
			proxy_pass TEXT NOT NULL DEFAULT '',
			FOREIGN KEY(project_id) REFERENCES projects(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS project_modules (
			project_id INTEGER NOT NULL,
			module_key TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (project_id, module_key),
			FOREIGN KEY(project_id) REFERENCES projects(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS header_rules (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER NOT NULL,
			header_name TEXT NOT NULL,
			header_value TEXT NOT NULL,
			action TEXT NOT NULL,
			FOREIGN KEY(project_id) REFERENCES projects(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS proxy_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER NOT NULL,
			created_at DATETIME NOT NULL,
			method TEXT NOT NULL,
			url TEXT NOT NULL,
			status INTEGER NOT NULL,
			server_addr TEXT NOT NULL,
			duration_ms INTEGER NOT NULL,
			resp_len INTEGER NOT NULL,
			req_raw TEXT NOT NULL,
			resp_raw TEXT NOT NULL,
			FOREIGN KEY(project_id) REFERENCES projects(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS repeater_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER NOT NULL,
			created_at DATETIME NOT NULL,
			duration_ms INTEGER NOT NULL,
			resp_len INTEGER NOT NULL,
			req_raw TEXT NOT NULL,
			resp_raw TEXT NOT NULL,
			FOREIGN KEY(project_id) REFERENCES projects(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS repeater_tabs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			created_at DATETIME NOT NULL,
			is_active INTEGER NOT NULL DEFAULT 0,
			FOREIGN KEY(project_id) REFERENCES projects(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS repeater_tab_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			tab_id INTEGER NOT NULL,
			created_at DATETIME NOT NULL,
			duration_ms INTEGER NOT NULL,
			resp_len INTEGER NOT NULL,
			req_raw TEXT NOT NULL,
			resp_raw TEXT NOT NULL,
			resp_hex TEXT NOT NULL,
			resp_b64 TEXT NOT NULL,
			FOREIGN KEY(tab_id) REFERENCES repeater_tabs(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS repeater_tab_draft (
			tab_id INTEGER PRIMARY KEY,
			req_raw TEXT NOT NULL,
			resp_raw TEXT NOT NULL,
			resp_hex TEXT NOT NULL,
			resp_b64 TEXT NOT NULL,
			resp_len INTEGER NOT NULL,
			duration_ms INTEGER NOT NULL,
			updated_at DATETIME NOT NULL,
			FOREIGN KEY(tab_id) REFERENCES repeater_tabs(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS automator_runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER NOT NULL,
			tab_id INTEGER,
			created_at DATETIME NOT NULL,
			status TEXT NOT NULL,
			total_requests INTEGER NOT NULL DEFAULT 0,
			completed_requests INTEGER NOT NULL DEFAULT 0,
			config_json TEXT NOT NULL,
			FOREIGN KEY(project_id) REFERENCES projects(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS automator_tabs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			created_at DATETIME NOT NULL,
			is_active INTEGER NOT NULL DEFAULT 0,
			FOREIGN KEY(project_id) REFERENCES projects(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS automator_tab_draft (
			tab_id INTEGER PRIMARY KEY,
			req_raw TEXT NOT NULL,
			config_json TEXT NOT NULL,
			updated_at DATETIME NOT NULL,
			FOREIGN KEY(tab_id) REFERENCES automator_tabs(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS automator_requests (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id INTEGER NOT NULL,
			index_no INTEGER NOT NULL,
			status TEXT NOT NULL,
			status_code INTEGER NOT NULL DEFAULT 0,
			payload_values TEXT NOT NULL DEFAULT '[]',
			duration_ms INTEGER NOT NULL,
			resp_len INTEGER NOT NULL,
			req_raw TEXT NOT NULL,
			resp_raw TEXT NOT NULL,
			FOREIGN KEY(run_id) REFERENCES automator_runs(id) ON DELETE CASCADE
		);`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	// Add new columns for upgrades (ignore if already exists).
	_, _ = db.Exec(`ALTER TABLE automator_runs ADD COLUMN tab_id INTEGER`)
	_, _ = db.Exec(`ALTER TABLE automator_runs ADD COLUMN total_requests INTEGER`)
	_, _ = db.Exec(`ALTER TABLE automator_runs ADD COLUMN completed_requests INTEGER`)
	_, _ = db.Exec(`ALTER TABLE automator_requests ADD COLUMN status_code INTEGER`)
	_, _ = db.Exec(`ALTER TABLE automator_requests ADD COLUMN payload_values TEXT`)
	_, _ = db.Exec(`ALTER TABLE project_settings ADD COLUMN proxy_enabled INTEGER`)
	_, _ = db.Exec(`ALTER TABLE project_settings ADD COLUMN proxy_type TEXT`)
	_, _ = db.Exec(`ALTER TABLE project_settings ADD COLUMN proxy_host TEXT`)
	_, _ = db.Exec(`ALTER TABLE project_settings ADD COLUMN proxy_port INTEGER`)
	_, _ = db.Exec(`ALTER TABLE project_settings ADD COLUMN proxy_user TEXT`)
	_, _ = db.Exec(`ALTER TABLE project_settings ADD COLUMN proxy_pass TEXT`)
	_, _ = db.Exec(`ALTER TABLE proxy_history ADD COLUMN resp_ip TEXT`)
	_, _ = db.Exec(`ALTER TABLE proxy_history ADD COLUMN resp_mime TEXT`)
	_, _ = db.Exec(`ALTER TABLE proxy_history ADD COLUMN listener_port INTEGER`)
	return nil
}

func (app *App) ensureDefaultAdmin() error {
	var count int
	if err := app.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	password := os.Getenv("ABP_ADMIN_PASSWORD")
	if password == "" {
		password = "admin123"
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if _, err := app.db.Exec(`INSERT INTO users (username, password_hash, role) VALUES (?, ?, ?)`, "admin", string(hash), roleAdmin); err != nil {
		return err
	}
	log.Printf("Default admin created: admin / %s", password)
	return nil
}

func (app *App) render(w http.ResponseWriter, name string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	if err := app.templates.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (app *App) handleIndex(w http.ResponseWriter, r *http.Request, user *User) {
	http.Redirect(w, r, "/projects", http.StatusFound)
}

func (app *App) handleDownloadCA(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", "attachment; filename=\"antiburp-ca.pem\"")
	_, _ = w.Write(app.ca.CertPEM())
}

func (app *App) handleCAHelp(w http.ResponseWriter, r *http.Request, user *User) {
	app.render(w, "ca_help.html", map[string]any{
		"User": user,
	})
}

func (app *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		app.render(w, "login.html", nil)
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		username := strings.TrimSpace(r.FormValue("username"))
		password := r.FormValue("password")
		var user User
		var hash string
		err := app.db.QueryRow(`SELECT id, username, role, password_hash FROM users WHERE username = ?`, username).
			Scan(&user.ID, &user.Username, &user.Role, &hash)
		if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
			app.render(w, "login.html", map[string]any{"Error": "Неверный логин или пароль"})
			return
		}
		token, err := app.sessions.Create(user.ID)
		if err != nil {
			http.Error(w, "session error", http.StatusInternalServerError)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     "abp_session",
			Value:    token,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
		http.Redirect(w, r, "/projects", http.StatusFound)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (app *App) handleLogout(w http.ResponseWriter, r *http.Request, user *User) {
	cookie, err := r.Cookie("abp_session")
	if err == nil {
		_ = app.sessions.Delete(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "abp_session",
		Value:    "",
		MaxAge:   -1,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/login", http.StatusFound)
}

func (app *App) requireAuth(next func(http.ResponseWriter, *http.Request, *User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("abp_session")
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		user, err := app.sessions.GetUser(cookie.Value)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next(w, r, user)
	}
}

func (app *App) handleProjects(w http.ResponseWriter, r *http.Request, user *User) {
	projects, err := app.listProjects(user)
	if err != nil {
		http.Error(w, "projects error", http.StatusInternalServerError)
		return
	}
	app.render(w, "projects.html", map[string]any{
		"User":     user,
		"Projects": projects,
	})
}

func (app *App) handleCreateProject(w http.ResponseWriter, r *http.Request, user *User) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	access := strings.TrimSpace(r.FormValue("access"))
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	tx, err := app.db.Begin()
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	res, err := tx.Exec(`INSERT INTO projects (name, owner_id, created_at) VALUES (?, ?, ?)`, name, user.ID, time.Now())
	if err != nil {
		_ = tx.Rollback()
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	projectID, _ := res.LastInsertId()
	if access != "" {
		names := strings.Split(access, ",")
		for _, raw := range names {
			username := strings.TrimSpace(raw)
			if username == "" {
				continue
			}
			var uid int64
			if err := tx.QueryRow(`SELECT id FROM users WHERE username = ?`, username).Scan(&uid); err == nil {
				_, _ = tx.Exec(`INSERT OR IGNORE INTO project_access (project_id, user_id) VALUES (?, ?)`, projectID, uid)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/projects", http.StatusFound)
}

func (app *App) handleDeleteProject(w http.ResponseWriter, r *http.Request, user *User) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	projectID, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	ownerID, err := app.getProjectOwner(projectID)
	if err != nil {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}
	if user.Role != roleAdmin && user.Role != roleModerator && user.ID != ownerID {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	app.proxy.StopProject(projectID)
	if _, err := app.db.Exec(`DELETE FROM projects WHERE id = ?`, projectID); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/projects", http.StatusFound)
}

func (app *App) handleProjectView(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	project, err := app.getProject(projectID)
	if err != nil {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := app.ensureRepeaterDefaultTab(projectID); err != nil {
		log.Printf("repeater default tab error: %v", err)
	}
	if err := app.ensureAutomatorDefaultTab(projectID); err != nil {
		log.Printf("automator default tab error: %v", err)
	}
	if _, _, err := app.getProjectEncodings(projectID); err != nil {
		log.Printf("project settings error: %v", err)
	}
	app.render(w, "project.html", map[string]any{
		"User":    user,
		"Project": project,
	})
}

func (app *App) handleAdminUsers(w http.ResponseWriter, r *http.Request, user *User) {
	if user.Role != roleAdmin && user.Role != roleModerator {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	rows, err := app.db.Query(`SELECT id, username, role FROM users ORDER BY id`)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.Role); err == nil {
			users = append(users, u)
		}
	}
	app.render(w, "admin_users.html", map[string]any{
		"User":  user,
		"Users": users,
	})
}

func (app *App) handleAdminCreateUser(w http.ResponseWriter, r *http.Request, user *User) {
	if user.Role != roleAdmin && user.Role != roleModerator {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	role := r.FormValue("role")
	if username == "" || password == "" || !isValidRole(role) {
		http.Error(w, "invalid input", http.StatusBadRequest)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "hash error", http.StatusInternalServerError)
		return
	}
	if _, err := app.db.Exec(`INSERT INTO users (username, password_hash, role) VALUES (?, ?, ?)`, username, string(hash), role); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin/users", http.StatusFound)
}

func (app *App) handleAdminSetRole(w http.ResponseWriter, r *http.Request, user *User) {
	if user.Role != roleAdmin && user.Role != roleModerator {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	userID, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	role := r.FormValue("role")
	if !isValidRole(role) {
		http.Error(w, "invalid role", http.StatusBadRequest)
		return
	}
	if _, err := app.db.Exec(`UPDATE users SET role = ? WHERE id = ?`, role, userID); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin/users", http.StatusFound)
}

func (app *App) handleAdminSetPassword(w http.ResponseWriter, r *http.Request, user *User) {
	if user.Role != roleAdmin && user.Role != roleModerator {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	userID, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	password := r.FormValue("password")
	if password == "" {
		http.Error(w, "invalid password", http.StatusBadRequest)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "hash error", http.StatusInternalServerError)
		return
	}
	if _, err := app.db.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, string(hash), userID); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin/users", http.StatusFound)
}

func (app *App) handleAdminDeleteUser(w http.ResponseWriter, r *http.Request, user *User) {
	if user.Role != roleAdmin && user.Role != roleModerator {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	userID, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if _, err := app.db.Exec(`DELETE FROM users WHERE id = ?`, userID); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin/users", http.StatusFound)
}

func isValidRole(role string) bool {
	return role == roleAdmin || role == roleModerator || role == roleUser
}

func (app *App) listProjects(user *User) ([]Project, error) {
	var rows *sql.Rows
	var err error
	if user.Role == roleAdmin || user.Role == roleModerator {
		rows, err = app.db.Query(`
			SELECT p.id, p.name, p.owner_id, u.username
			FROM projects p
			JOIN users u ON u.id = p.owner_id
			ORDER BY p.id DESC`)
	} else {
		rows, err = app.db.Query(`
			SELECT DISTINCT p.id, p.name, p.owner_id, u.username
			FROM projects p
			JOIN users u ON u.id = p.owner_id
			LEFT JOIN project_access pa ON pa.project_id = p.id
			WHERE p.owner_id = ? OR pa.user_id = ?
			ORDER BY p.id DESC`, user.ID, user.ID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var projects []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.OwnerID, &p.OwnerName); err != nil {
			continue
		}
		p.AccessList = app.getProjectAccess(p.ID)
		projects = append(projects, p)
	}
	return projects, nil
}

func (app *App) getProjectAccess(projectID int64) []string {
	rows, err := app.db.Query(`
		SELECT u.username
		FROM project_access pa
		JOIN users u ON u.id = pa.user_id
		WHERE pa.project_id = ?
		ORDER BY u.username`, projectID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var list []string
	for rows.Next() {
		var username string
		if err := rows.Scan(&username); err == nil {
			list = append(list, username)
		}
	}
	return list
}

func (app *App) getProjectOwner(projectID int64) (int64, error) {
	var ownerID int64
	err := app.db.QueryRow(`SELECT owner_id FROM projects WHERE id = ?`, projectID).Scan(&ownerID)
	return ownerID, err
}

func (app *App) getProject(projectID int64) (Project, error) {
	var p Project
	err := app.db.QueryRow(`
		SELECT p.id, p.name, p.owner_id, u.username
		FROM projects p
		JOIN users u ON u.id = p.owner_id
		WHERE p.id = ?`, projectID).
		Scan(&p.ID, &p.Name, &p.OwnerID, &p.OwnerName)
	if err != nil {
		return p, err
	}
	p.AccessList = app.getProjectAccess(p.ID)
	return p, nil
}

func (app *App) canAccessProject(user *User, projectID int64) bool {
	if user.Role == roleAdmin || user.Role == roleModerator {
		return true
	}
	var count int
	err := app.db.QueryRow(`
		SELECT COUNT(*)
		FROM projects p
		LEFT JOIN project_access pa ON pa.project_id = p.id
		WHERE p.id = ? AND (p.owner_id = ? OR pa.user_id = ?)`, projectID, user.ID, user.ID).
		Scan(&count)
	return err == nil && count > 0
}

func (app *App) getProjectEncodings(projectID int64) (string, string, error) {
	if err := app.ensureProjectSettings(projectID); err != nil {
		return "", "", err
	}
	var encodingReq, encodingResp string
	err := app.db.QueryRow(`SELECT encoding_req, encoding_resp FROM project_settings WHERE project_id = ?`, projectID).
		Scan(&encodingReq, &encodingResp)
	return encodingReq, encodingResp, err
}

func (app *App) setProjectEncodings(projectID int64, encodingReq, encodingResp string) error {
	_, err := app.db.Exec(`
		INSERT INTO project_settings (project_id, encoding_req, encoding_resp)
		VALUES (?, ?, ?)
		ON CONFLICT(project_id) DO UPDATE SET
			encoding_req = excluded.encoding_req,
			encoding_resp = excluded.encoding_resp
	`, projectID, encodingReq, encodingResp)
	return err
}

type ProxySettings struct {
	Enabled bool   `json:"enabled"`
	Type    string `json:"type"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	User    string `json:"user"`
	Pass    string `json:"pass"`
}

type ModuleInfo struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

type HeaderRule struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Value  string `json:"value"`
	Action string `json:"action"`
}

func (app *App) ensureProjectSettings(projectID int64) error {
	_, err := app.db.Exec(`
		INSERT INTO project_settings (
			project_id, encoding_req, encoding_resp,
			proxy_enabled, proxy_type, proxy_host, proxy_port, proxy_user, proxy_pass
		)
		VALUES (?, ?, ?, 0, '', '', 0, '', '')
		ON CONFLICT(project_id) DO NOTHING
	`, projectID, "utf-8", "universal")
	return err
}

func (app *App) getProjectProxySettings(projectID int64) (ProxySettings, error) {
	if err := app.ensureProjectSettings(projectID); err != nil {
		return ProxySettings{}, err
	}
	var enabled int
	var settings ProxySettings
	err := app.db.QueryRow(`
		SELECT proxy_enabled, proxy_type, proxy_host, proxy_port, proxy_user, proxy_pass
		FROM project_settings WHERE project_id = ?`, projectID).
		Scan(&enabled, &settings.Type, &settings.Host, &settings.Port, &settings.User, &settings.Pass)
	if err != nil {
		return settings, err
	}
	settings.Enabled = enabled == 1
	return settings, nil
}

func (app *App) setProjectProxySettings(projectID int64, settings ProxySettings) error {
	if err := app.ensureProjectSettings(projectID); err != nil {
		return err
	}
	enabled := 0
	if settings.Enabled {
		enabled = 1
	}
	_, err := app.db.Exec(`
		UPDATE project_settings
		SET proxy_enabled = ?, proxy_type = ?, proxy_host = ?, proxy_port = ?, proxy_user = ?, proxy_pass = ?
		WHERE project_id = ?
	`, enabled, settings.Type, settings.Host, settings.Port, settings.User, settings.Pass, projectID)
	return err
}

func (app *App) getModuleEnabled(projectID int64, key string) (bool, error) {
	var enabled int
	err := app.db.QueryRow(`SELECT enabled FROM project_modules WHERE project_id = ? AND module_key = ?`, projectID, key).Scan(&enabled)
	if err == sql.ErrNoRows {
		if _, err := app.db.Exec(`INSERT INTO project_modules (project_id, module_key, enabled) VALUES (?, ?, 0)`, projectID, key); err != nil {
			return false, err
		}
		return false, nil
	}
	return enabled == 1, err
}

func (app *App) setModuleEnabled(projectID int64, key string, enabled bool) error {
	val := 0
	if enabled {
		val = 1
	}
	_, err := app.db.Exec(`
		INSERT INTO project_modules (project_id, module_key, enabled)
		VALUES (?, ?, ?)
		ON CONFLICT(project_id, module_key) DO UPDATE SET enabled = excluded.enabled
	`, projectID, key, val)
	return err
}

func (app *App) getHeaderRules(projectID int64) ([]HeaderRule, error) {
	rows, err := app.db.Query(`SELECT id, header_name, header_value, action FROM header_rules WHERE project_id = ? ORDER BY id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rules []HeaderRule
	for rows.Next() {
		var r HeaderRule
		if err := rows.Scan(&r.ID, &r.Name, &r.Value, &r.Action); err == nil {
			rules = append(rules, r)
		}
	}
	return rules, nil
}

func (app *App) applyHeaderRules(req *http.Request, projectID int64) error {
	enabled, err := app.getModuleEnabled(projectID, "headers")
	if err != nil || !enabled {
		return err
	}
	rules, err := app.getHeaderRules(projectID)
	if err != nil {
		return err
	}
	for _, rule := range rules {
		name := http.CanonicalHeaderKey(strings.TrimSpace(rule.Name))
		switch strings.ToLower(rule.Action) {
		case "add":
			if name == "Host" {
				req.Host = rule.Value
			} else {
				req.Header.Add(name, rule.Value)
			}
		case "replace":
			if name == "Host" {
				req.Host = rule.Value
			} else {
				req.Header.Set(name, rule.Value)
			}
		case "delete":
			if name == "Host" {
				req.Host = ""
			} else {
				req.Header.Del(name)
			}
		}
	}
	return nil
}

func (app *App) ensureRepeaterDefaultTab(projectID int64) error {
	var count int
	if err := app.db.QueryRow(`SELECT COUNT(*) FROM repeater_tabs WHERE project_id = ?`, projectID).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	_, err := app.db.Exec(`INSERT INTO repeater_tabs (project_id, name, created_at, is_active) VALUES (?, ?, ?, 1)`,
		projectID, "Tab 1", time.Now())
	return err
}

func (app *App) createRepeaterTab(projectID int64, name string) error {
	if name == "" {
		name = "Tab"
	}
	_, err := app.db.Exec(`UPDATE repeater_tabs SET is_active = 0 WHERE project_id = ?`, projectID)
	if err != nil {
		return err
	}
	_, err = app.db.Exec(`INSERT INTO repeater_tabs (project_id, name, created_at, is_active) VALUES (?, ?, ?, 1)`,
		projectID, name, time.Now())
	return err
}

func (app *App) setRepeaterActiveTab(projectID, tabID int64) error {
	_, err := app.db.Exec(`UPDATE repeater_tabs SET is_active = 0 WHERE project_id = ?`, projectID)
	if err != nil {
		return err
	}
	_, err = app.db.Exec(`UPDATE repeater_tabs SET is_active = 1 WHERE id = ?`, tabID)
	return err
}

func (app *App) repeaterTabBelongs(projectID, tabID int64) bool {
	var count int
	err := app.db.QueryRow(`SELECT COUNT(*) FROM repeater_tabs WHERE project_id = ? AND id = ?`, projectID, tabID).Scan(&count)
	return err == nil && count > 0
}

func (app *App) deleteRepeaterTab(projectID, tabID int64) error {
	tx, err := app.db.Begin()
	if err != nil {
		return err
	}
	var isActive int
	if err := tx.QueryRow(`SELECT is_active FROM repeater_tabs WHERE id = ?`, tabID).Scan(&isActive); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DELETE FROM repeater_tabs WHERE id = ?`, tabID); err != nil {
		_ = tx.Rollback()
		return err
	}
	if isActive == 1 {
		var nextID int64
		err := tx.QueryRow(`SELECT id FROM repeater_tabs WHERE project_id = ? ORDER BY id DESC LIMIT 1`, projectID).Scan(&nextID)
		if err == nil {
			if _, err := tx.Exec(`UPDATE repeater_tabs SET is_active = 1 WHERE id = ?`, nextID); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
	}
	return tx.Commit()
}

func (app *App) ensureAutomatorDefaultTab(projectID int64) error {
	var count int
	if err := app.db.QueryRow(`SELECT COUNT(*) FROM automator_tabs WHERE project_id = ?`, projectID).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	_, err := app.db.Exec(`INSERT INTO automator_tabs (project_id, name, created_at, is_active) VALUES (?, ?, ?, 1)`,
		projectID, "Tab 1", time.Now())
	return err
}

func (app *App) createAutomatorTab(projectID int64, name string) error {
	if name == "" {
		name = "Tab"
	}
	if _, err := app.db.Exec(`UPDATE automator_tabs SET is_active = 0 WHERE project_id = ?`, projectID); err != nil {
		return err
	}
	_, err := app.db.Exec(`INSERT INTO automator_tabs (project_id, name, created_at, is_active) VALUES (?, ?, ?, 1)`,
		projectID, name, time.Now())
	return err
}

func (app *App) setAutomatorActiveTab(projectID, tabID int64) error {
	if _, err := app.db.Exec(`UPDATE automator_tabs SET is_active = 0 WHERE project_id = ?`, projectID); err != nil {
		return err
	}
	_, err := app.db.Exec(`UPDATE automator_tabs SET is_active = 1 WHERE id = ?`, tabID)
	return err
}

func (app *App) automatorTabBelongs(projectID, tabID int64) bool {
	var count int
	err := app.db.QueryRow(`SELECT COUNT(*) FROM automator_tabs WHERE project_id = ? AND id = ?`, projectID, tabID).Scan(&count)
	return err == nil && count > 0
}

func (app *App) deleteAutomatorTab(projectID, tabID int64) error {
	tx, err := app.db.Begin()
	if err != nil {
		return err
	}
	var isActive int
	if err := tx.QueryRow(`SELECT is_active FROM automator_tabs WHERE id = ?`, tabID).Scan(&isActive); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DELETE FROM automator_tabs WHERE id = ?`, tabID); err != nil {
		_ = tx.Rollback()
		return err
	}
	if isActive == 1 {
		var nextID int64
		err := tx.QueryRow(`SELECT id FROM automator_tabs WHERE project_id = ? ORDER BY id DESC LIMIT 1`, projectID).Scan(&nextID)
		if err == nil {
			if _, err := tx.Exec(`UPDATE automator_tabs SET is_active = 1 WHERE id = ?`, nextID); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
	}
	return tx.Commit()
}

func (app *App) handleListenersAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
		listeners := app.proxy.List(projectID)
		writeJSON(w, listeners)
	case http.MethodPost:
		var payload struct {
			Address string `json:"address"`
			Port    int    `json:"port"`
			MITM    bool   `json:"mitm"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		payload.Address = strings.TrimSpace(payload.Address)
		listener, err := app.proxy.Start(projectID, payload.Address, payload.Port, payload.MITM)
		if err != nil {
			log.Printf("listener start error: project=%d address=%q port=%d mitm=%v err=%v", projectID, payload.Address, payload.Port, payload.MITM, err)
			status := http.StatusBadRequest
			if strings.Contains(strings.ToLower(err.Error()), "address already in use") {
				status = http.StatusConflict
			}
			http.Error(w, err.Error(), status)
			return
		}
		writeJSON(w, listener)
	case http.MethodDelete:
		listenerID, _ := strconv.ParseInt(r.URL.Query().Get("listener_id"), 10, 64)
		if err := app.proxy.Stop(listenerID); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (app *App) handleHistoryAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	rows, err := app.db.Query(`
		SELECT id, created_at, method, url, status, server_addr, duration_ms, resp_len, req_raw, resp_ip, resp_mime, listener_port
		FROM proxy_history
		WHERE project_id = ?
		ORDER BY id DESC
		LIMIT 500`, projectID)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type item struct {
		ID           int64  `json:"id"`
		CreatedAt    string `json:"created_at"`
		Method       string `json:"method"`
		URL          string `json:"url"`
		Status       int    `json:"status"`
		ServerAddr   string `json:"server_addr"`
		DurationMs   int64  `json:"duration_ms"`
		RespLen      int64  `json:"resp_len"`
		RespIP       string `json:"resp_ip"`
		RespMime     string `json:"resp_mime"`
		ListenerPort int    `json:"listener_port"`
		HasGet       bool   `json:"has_get"`
		HasPost      bool   `json:"has_post"`
	}
	var items []item
	for rows.Next() {
		var it item
		var created time.Time
		var reqRaw string
		if err := rows.Scan(&it.ID, &created, &it.Method, &it.URL, &it.Status, &it.ServerAddr, &it.DurationMs, &it.RespLen, &reqRaw, &it.RespIP, &it.RespMime, &it.ListenerPort); err == nil {
			it.CreatedAt = created.Format(time.RFC3339)
			it.HasGet, it.HasPost = detectRequestParams(reqRaw)
			items = append(items, it)
		}
	}
	writeJSON(w, items)
}

func (app *App) handleHistoryDetailAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	historyID, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var reqRaw, respRaw string
	err := app.db.QueryRow(`
		SELECT req_raw, resp_raw
		FROM proxy_history
		WHERE project_id = ? AND id = ?`, projectID, historyID).
		Scan(&reqRaw, &respRaw)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{
		"req_raw":  reqRaw,
		"resp_raw": respRaw,
		"req_hex":  toHex(reqRaw),
		"resp_hex": toHex(respRaw),
		"req_b64":  base64.StdEncoding.EncodeToString([]byte(reqRaw)),
		"resp_b64": base64.StdEncoding.EncodeToString([]byte(respRaw)),
	})
}

func normalizeHost(host string) string {
	if host == "" {
		return host
	}
	h, port, err := net.SplitHostPort(host)
	if err != nil {
		return host
	}
	if h == "" {
		return host
	}
	if port == "443" || port == "80" {
		return h
	}
	return host
}

func (app *App) handleTargetsAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	type requestItem struct {
		ID         int64  `json:"id"`
		Method     string `json:"method"`
		URL        string `json:"url"`
		Status     int    `json:"status"`
		DurationMs int64  `json:"duration_ms"`
		RespLen    int64  `json:"resp_len"`
	}

	rows, err := app.db.Query(`
		SELECT id, method, url, status, duration_ms, resp_len
		FROM proxy_history
		WHERE project_id = ?
		ORDER BY id DESC
		LIMIT 2000`, projectID)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var requests []requestItem
	for rows.Next() {
		var it requestItem
		if err := rows.Scan(&it.ID, &it.Method, &it.URL, &it.Status, &it.DurationMs, &it.RespLen); err == nil {
			requests = append(requests, it)
		}
	}

	type domainItem struct {
		Host   string   `json:"host"`
		HasTLS bool     `json:"has_tls"`
		Paths  []string `json:"paths"`
	}
	type domainData struct {
		hasTLS bool
		paths  map[string]struct{}
	}
	domains := map[string]*domainData{}

	domainRows, err := app.db.Query(`
		SELECT url, server_addr
		FROM proxy_history
		WHERE project_id = ?
		ORDER BY id DESC
		LIMIT 2000`, projectID)
	if err == nil {
		for domainRows.Next() {
			var urlStr, serverAddr string
			if err := domainRows.Scan(&urlStr, &serverAddr); err != nil {
				continue
			}
			u, err := url.Parse(urlStr)
			if err != nil {
				continue
			}
			host := u.Host
			if host == "" {
				if h, _, err := net.SplitHostPort(serverAddr); err == nil {
					host = h
				} else {
					host = serverAddr
				}
				if host == "" {
					continue
				}
			}
			host = normalizeHost(host)
			path := u.Path
			if path == "" {
				path = "/"
			}
			if !strings.HasPrefix(path, "/") {
				path = "/" + path
			}
			entry := domains[host]
			if entry == nil {
				entry = &domainData{paths: map[string]struct{}{}}
				domains[host] = entry
			}
			entry.paths[path] = struct{}{}
			if u.Scheme == "https" || strings.HasPrefix(urlStr, "https://") {
				entry.hasTLS = true
			}
		}
		domainRows.Close()
	}

	var domainItems []domainItem
	for host, entry := range domains {
		paths := make([]string, 0, len(entry.paths))
		for p := range entry.paths {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		domainItems = append(domainItems, domainItem{
			Host:   host,
			HasTLS: entry.hasTLS,
			Paths:  paths,
		})
	}
	sort.Slice(domainItems, func(i, j int) bool {
		return domainItems[i].Host < domainItems[j].Host
	})

	writeJSON(w, map[string]any{
		"domains":  domainItems,
		"requests": requests,
	})
}

type exportProjectSettings struct {
	EncodingReq  string        `json:"encoding_req"`
	EncodingResp string        `json:"encoding_resp"`
	Proxy        ProxySettings `json:"proxy"`
}

type exportProjectData struct {
	Version            int                     `json:"version"`
	ProjectName        string                  `json:"project_name"`
	Settings           exportProjectSettings   `json:"settings"`
	Modules            []ModuleInfo            `json:"modules"`
	HeaderRules        []HeaderRule            `json:"header_rules"`
	ProxyHistory       []proxyHistoryRow       `json:"proxy_history"`
	RepeaterHistory    []repeaterHistoryRow    `json:"repeater_history"`
	RepeaterTabs       []repeaterTabRow        `json:"repeater_tabs"`
	RepeaterTabHistory []repeaterTabHistoryRow `json:"repeater_tab_history"`
	RepeaterTabDrafts  []repeaterTabDraftRow   `json:"repeater_tab_drafts"`
	AutomatorTabs      []automatorTabRow       `json:"automator_tabs"`
	AutomatorTabDrafts []automatorTabDraftRow  `json:"automator_tab_drafts"`
	AutomatorRuns      []automatorRunRow       `json:"automator_runs"`
	AutomatorRequests  []automatorRequestRow   `json:"automator_requests"`
}

type proxyHistoryRow struct {
	ID         int64  `json:"id"`
	CreatedAt  string `json:"created_at"`
	Method     string `json:"method"`
	URL        string `json:"url"`
	Status     int    `json:"status"`
	ServerAddr string `json:"server_addr"`
	DurationMs int64  `json:"duration_ms"`
	RespLen    int64  `json:"resp_len"`
	ReqRaw     string `json:"req_raw"`
	RespRaw    string `json:"resp_raw"`
}

type repeaterHistoryRow struct {
	ID         int64  `json:"id"`
	CreatedAt  string `json:"created_at"`
	DurationMs int64  `json:"duration_ms"`
	RespLen    int64  `json:"resp_len"`
	ReqRaw     string `json:"req_raw"`
	RespRaw    string `json:"resp_raw"`
}

type repeaterTabRow struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	IsActive  int    `json:"is_active"`
}

type repeaterTabHistoryRow struct {
	ID         int64  `json:"id"`
	TabID      int64  `json:"tab_id"`
	CreatedAt  string `json:"created_at"`
	DurationMs int64  `json:"duration_ms"`
	RespLen    int64  `json:"resp_len"`
	ReqRaw     string `json:"req_raw"`
	RespRaw    string `json:"resp_raw"`
	RespHex    string `json:"resp_hex"`
	RespB64    string `json:"resp_b64"`
}

type repeaterTabDraftRow struct {
	TabID      int64  `json:"tab_id"`
	ReqRaw     string `json:"req_raw"`
	RespRaw    string `json:"resp_raw"`
	RespHex    string `json:"resp_hex"`
	RespB64    string `json:"resp_b64"`
	RespLen    int64  `json:"resp_len"`
	DurationMs int64  `json:"duration_ms"`
	UpdatedAt  string `json:"updated_at"`
}

type automatorTabRow struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	IsActive  int    `json:"is_active"`
}

type automatorTabDraftRow struct {
	TabID      int64  `json:"tab_id"`
	ReqRaw     string `json:"req_raw"`
	ConfigJSON string `json:"config_json"`
	UpdatedAt  string `json:"updated_at"`
}

type automatorRunRow struct {
	ID                int64  `json:"id"`
	CreatedAt         string `json:"created_at"`
	Status            string `json:"status"`
	TabID             int64  `json:"tab_id"`
	TotalRequests     int64  `json:"total_requests"`
	CompletedRequests int64  `json:"completed_requests"`
	ConfigJSON        string `json:"config_json"`
}

type automatorRequestRow struct {
	ID            int64  `json:"id"`
	RunID         int64  `json:"run_id"`
	IndexNo       int    `json:"index_no"`
	Status        string `json:"status"`
	StatusCode    int    `json:"status_code"`
	PayloadValues string `json:"payload_values"`
	DurationMs    int64  `json:"duration_ms"`
	RespLen       int64  `json:"resp_len"`
	ReqRaw        string `json:"req_raw"`
	RespRaw       string `json:"resp_raw"`
}

func parseTimeValue(value string) time.Time {
	if value == "" {
		return time.Now()
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t
	}
	return time.Now()
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (app *App) handleProjectExportAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	project, _ := app.getProject(projectID)
	encodingReq, encodingResp, _ := app.getProjectEncodings(projectID)
	proxySettings, _ := app.getProjectProxySettings(projectID)

	data := exportProjectData{
		Version:     1,
		ProjectName: project.Name,
		Settings: exportProjectSettings{
			EncodingReq:  encodingReq,
			EncodingResp: encodingResp,
			Proxy:        proxySettings,
		},
	}
	if enabled, _ := app.getModuleEnabled(projectID, "headers"); true {
		data.Modules = append(data.Modules, ModuleInfo{Key: "headers", Name: "Обработка заголовков", Enabled: enabled})
	}
	if rules, err := app.getHeaderRules(projectID); err == nil {
		data.HeaderRules = rules
	}

	rows, _ := app.db.Query(`SELECT id, created_at, method, url, status, server_addr, duration_ms, resp_len, req_raw, resp_raw FROM proxy_history WHERE project_id = ? ORDER BY id`, projectID)
	for rows != nil && rows.Next() {
		var it proxyHistoryRow
		var created time.Time
		_ = rows.Scan(&it.ID, &created, &it.Method, &it.URL, &it.Status, &it.ServerAddr, &it.DurationMs, &it.RespLen, &it.ReqRaw, &it.RespRaw)
		it.CreatedAt = created.Format(time.RFC3339)
		data.ProxyHistory = append(data.ProxyHistory, it)
	}
	if rows != nil {
		rows.Close()
	}

	rows, _ = app.db.Query(`SELECT id, created_at, duration_ms, resp_len, req_raw, resp_raw FROM repeater_history WHERE project_id = ? ORDER BY id`, projectID)
	for rows != nil && rows.Next() {
		var it repeaterHistoryRow
		var created time.Time
		_ = rows.Scan(&it.ID, &created, &it.DurationMs, &it.RespLen, &it.ReqRaw, &it.RespRaw)
		it.CreatedAt = created.Format(time.RFC3339)
		data.RepeaterHistory = append(data.RepeaterHistory, it)
	}
	if rows != nil {
		rows.Close()
	}

	rows, _ = app.db.Query(`SELECT id, name, created_at, is_active FROM repeater_tabs WHERE project_id = ? ORDER BY id`, projectID)
	for rows != nil && rows.Next() {
		var it repeaterTabRow
		var created time.Time
		_ = rows.Scan(&it.ID, &it.Name, &created, &it.IsActive)
		it.CreatedAt = created.Format(time.RFC3339)
		data.RepeaterTabs = append(data.RepeaterTabs, it)
	}
	if rows != nil {
		rows.Close()
	}

	rows, _ = app.db.Query(`SELECT id, tab_id, created_at, duration_ms, resp_len, req_raw, resp_raw, resp_hex, resp_b64 FROM repeater_tab_history WHERE tab_id IN (SELECT id FROM repeater_tabs WHERE project_id = ?) ORDER BY id`, projectID)
	for rows != nil && rows.Next() {
		var it repeaterTabHistoryRow
		var created time.Time
		_ = rows.Scan(&it.ID, &it.TabID, &created, &it.DurationMs, &it.RespLen, &it.ReqRaw, &it.RespRaw, &it.RespHex, &it.RespB64)
		it.CreatedAt = created.Format(time.RFC3339)
		data.RepeaterTabHistory = append(data.RepeaterTabHistory, it)
	}
	if rows != nil {
		rows.Close()
	}

	rows, _ = app.db.Query(`SELECT tab_id, req_raw, resp_raw, resp_hex, resp_b64, resp_len, duration_ms, updated_at FROM repeater_tab_draft WHERE tab_id IN (SELECT id FROM repeater_tabs WHERE project_id = ?)`, projectID)
	for rows != nil && rows.Next() {
		var it repeaterTabDraftRow
		var updated time.Time
		_ = rows.Scan(&it.TabID, &it.ReqRaw, &it.RespRaw, &it.RespHex, &it.RespB64, &it.RespLen, &it.DurationMs, &updated)
		it.UpdatedAt = updated.Format(time.RFC3339)
		data.RepeaterTabDrafts = append(data.RepeaterTabDrafts, it)
	}
	if rows != nil {
		rows.Close()
	}

	rows, _ = app.db.Query(`SELECT id, name, created_at, is_active FROM automator_tabs WHERE project_id = ? ORDER BY id`, projectID)
	for rows != nil && rows.Next() {
		var it automatorTabRow
		var created time.Time
		_ = rows.Scan(&it.ID, &it.Name, &created, &it.IsActive)
		it.CreatedAt = created.Format(time.RFC3339)
		data.AutomatorTabs = append(data.AutomatorTabs, it)
	}
	if rows != nil {
		rows.Close()
	}

	rows, _ = app.db.Query(`SELECT tab_id, req_raw, config_json, updated_at FROM automator_tab_draft WHERE tab_id IN (SELECT id FROM automator_tabs WHERE project_id = ?)`, projectID)
	for rows != nil && rows.Next() {
		var it automatorTabDraftRow
		var updated time.Time
		_ = rows.Scan(&it.TabID, &it.ReqRaw, &it.ConfigJSON, &updated)
		it.UpdatedAt = updated.Format(time.RFC3339)
		data.AutomatorTabDrafts = append(data.AutomatorTabDrafts, it)
	}
	if rows != nil {
		rows.Close()
	}

	rows, _ = app.db.Query(`SELECT id, created_at, status, tab_id, total_requests, completed_requests, config_json FROM automator_runs WHERE project_id = ? ORDER BY id`, projectID)
	for rows != nil && rows.Next() {
		var it automatorRunRow
		var created time.Time
		_ = rows.Scan(&it.ID, &created, &it.Status, &it.TabID, &it.TotalRequests, &it.CompletedRequests, &it.ConfigJSON)
		it.CreatedAt = created.Format(time.RFC3339)
		data.AutomatorRuns = append(data.AutomatorRuns, it)
	}
	if rows != nil {
		rows.Close()
	}

	rows, _ = app.db.Query(`SELECT id, run_id, index_no, status, status_code, payload_values, duration_ms, resp_len, req_raw, resp_raw FROM automator_requests WHERE run_id IN (SELECT id FROM automator_runs WHERE project_id = ?) ORDER BY id`, projectID)
	for rows != nil && rows.Next() {
		var it automatorRequestRow
		_ = rows.Scan(&it.ID, &it.RunID, &it.IndexNo, &it.Status, &it.StatusCode, &it.PayloadValues, &it.DurationMs, &it.RespLen, &it.ReqRaw, &it.RespRaw)
		data.AutomatorRequests = append(data.AutomatorRequests, it)
	}
	if rows != nil {
		rows.Close()
	}

	body, _ := json.Marshal(data)
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:8])
	filename := fmt.Sprintf("project_%d_%s.antiburp", projectID, hash)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filename))
	w.Write(body)
}

func (app *App) clearProjectData(tx *sql.Tx, projectID int64) error {
	if _, err := tx.Exec(`DELETE FROM proxy_history WHERE project_id = ?`, projectID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM repeater_history WHERE project_id = ?`, projectID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM repeater_tabs WHERE project_id = ?`, projectID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM automator_tabs WHERE project_id = ?`, projectID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM automator_runs WHERE project_id = ?`, projectID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM header_rules WHERE project_id = ?`, projectID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM project_modules WHERE project_id = ?`, projectID); err != nil {
		return err
	}
	return nil
}

func (app *App) handleProjectImportAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "file required", http.StatusBadRequest)
		return
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "file read error", http.StatusBadRequest)
		return
	}
	var data exportProjectData
	if err := json.Unmarshal(content, &data); err != nil {
		http.Error(w, "invalid file", http.StatusBadRequest)
		return
	}

	tx, err := app.db.Begin()
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	if err := app.clearProjectData(tx, projectID); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	if _, err := tx.Exec(`
		INSERT INTO project_settings (project_id, encoding_req, encoding_resp, proxy_enabled, proxy_type, proxy_host, proxy_port, proxy_user, proxy_pass)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project_id) DO UPDATE SET
			encoding_req = excluded.encoding_req,
			encoding_resp = excluded.encoding_resp,
			proxy_enabled = excluded.proxy_enabled,
			proxy_type = excluded.proxy_type,
			proxy_host = excluded.proxy_host,
			proxy_port = excluded.proxy_port,
			proxy_user = excluded.proxy_user,
			proxy_pass = excluded.proxy_pass
	`, projectID, data.Settings.EncodingReq, data.Settings.EncodingResp,
		boolToInt(data.Settings.Proxy.Enabled), data.Settings.Proxy.Type, data.Settings.Proxy.Host, data.Settings.Proxy.Port, data.Settings.Proxy.User, data.Settings.Proxy.Pass); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	for _, mod := range data.Modules {
		_ = app.setModuleEnabled(projectID, mod.Key, mod.Enabled)
	}

	for _, rule := range data.HeaderRules {
		_, _ = tx.Exec(`INSERT INTO header_rules (project_id, header_name, header_value, action) VALUES (?, ?, ?, ?)`,
			projectID, rule.Name, rule.Value, rule.Action)
	}

	for _, it := range data.ProxyHistory {
		_, _ = tx.Exec(`
			INSERT INTO proxy_history (id, project_id, created_at, method, url, status, server_addr, duration_ms, resp_len, req_raw, resp_raw)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			it.ID, projectID, parseTimeValue(it.CreatedAt), it.Method, it.URL, it.Status, it.ServerAddr, it.DurationMs, it.RespLen, it.ReqRaw, it.RespRaw)
	}

	for _, it := range data.RepeaterHistory {
		_, _ = tx.Exec(`
			INSERT INTO repeater_history (id, project_id, created_at, duration_ms, resp_len, req_raw, resp_raw)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			it.ID, projectID, parseTimeValue(it.CreatedAt), it.DurationMs, it.RespLen, it.ReqRaw, it.RespRaw)
	}

	for _, it := range data.RepeaterTabs {
		_, _ = tx.Exec(`
			INSERT INTO repeater_tabs (id, project_id, name, created_at, is_active)
			VALUES (?, ?, ?, ?, ?)`,
			it.ID, projectID, it.Name, parseTimeValue(it.CreatedAt), it.IsActive)
	}

	for _, it := range data.RepeaterTabHistory {
		_, _ = tx.Exec(`
			INSERT INTO repeater_tab_history (id, tab_id, created_at, duration_ms, resp_len, req_raw, resp_raw, resp_hex, resp_b64)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			it.ID, it.TabID, parseTimeValue(it.CreatedAt), it.DurationMs, it.RespLen, it.ReqRaw, it.RespRaw, it.RespHex, it.RespB64)
	}

	for _, it := range data.RepeaterTabDrafts {
		_, _ = tx.Exec(`
			INSERT INTO repeater_tab_draft (tab_id, req_raw, resp_raw, resp_hex, resp_b64, resp_len, duration_ms, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			it.TabID, it.ReqRaw, it.RespRaw, it.RespHex, it.RespB64, it.RespLen, it.DurationMs, parseTimeValue(it.UpdatedAt))
	}

	for _, it := range data.AutomatorTabs {
		_, _ = tx.Exec(`
			INSERT INTO automator_tabs (id, project_id, name, created_at, is_active)
			VALUES (?, ?, ?, ?, ?)`,
			it.ID, projectID, it.Name, parseTimeValue(it.CreatedAt), it.IsActive)
	}

	for _, it := range data.AutomatorTabDrafts {
		_, _ = tx.Exec(`
			INSERT INTO automator_tab_draft (tab_id, req_raw, config_json, updated_at)
			VALUES (?, ?, ?, ?)`,
			it.TabID, it.ReqRaw, it.ConfigJSON, parseTimeValue(it.UpdatedAt))
	}

	for _, it := range data.AutomatorRuns {
		_, _ = tx.Exec(`
			INSERT INTO automator_runs (id, project_id, tab_id, created_at, status, total_requests, completed_requests, config_json)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			it.ID, projectID, it.TabID, parseTimeValue(it.CreatedAt), it.Status, it.TotalRequests, it.CompletedRequests, it.ConfigJSON)
	}

	for _, it := range data.AutomatorRequests {
		_, _ = tx.Exec(`
			INSERT INTO automator_requests (id, run_id, index_no, status, status_code, payload_values, duration_ms, resp_len, req_raw, resp_raw)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			it.ID, it.RunID, it.IndexNo, it.Status, it.StatusCode, it.PayloadValues, it.DurationMs, it.RespLen, it.ReqRaw, it.RespRaw)
	}

	if err := tx.Commit(); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (app *App) handleProjectClearAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tx, err := app.db.Begin()
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if err := app.clearProjectData(tx, projectID); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (app *App) handleInterceptAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
		pending := app.interceptor.List(projectID)
		writeJSON(w, pending)
	case http.MethodPost:
		var payload struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		app.interceptor.SetEnabled(projectID, payload.Enabled)
		writeJSON(w, map[string]any{"enabled": payload.Enabled})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (app *App) handleInterceptDecisionAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		ID     string `json:"id"`
		Allow  bool   `json:"allow"`
		RawReq string `json:"raw_req"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if err := app.interceptor.Decide(projectID, payload.ID, payload.Allow, payload.RawReq); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (app *App) handleProjectSettingsAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
		encodingReq, encodingResp, err := app.getProjectEncodings(projectID)
		if err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{
			"encoding_req":  encodingReq,
			"encoding_resp": encodingResp,
		})
	case http.MethodPost:
		var payload struct {
			EncodingReq  string `json:"encoding_req"`
			EncodingResp string `json:"encoding_resp"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		if payload.EncodingReq == "" || payload.EncodingResp == "" {
			http.Error(w, "encoding required", http.StatusBadRequest)
			return
		}
		if err := app.setProjectEncodings(projectID, payload.EncodingReq, payload.EncodingResp); err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (app *App) handleProjectProxySettingsAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
		settings, err := app.getProjectProxySettings(projectID)
		if err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, settings)
	case http.MethodPost:
		var payload ProxySettings
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		payload.Type = strings.ToLower(strings.TrimSpace(payload.Type))
		payload.Host = strings.TrimSpace(payload.Host)
		if payload.Enabled {
			if payload.Type != "http" && payload.Type != "socks5" {
				http.Error(w, "invalid proxy type", http.StatusBadRequest)
				return
			}
			if payload.Host == "" || payload.Port <= 0 {
				http.Error(w, "proxy host and port required", http.StatusBadRequest)
				return
			}
		}
		if err := app.setProjectProxySettings(projectID, payload); err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (app *App) handleModulesAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	enabled, _ := app.getModuleEnabled(projectID, "headers")
	writeJSON(w, []ModuleInfo{
		{Key: "headers", Name: "Обработка заголовков", Enabled: enabled},
	})
}

func (app *App) handleModuleToggleAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		Key     string `json:"key"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	payload.Key = strings.TrimSpace(payload.Key)
	if payload.Key == "" {
		http.Error(w, "key required", http.StatusBadRequest)
		return
	}
	if err := app.setModuleEnabled(projectID, payload.Key, payload.Enabled); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (app *App) handleHeaderRulesAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
		rules, err := app.getHeaderRules(projectID)
		if err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, rules)
	case http.MethodPost:
		var payload struct {
			Name   string `json:"name"`
			Value  string `json:"value"`
			Action string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		name := strings.TrimSpace(payload.Name)
		action := strings.ToLower(strings.TrimSpace(payload.Action))
		if name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		if action != "add" && action != "replace" && action != "delete" {
			http.Error(w, "invalid action", http.StatusBadRequest)
			return
		}
		_, err := app.db.Exec(`INSERT INTO header_rules (project_id, header_name, header_value, action) VALUES (?, ?, ?, ?)`,
			projectID, name, payload.Value, action)
		if err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (app *App) handleHeaderRuleDeleteAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if _, err := app.db.Exec(`DELETE FROM header_rules WHERE id = ? AND project_id = ?`, payload.ID, projectID); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (app *App) handleMetricsAPI(w http.ResponseWriter, r *http.Request) {
	var user *User
	if cookie, err := r.Cookie("abp_session"); err == nil {
		if u, err := app.sessions.GetUser(cookie.Value); err == nil {
			user = u
		}
	}

	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	projectBytes := int64(0)
	if projectID > 0 && user != nil && app.canAccessProject(user, projectID) {
		projectBytes = app.projectDBSize(projectID)
	}

	dbTotalBytes := int64(0)
	if info, err := os.Stat("antiburp.db"); err == nil {
		dbTotalBytes = info.Size()
	}

	dbFreeBytes := int64(0)
	if usage, err := disk.Usage(filepath.Dir("antiburp.db")); err == nil {
		dbFreeBytes = int64(usage.Free)
	}

	cpuProcess := 0.0
	memProcess := 0.0
	if proc, err := process.NewProcess(int32(os.Getpid())); err == nil {
		if p, err := proc.CPUPercent(); err == nil && runtime.NumCPU() > 0 {
			cpuProcess = p / float64(runtime.NumCPU())
		}
		if m, err := proc.MemoryPercent(); err == nil {
			memProcess = float64(m)
		}
	}

	cpuTotal := 0.0
	if percents, err := cpu.Percent(0, false); err == nil && len(percents) > 0 {
		cpuTotal = percents[0]
	}
	cpuFree := math.Max(0, 100-cpuTotal)

	memFree := 0.0
	if vm, err := mem.VirtualMemory(); err == nil {
		memFree = math.Max(0, 100-vm.UsedPercent)
	}

	writeJSON(w, map[string]any{
		"cpu_process":      cpuProcess,
		"cpu_free":         cpuFree,
		"mem_process":      memProcess,
		"mem_free":         memFree,
		"db_project_bytes": projectBytes,
		"db_total_bytes":   dbTotalBytes,
		"db_free_bytes":    dbFreeBytes,
	})
}

func (app *App) projectDBSize(projectID int64) int64 {
	total := int64(0)
	queries := []struct {
		sql  string
		args []any
	}{
		{`SELECT COALESCE(SUM(LENGTH(req_raw)+LENGTH(resp_raw)+LENGTH(url)+LENGTH(server_addr)),0) FROM proxy_history WHERE project_id = ?`, []any{projectID}},
		{`SELECT COALESCE(SUM(LENGTH(req_raw)+LENGTH(resp_raw)),0) FROM repeater_history WHERE project_id = ?`, []any{projectID}},
		{`SELECT COALESCE(SUM(LENGTH(name)),0) FROM repeater_tabs WHERE project_id = ?`, []any{projectID}},
		{`SELECT COALESCE(SUM(LENGTH(h.req_raw)+LENGTH(h.resp_raw)+LENGTH(h.resp_hex)+LENGTH(h.resp_b64)),0)
		  FROM repeater_tab_history h
		  JOIN repeater_tabs t ON t.id = h.tab_id
		  WHERE t.project_id = ?`, []any{projectID}},
		{`SELECT COALESCE(SUM(LENGTH(d.req_raw)+LENGTH(d.resp_raw)+LENGTH(d.resp_hex)+LENGTH(d.resp_b64)),0)
		  FROM repeater_tab_draft d
		  JOIN repeater_tabs t ON t.id = d.tab_id
		  WHERE t.project_id = ?`, []any{projectID}},
		{`SELECT COALESCE(SUM(LENGTH(name)),0) FROM automator_tabs WHERE project_id = ?`, []any{projectID}},
		{`SELECT COALESCE(SUM(LENGTH(req_raw)+LENGTH(config_json)),0)
		  FROM automator_tab_draft d
		  JOIN automator_tabs t ON t.id = d.tab_id
		  WHERE t.project_id = ?`, []any{projectID}},
		{`SELECT COALESCE(SUM(LENGTH(config_json)),0) FROM automator_runs WHERE project_id = ?`, []any{projectID}},
		{`SELECT COALESCE(SUM(LENGTH(r.req_raw)+LENGTH(r.resp_raw)+LENGTH(r.payload_values)),0)
		  FROM automator_requests r
		  JOIN automator_runs run ON run.id = r.run_id
		  WHERE run.project_id = ?`, []any{projectID}},
		{`SELECT COALESCE(SUM(LENGTH(encoding_req)+LENGTH(encoding_resp)),0) FROM project_settings WHERE project_id = ?`, []any{projectID}},
	}
	for _, q := range queries {
		var value sql.NullInt64
		if err := app.db.QueryRow(q.sql, q.args...).Scan(&value); err == nil && value.Valid {
			total += value.Int64
		}
	}
	return total
}

func (app *App) handleRepeaterSendAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		RawReq    string `json:"raw_req"`
		RequestID string `json:"request_id"`
		TabID     int64  `json:"tab_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	requestID := strings.TrimSpace(payload.RequestID)
	if requestID == "" {
		requestID = fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	ctx, cancel := context.WithCancel(context.Background())
	app.repeater.Register(requestID, cancel)
	defer app.repeater.Done(requestID)
	respRaw, duration, respLen, err := app.sendRawRequestWithContext(ctx, payload.RawReq, projectID)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	if payload.TabID != 0 {
		_, _ = app.db.Exec(`
			INSERT INTO repeater_tab_history (tab_id, created_at, duration_ms, resp_len, req_raw, resp_raw, resp_hex, resp_b64)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			payload.TabID, time.Now(), duration.Milliseconds(), respLen, payload.RawReq, respRaw, toHex(respRaw),
			base64.StdEncoding.EncodeToString([]byte(respRaw)))
	} else {
		if _, err := app.db.Exec(`
			INSERT INTO repeater_history (project_id, created_at, duration_ms, resp_len, req_raw, resp_raw)
			VALUES (?, ?, ?, ?, ?, ?)`, projectID, time.Now(), duration.Milliseconds(), respLen, payload.RawReq, respRaw); err != nil {
			log.Printf("repeater history error: %v", err)
		}
	}
	writeJSON(w, map[string]any{
		"resp_raw":   respRaw,
		"resp_hex":   toHex(respRaw),
		"duration":   duration.Milliseconds(),
		"resp_len":   respLen,
		"req_raw":    payload.RawReq,
		"req_hex":    toHex(payload.RawReq),
		"req_b64":    base64.StdEncoding.EncodeToString([]byte(payload.RawReq)),
		"resp_b64":   base64.StdEncoding.EncodeToString([]byte(respRaw)),
		"timestamp":  time.Now().Format(time.RFC3339),
		"error":      "",
		"request_id": requestID,
	})
}

func (app *App) handleRepeaterTabsAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if err := app.ensureRepeaterDefaultTab(projectID); err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		rows, err := app.db.Query(`
			SELECT id, name, is_active
			FROM repeater_tabs
			WHERE project_id = ?
			ORDER BY id ASC`, projectID)
		if err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		type tab struct {
			ID       int64  `json:"id"`
			Name     string `json:"name"`
			IsActive bool   `json:"is_active"`
		}
		var tabs []tab
		for rows.Next() {
			var t tab
			var isActive int
			if err := rows.Scan(&t.ID, &t.Name, &isActive); err == nil {
				t.IsActive = isActive == 1
				tabs = append(tabs, t)
			}
		}
		if len(tabs) == 0 {
			if err := app.ensureRepeaterDefaultTab(projectID); err != nil {
				http.Error(w, "db error", http.StatusInternalServerError)
				return
			}
			rows2, err := app.db.Query(`
				SELECT id, name, is_active
				FROM repeater_tabs
				WHERE project_id = ?
				ORDER BY id ASC`, projectID)
			if err != nil {
				http.Error(w, "db error", http.StatusInternalServerError)
				return
			}
			defer rows2.Close()
			for rows2.Next() {
				var t tab
				var isActive int
				if err := rows2.Scan(&t.ID, &t.Name, &isActive); err == nil {
					t.IsActive = isActive == 1
					tabs = append(tabs, t)
				}
			}
		}
		writeJSON(w, map[string]any{"tabs": tabs})
	case http.MethodPost:
		var payload struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payload.Name == "" {
			payload.Name = "Tab"
		}
		if err := app.createRepeaterTab(projectID, payload.Name); err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (app *App) handleRepeaterTabAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	tabID, _ := strconv.ParseInt(r.URL.Query().Get("tab_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !app.repeaterTabBelongs(projectID, tabID) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	type historyItem struct {
		ID         int64  `json:"id"`
		CreatedAt  string `json:"created_at"`
		DurationMs int64  `json:"duration_ms"`
		RespLen    int64  `json:"resp_len"`
		ReqRaw     string `json:"req_raw"`
		RespRaw    string `json:"resp_raw"`
		RespHex    string `json:"resp_hex"`
		RespB64    string `json:"resp_b64"`
	}
	rows, err := app.db.Query(`
		SELECT id, created_at, duration_ms, resp_len, req_raw, resp_raw, resp_hex, resp_b64
		FROM repeater_tab_history
		WHERE tab_id = ?
		ORDER BY id DESC
		LIMIT 20`, tabID)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var items []historyItem
	for rows.Next() {
		var it historyItem
		var created time.Time
		if err := rows.Scan(&it.ID, &created, &it.DurationMs, &it.RespLen, &it.ReqRaw, &it.RespRaw, &it.RespHex, &it.RespB64); err == nil {
			it.CreatedAt = created.Format(time.RFC3339)
			items = append(items, it)
		}
	}
	var draft struct {
		ReqRaw    string `json:"req_raw"`
		RespRaw   string `json:"resp_raw"`
		RespHex   string `json:"resp_hex"`
		RespB64   string `json:"resp_b64"`
		RespLen   int64  `json:"resp_len"`
		Duration  int64  `json:"duration_ms"`
		UpdatedAt string `json:"updated_at"`
	}
	var updated time.Time
	err = app.db.QueryRow(`
		SELECT req_raw, resp_raw, resp_hex, resp_b64, resp_len, duration_ms, updated_at
		FROM repeater_tab_draft WHERE tab_id = ?`, tabID).
		Scan(&draft.ReqRaw, &draft.RespRaw, &draft.RespHex, &draft.RespB64, &draft.RespLen, &draft.Duration, &updated)
	if err == nil {
		draft.UpdatedAt = updated.Format(time.RFC3339)
	} else {
		draft = struct {
			ReqRaw    string `json:"req_raw"`
			RespRaw   string `json:"resp_raw"`
			RespHex   string `json:"resp_hex"`
			RespB64   string `json:"resp_b64"`
			RespLen   int64  `json:"resp_len"`
			Duration  int64  `json:"duration_ms"`
			UpdatedAt string `json:"updated_at"`
		}{}
	}
	writeJSON(w, map[string]any{"history": items, "draft": draft})
}

func (app *App) handleRepeaterDraftAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		TabID    int64  `json:"tab_id"`
		ReqRaw   string `json:"req_raw"`
		RespRaw  string `json:"resp_raw"`
		RespHex  string `json:"resp_hex"`
		RespB64  string `json:"resp_b64"`
		RespLen  int64  `json:"resp_len"`
		Duration int64  `json:"duration_ms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if payload.TabID == 0 || !app.repeaterTabBelongs(projectID, payload.TabID) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	_, err := app.db.Exec(`
		INSERT INTO repeater_tab_draft (tab_id, req_raw, resp_raw, resp_hex, resp_b64, resp_len, duration_ms, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(tab_id) DO UPDATE SET
			req_raw = excluded.req_raw,
			resp_raw = excluded.resp_raw,
			resp_hex = excluded.resp_hex,
			resp_b64 = excluded.resp_b64,
			resp_len = excluded.resp_len,
			duration_ms = excluded.duration_ms,
			updated_at = excluded.updated_at
	`, payload.TabID, payload.ReqRaw, payload.RespRaw, payload.RespHex, payload.RespB64, payload.RespLen, payload.Duration, time.Now())
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (app *App) handleRepeaterRenameAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		TabID int64  `json:"tab_id"`
		Name  string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if payload.TabID == 0 || payload.Name == "" || !app.repeaterTabBelongs(projectID, payload.TabID) {
		http.Error(w, "invalid input", http.StatusBadRequest)
		return
	}
	_, err := app.db.Exec(`UPDATE repeater_tabs SET name = ? WHERE id = ?`, payload.Name, payload.TabID)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (app *App) handleRepeaterActivateAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		TabID int64 `json:"tab_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if payload.TabID == 0 || !app.repeaterTabBelongs(projectID, payload.TabID) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err := app.setRepeaterActiveTab(projectID, payload.TabID); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (app *App) handleRepeaterDeleteAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		TabID int64 `json:"tab_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if payload.TabID == 0 || !app.repeaterTabBelongs(projectID, payload.TabID) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err := app.deleteRepeaterTab(projectID, payload.TabID); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (app *App) handleRepeaterCancelAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		RequestID string `json:"request_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if payload.RequestID == "" {
		http.Error(w, "request_id required", http.StatusBadRequest)
		return
	}
	ok := app.repeater.Cancel(payload.RequestID)
	writeJSON(w, map[string]any{"ok": ok})
}

func (app *App) handleRepeaterHistoryAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	rows, err := app.db.Query(`
		SELECT id, created_at, duration_ms, resp_len
		FROM repeater_history
		WHERE project_id = ?
		ORDER BY id DESC
		LIMIT 200`, projectID)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type item struct {
		ID         int64  `json:"id"`
		CreatedAt  string `json:"created_at"`
		DurationMs int64  `json:"duration_ms"`
		RespLen    int64  `json:"resp_len"`
	}
	var items []item
	for rows.Next() {
		var it item
		var created time.Time
		if err := rows.Scan(&it.ID, &created, &it.DurationMs, &it.RespLen); err == nil {
			it.CreatedAt = created.Format(time.RFC3339)
			items = append(items, it)
		}
	}
	writeJSON(w, items)
}

func (app *App) handleAutomatorRunAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var cfg AutomatorConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if cfg.TabID == 0 || !app.automatorTabBelongs(projectID, cfg.TabID) {
		http.Error(w, "tab not found", http.StatusNotFound)
		return
	}
	runID, err := app.automator.Start(app.db, projectID, cfg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"run_id": runID})
}

func (app *App) handleAutomatorStatusAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	runID, _ := strconv.ParseInt(r.URL.Query().Get("run_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var status, configJSON string
	err := app.db.QueryRow(`SELECT status, config_json FROM automator_runs WHERE id = ? AND project_id = ?`, runID, projectID).Scan(&status, &configJSON)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var cfg AutomatorConfig
	_ = json.Unmarshal([]byte(configJSON), &cfg)
	positionsCount := len(cfg.Positions)
	rows, err := app.db.Query(`
		SELECT id, index_no, status_code, payload_values, duration_ms, resp_len
		FROM automator_requests
		WHERE run_id = ?
		ORDER BY id DESC
		LIMIT 500`, runID)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type item struct {
		ID         int64    `json:"id"`
		Index      int      `json:"index"`
		StatusCode int      `json:"status_code"`
		Values     []string `json:"values"`
		DurationMs int64    `json:"duration_ms"`
		RespLen    int64    `json:"resp_len"`
	}
	var items []item
	for rows.Next() {
		var it item
		var valuesJSON string
		if err := rows.Scan(&it.ID, &it.Index, &it.StatusCode, &valuesJSON, &it.DurationMs, &it.RespLen); err == nil {
			if valuesJSON != "" {
				_ = json.Unmarshal([]byte(valuesJSON), &it.Values)
			}
			items = append(items, it)
		}
	}
	writeJSON(w, map[string]any{"status": status, "positions_count": positionsCount, "items": items})
}

func (app *App) handleAutomatorRequestAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	requestID, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var reqRaw, respRaw string
	err := app.db.QueryRow(`
		SELECT ar.req_raw, ar.resp_raw
		FROM automator_requests ar
		JOIN automator_runs r ON r.id = ar.run_id
		WHERE ar.id = ? AND r.project_id = ?`, requestID, projectID).Scan(&reqRaw, &respRaw)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{
		"req_raw":  reqRaw,
		"resp_raw": respRaw,
		"req_hex":  toHex(reqRaw),
		"resp_hex": toHex(respRaw),
		"req_b64":  base64.StdEncoding.EncodeToString([]byte(reqRaw)),
		"resp_b64": base64.StdEncoding.EncodeToString([]byte(respRaw)),
	})
}

func (app *App) handleAutomatorStopAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		RunID int64 `json:"run_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	var status string
	if err := app.db.QueryRow(`SELECT status FROM automator_runs WHERE id = ? AND project_id = ?`, payload.RunID, projectID).Scan(&status); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if status != "running" {
		writeJSON(w, map[string]any{"ok": false})
		return
	}
	ok := app.automator.Stop(payload.RunID)
	writeJSON(w, map[string]any{"ok": ok})
}

func (app *App) handleAutomatorTabDeleteAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		TabID int64 `json:"tab_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if payload.TabID == 0 || !app.automatorTabBelongs(projectID, payload.TabID) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err := app.deleteAutomatorTab(projectID, payload.TabID); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (app *App) handleAutomatorTabsAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if err := app.ensureAutomatorDefaultTab(projectID); err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		rows, err := app.db.Query(`
			SELECT id, name, is_active
			FROM automator_tabs
			WHERE project_id = ?
			ORDER BY id ASC`, projectID)
		if err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		type tab struct {
			ID       int64  `json:"id"`
			Name     string `json:"name"`
			IsActive bool   `json:"is_active"`
		}
		var tabs []tab
		for rows.Next() {
			var t tab
			var isActive int
			if err := rows.Scan(&t.ID, &t.Name, &isActive); err == nil {
				t.IsActive = isActive == 1
				tabs = append(tabs, t)
			}
		}
		if len(tabs) == 0 {
			if err := app.ensureAutomatorDefaultTab(projectID); err != nil {
				http.Error(w, "db error", http.StatusInternalServerError)
				return
			}
			rows2, err := app.db.Query(`
				SELECT id, name, is_active
				FROM automator_tabs
				WHERE project_id = ?
				ORDER BY id ASC`, projectID)
			if err != nil {
				http.Error(w, "db error", http.StatusInternalServerError)
				return
			}
			defer rows2.Close()
			for rows2.Next() {
				var t tab
				var isActive int
				if err := rows2.Scan(&t.ID, &t.Name, &isActive); err == nil {
					t.IsActive = isActive == 1
					tabs = append(tabs, t)
				}
			}
		}
		writeJSON(w, map[string]any{"tabs": tabs})
	case http.MethodPost:
		var payload struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payload.Name == "" {
			payload.Name = "Tab"
		}
		if err := app.createAutomatorTab(projectID, payload.Name); err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (app *App) handleAutomatorTabAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	tabID, _ := strconv.ParseInt(r.URL.Query().Get("tab_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !app.automatorTabBelongs(projectID, tabID) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var draft struct {
		ReqRaw    string `json:"req_raw"`
		Config    string `json:"config_json"`
		UpdatedAt string `json:"updated_at"`
	}
	var updated time.Time
	err := app.db.QueryRow(`
		SELECT req_raw, config_json, updated_at
		FROM automator_tab_draft WHERE tab_id = ?`, tabID).
		Scan(&draft.ReqRaw, &draft.Config, &updated)
	if err == nil {
		draft.UpdatedAt = updated.Format(time.RFC3339)
	} else {
		draft = struct {
			ReqRaw    string `json:"req_raw"`
			Config    string `json:"config_json"`
			UpdatedAt string `json:"updated_at"`
		}{}
	}
	writeJSON(w, map[string]any{"draft": draft})
}

func (app *App) handleAutomatorDraftAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		TabID  int64  `json:"tab_id"`
		ReqRaw string `json:"req_raw"`
		Config string `json:"config_json"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if payload.TabID == 0 || !app.automatorTabBelongs(projectID, payload.TabID) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	_, err := app.db.Exec(`
		INSERT INTO automator_tab_draft (tab_id, req_raw, config_json, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(tab_id) DO UPDATE SET
			req_raw = excluded.req_raw,
			config_json = excluded.config_json,
			updated_at = excluded.updated_at
	`, payload.TabID, payload.ReqRaw, payload.Config, time.Now())
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (app *App) handleAutomatorRenameAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		TabID int64  `json:"tab_id"`
		Name  string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if payload.TabID == 0 || payload.Name == "" || !app.automatorTabBelongs(projectID, payload.TabID) {
		http.Error(w, "invalid input", http.StatusBadRequest)
		return
	}
	if _, err := app.db.Exec(`UPDATE automator_tabs SET name = ? WHERE id = ?`, payload.Name, payload.TabID); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (app *App) handleAutomatorActivateAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		TabID int64 `json:"tab_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if payload.TabID == 0 || !app.automatorTabBelongs(projectID, payload.TabID) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err := app.setAutomatorActiveTab(projectID, payload.TabID); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (app *App) handleAutomatorDeleteAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		RunID int64 `json:"run_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if payload.RunID == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var status string
	if err := app.db.QueryRow(`SELECT status FROM automator_runs WHERE id = ? AND project_id = ?`, payload.RunID, projectID).Scan(&status); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if status == "running" {
		http.Error(w, "run is active", http.StatusConflict)
		return
	}
	if _, err := app.db.Exec(`DELETE FROM automator_runs WHERE id = ? AND project_id = ?`, payload.RunID, projectID); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (app *App) handleAutomatorRunsAPI(w http.ResponseWriter, r *http.Request, user *User) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	tabID, _ := strconv.ParseInt(r.URL.Query().Get("tab_id"), 10, 64)
	if !app.canAccessProject(user, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if tabID == 0 || !app.automatorTabBelongs(projectID, tabID) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	rows, err := app.db.Query(`
		SELECT id, created_at, status, COALESCE(total_requests, 0), COALESCE(completed_requests, 0)
		FROM automator_runs
		WHERE project_id = ? AND tab_id = ?
		ORDER BY id DESC
		LIMIT 200`, projectID, tabID)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type runItem struct {
		ID        int64  `json:"id"`
		CreatedAt string `json:"created_at"`
		Status    string `json:"status"`
		Total     int64  `json:"total"`
		Completed int64  `json:"completed"`
	}
	var runs []runItem
	for rows.Next() {
		var it runItem
		var created time.Time
		if err := rows.Scan(&it.ID, &created, &it.Status, &it.Total, &it.Completed); err == nil {
			it.CreatedAt = created.Format(time.RFC3339)
			runs = append(runs, it)
		}
	}
	writeJSON(w, map[string]any{"runs": runs})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

func toHex(raw string) string {
	return hex.Dump([]byte(raw))
}

// Session store

type SessionStore struct {
	db *sql.DB
}

func NewSessionStore(db *sql.DB) *SessionStore {
	return &SessionStore{db: db}
}

func (s *SessionStore) Create(userID int64) (string, error) {
	token, err := randomToken(32)
	if err != nil {
		return "", err
	}
	expires := time.Now().Add(24 * time.Hour)
	_, err = s.db.Exec(`INSERT INTO sessions (token, user_id, expires_at) VALUES (?, ?, ?)`, token, userID, expires)
	return token, err
}

func (s *SessionStore) GetUser(token string) (*User, error) {
	var u User
	var expires time.Time
	err := s.db.QueryRow(`
		SELECT u.id, u.username, u.role, s.expires_at
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token = ?`, token).Scan(&u.ID, &u.Username, &u.Role, &expires)
	if err != nil {
		return nil, err
	}
	if time.Now().After(expires) {
		_ = s.Delete(token)
		return nil, fmt.Errorf("session expired")
	}
	return &u, nil
}

func (s *SessionStore) Delete(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
	return err
}

func randomToken(length int) (string, error) {
	buf := make([]byte, length)
	if _, err := crand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// Repeater manager

type RepeaterManager struct {
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

func NewRepeaterManager() *RepeaterManager {
	return &RepeaterManager{
		cancels: make(map[string]context.CancelFunc),
	}
}

func (rm *RepeaterManager) Register(id string, cancel context.CancelFunc) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rm.cancels[id] = cancel
}

func (rm *RepeaterManager) Cancel(id string) bool {
	rm.mu.Lock()
	cancel, ok := rm.cancels[id]
	if ok {
		delete(rm.cancels, id)
	}
	rm.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

func (rm *RepeaterManager) Done(id string) {
	rm.mu.Lock()
	delete(rm.cancels, id)
	rm.mu.Unlock()
}

// Certificate authority

type CertAuthority struct {
	cert     *x509.Certificate
	key      *rsa.PrivateKey
	certPEM  []byte
	keyPEM   []byte
	cache    map[string]*tls.Certificate
	mu       sync.Mutex
	certPath string
	keyPath  string
}

func NewCertAuthority(dir string) (*CertAuthority, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	certPath := filepath.Join(dir, "antiburp-ca.pem")
	keyPath := filepath.Join(dir, "antiburp-ca.key")
	ca := &CertAuthority{
		cache:    make(map[string]*tls.Certificate),
		certPath: certPath,
		keyPath:  keyPath,
	}
	if err := ca.loadOrCreate(); err != nil {
		return nil, err
	}
	return ca, nil
}

func (ca *CertAuthority) loadOrCreate() error {
	certPEM, certErr := os.ReadFile(ca.certPath)
	keyPEM, keyErr := os.ReadFile(ca.keyPath)
	if certErr == nil && keyErr == nil {
		cert, key, err := parseCAPEM(certPEM, keyPEM)
		if err == nil {
			ca.cert = cert
			ca.key = key
			ca.certPEM = certPEM
			ca.keyPEM = keyPEM
			return nil
		}
	}

	key, err := rsa.GenerateKey(crand.Reader, 2048)
	if err != nil {
		return err
	}
	serial, _ := crand.Int(crand.Reader, big.NewInt(1<<62))
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "AntiBurp CA",
			Organization: []string{"AntiBurp"},
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(5 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(crand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(ca.certPath, certPEM, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(ca.keyPath, keyPEM, 0o600); err != nil {
		return err
	}
	ca.cert = cert
	ca.key = key
	ca.certPEM = certPEM
	ca.keyPEM = keyPEM
	return nil
}

func parseCAPEM(certPEM, keyPEM []byte) (*x509.Certificate, *rsa.PrivateKey, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, nil, fmt.Errorf("invalid cert pem")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, err
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("invalid key pem")
	}
	key, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

func (ca *CertAuthority) CertPEM() []byte {
	return ca.certPEM
}

func (ca *CertAuthority) GetCertificate(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
	host := chi.ServerName
	if host == "" {
		host = "localhost"
	}
	return ca.getForHost(host)
}

func (ca *CertAuthority) getForHost(host string) (*tls.Certificate, error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if cert, ok := ca.cache[host]; ok {
		return cert, nil
	}
	key, err := rsa.GenerateKey(crand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	serial, _ := crand.Int(crand.Reader, big.NewInt(1<<62))
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: host,
		},
		NotBefore:   time.Now().Add(-time.Hour),
		NotAfter:    time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{host},
	}
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip}
		template.DNSNames = nil
	}
	der, err := x509.CreateCertificate(crand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	ca.cache[host] = &pair
	return &pair, nil
}

// Proxy manager

type ProxyManager struct {
	app       *App
	mu        sync.Mutex
	listeners map[int64]*ProxyListener
	nextID    int64
}

type ProxyListener struct {
	ID        int64  `json:"id"`
	ProjectID int64  `json:"project_id"`
	Address   string `json:"address"`
	Port      int    `json:"port"`
	Active    bool   `json:"active"`
	MITM      bool   `json:"mitm"`
	server    *http.Server
	listener  net.Listener
}

func NewProxyManager(app *App) *ProxyManager {
	return &ProxyManager{
		app:       app,
		listeners: make(map[int64]*ProxyListener),
	}
}

func (pm *ProxyManager) Start(projectID int64, address string, port int, mitm bool) (*ProxyListener, error) {
	if address == "" {
		address = "0.0.0.0"
	}
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("invalid port")
	}
	pm.mu.Lock()
	pm.nextID++
	id := pm.nextID
	listener := &ProxyListener{
		ID:        id,
		ProjectID: projectID,
		Address:   address,
		Port:      port,
		Active:    true,
		MITM:      mitm,
	}
	pm.listeners[id] = listener
	pm.mu.Unlock()

	server := &http.Server{
		Handler: pm.proxyHandler(projectID, listener),
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", address, port))
	if err != nil {
		pm.mu.Lock()
		delete(pm.listeners, id)
		pm.mu.Unlock()
		return nil, err
	}
	listener.server = server
	listener.listener = ln
	go func() {
		if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("proxy listener error: %v", err)
		}
		pm.mu.Lock()
		if l, ok := pm.listeners[id]; ok {
			l.Active = false
		}
		pm.mu.Unlock()
	}()
	return listener, nil
}

func (pm *ProxyManager) Stop(listenerID int64) error {
	pm.mu.Lock()
	listener, ok := pm.listeners[listenerID]
	if !ok {
		pm.mu.Unlock()
		return fmt.Errorf("listener not found")
	}
	delete(pm.listeners, listenerID)
	pm.mu.Unlock()
	listener.Active = false
	if listener.listener != nil {
		_ = listener.listener.Close()
	}
	if listener.server != nil {
		_ = listener.server.Close()
	}
	return nil
}

func (pm *ProxyManager) StopProject(projectID int64) {
	var targets []*ProxyListener
	pm.mu.Lock()
	for id, listener := range pm.listeners {
		if listener.ProjectID == projectID {
			delete(pm.listeners, id)
			listener.Active = false
			targets = append(targets, listener)
		}
	}
	pm.mu.Unlock()
	for _, listener := range targets {
		if listener.listener != nil {
			_ = listener.listener.Close()
		}
		if listener.server != nil {
			_ = listener.server.Close()
		}
	}
}

func (pm *ProxyManager) List(projectID int64) []*ProxyListener {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	var out []*ProxyListener
	for _, l := range pm.listeners {
		if l.ProjectID == projectID {
			out = append(out, l)
		}
	}
	return out
}

func (pm *ProxyManager) proxyHandler(projectID int64, listener *ProxyListener) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			pm.handleConnect(listener, w, r, projectID)
			return
		}
		pm.handleHTTP(listener, w, r, projectID)
	})
}

func (pm *ProxyManager) handleConnect(listener *ProxyListener, w http.ResponseWriter, r *http.Request, projectID int64) {
	if listener.MITM {
		pm.handleConnectMITM(listener, w, r, projectID)
		return
	}
	destConn, err := pm.app.dialThroughProxy(projectID, "tcp", r.Host, 30*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	_, _ = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	go transfer(destConn, clientConn)
	go transfer(clientConn, destConn)

	reqRaw := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", r.Host, r.Host)
	respRaw := "HTTP/1.1 200 Connection Established\r\n\r\n"
	respIP := ""
	if destConn != nil {
		respIP = destConn.RemoteAddr().String()
	}
	_, _ = pm.app.db.Exec(`
		INSERT INTO proxy_history (project_id, created_at, method, url, status, server_addr, duration_ms, resp_len, req_raw, resp_raw, resp_ip, resp_mime, listener_port)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		projectID, time.Now(), "CONNECT", r.Host, 200, r.Host, 0, len(respRaw), reqRaw, respRaw, respIP, "", listener.Port)
}

func (pm *ProxyManager) handleHTTP(listener *ProxyListener, w http.ResponseWriter, r *http.Request, projectID int64) {
	start := time.Now()
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	reqDump, _ := httputil.DumpRequest(r, true)
	reqRaw := string(reqDump)

	if pm.app.interceptor.IsEnabled(projectID) {
		decision, ok := pm.app.interceptor.Intercept(projectID, reqRaw)
		if !ok || !decision.Allow {
			http.Error(w, "request dropped", http.StatusForbidden)
			return
		}
		reqRaw = decision.Raw
	}

	parsedReq, err := parseRawRequest(reqRaw)
	if err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	respRaw, _, respLen, respMime, respIP, err := pm.app.sendRequestInfo(parsedReq, projectID)
	if err != nil {
		writeProxyErrorPage(w, err)
		return
	}
	resp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(respRaw)), parsedReq)
	if err != nil {
		http.Error(w, "bad response", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, v := range resp.Header {
		for _, val := range v {
			w.Header().Add(k, val)
		}
	}
	w.WriteHeader(resp.StatusCode)
	respBody, _ := io.ReadAll(resp.Body)
	_, _ = w.Write(respBody)

	durationMs := time.Since(start).Milliseconds()
	serverAddr := parsedReq.Host
	if parsedReq.URL != nil && parsedReq.URL.Host != "" {
		serverAddr = parsedReq.URL.Host
	}
	if respIP == "" {
		respIP = serverAddr
	}
	_, _ = pm.app.db.Exec(`
		INSERT INTO proxy_history (project_id, created_at, method, url, status, server_addr, duration_ms, resp_len, req_raw, resp_raw, resp_ip, resp_mime, listener_port)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		projectID, time.Now(), parsedReq.Method, parsedReq.URL.String(), resp.StatusCode, serverAddr, durationMs, respLen, reqRaw, respRaw, respIP, respMime, listener.Port)
}

func (pm *ProxyManager) handleConnectMITM(listener *ProxyListener, w http.ResponseWriter, r *http.Request, projectID int64) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	_, _ = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	host := r.Host
	if strings.Contains(host, ":") {
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
	}
	tlsConn := tls.Server(clientConn, &tls.Config{
		GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if chi.ServerName != "" {
				return pm.app.ca.getForHost(chi.ServerName)
			}
			return pm.app.ca.getForHost(host)
		},
	})
	if err := tlsConn.Handshake(); err != nil {
		_ = tlsConn.Close()
		return
	}
	pm.handleTLSRequests(listener, tlsConn, projectID)
}

func (pm *ProxyManager) handleTLSRequests(listener *ProxyListener, conn net.Conn, projectID int64) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	for {
		req, err := http.ReadRequest(reader)
		if err != nil {
			if err == io.EOF {
				return
			}
			return
		}
		req.URL.Scheme = "https"
		if req.URL.Host == "" {
			req.URL.Host = req.Host
		}
		req.RequestURI = ""
		body, _ := io.ReadAll(req.Body)
		req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(body))
		reqDump, _ := httputil.DumpRequest(req, true)
		reqRaw := string(reqDump)

		if pm.app.interceptor.IsEnabled(projectID) {
			decision, ok := pm.app.interceptor.Intercept(projectID, reqRaw)
			if !ok || !decision.Allow {
				writeRawResponse(conn, 403, "request dropped")
				continue
			}
			reqRaw = decision.Raw
		}

		parsedReq, err := parseRawRequest(reqRaw)
		if err != nil {
			writeRawResponse(conn, 400, "bad request")
			continue
		}
		parsedReq.URL.Scheme = "https"

		start := time.Now()
		respRaw, _, respLen, respMime, respIP, err := pm.app.sendRequestInfo(parsedReq, projectID)
		if err != nil {
			writeRawHTMLResponse(conn, 502, proxyErrorHTML())
			continue
		}
		_, _ = io.WriteString(conn, respRaw)

		durationMs := time.Since(start).Milliseconds()
		serverAddr := parsedReq.Host
		if parsedReq.URL != nil && parsedReq.URL.Host != "" {
			serverAddr = parsedReq.URL.Host
		}
		if respIP == "" {
			respIP = serverAddr
		}
		statusCode := extractStatus(respRaw)
		_, _ = pm.app.db.Exec(`
			INSERT INTO proxy_history (project_id, created_at, method, url, status, server_addr, duration_ms, resp_len, req_raw, resp_raw, resp_ip, resp_mime, listener_port)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			projectID, time.Now(), parsedReq.Method, parsedReq.URL.String(), statusCode, serverAddr, durationMs, respLen, reqRaw, respRaw, respIP, respMime, listener.Port)
	}
}

func writeRawResponse(w io.Writer, status int, body string) {
	resp := fmt.Sprintf("HTTP/1.1 %d %s\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n\r\n%s",
		status, http.StatusText(status), len(body), body)
	_, _ = io.WriteString(w, resp)
}

func writeRawHTMLResponse(w io.Writer, status int, htmlBody string) {
	resp := fmt.Sprintf("HTTP/1.1 %d %s\r\nContent-Type: text/html; charset=utf-8\r\nContent-Length: %d\r\n\r\n%s",
		status, http.StatusText(status), len(htmlBody), htmlBody)
	_, _ = io.WriteString(w, resp)
}

func proxyErrorHTML() string {
	return `<!DOCTYPE html>
<html lang="ru">
<head>
  <meta charset="UTF-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>AntiBurp — Ошибка прокси</title>
  <style>
    body{margin:0;background:#0f172a;color:#e2e8f0;font-family:Segoe UI,Arial,sans-serif;display:flex;align-items:center;justify-content:center;min-height:100vh}
    .card{background:#111827;border:1px solid #1f2937;border-radius:12px;padding:24px;max-width:560px;width:92%}
    h1{margin:0 0 8px;font-size:20px}
    p{margin:0 0 12px;color:#94a3b8}
    .muted{color:#94a3b8;font-size:12px}
  </style>
</head>
<body>
  <div class="card">
    <h1>AntiBurp. Ошибка подключения</h1>
    <p>Проверьте текущие параметры подключения к SOCKS/HTTP-прокси</p>
    <div class="muted">Код ошибки: 502 Bad Gateway</div>
  </div>
</body>
</html>`
}

func writeProxyErrorPage(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = w.Write([]byte(proxyErrorHTML()))
}

func extractStatus(respRaw string) int {
	lines := strings.SplitN(respRaw, "\r\n", 2)
	if len(lines) == 0 {
		return 0
	}
	parts := strings.Split(lines[0], " ")
	if len(parts) < 2 {
		return 0
	}
	code, _ := strconv.Atoi(parts[1])
	return code
}

func transfer(dst io.WriteCloser, src io.ReadCloser) {
	defer dst.Close()
	defer src.Close()
	_, _ = io.Copy(dst, src)
}

// Interceptor

type Interceptor struct {
	mu        sync.Mutex
	enabled   map[int64]bool
	pending   map[int64][]*InterceptRequest
	waiters   map[string]chan interceptDecision
	pendingID int64
}

type InterceptRequest struct {
	ID        string `json:"id"`
	ProjectID int64  `json:"project_id"`
	CreatedAt string `json:"created_at"`
	RawReq    string `json:"raw_req"`
}

type interceptDecision struct {
	Allow bool
	Raw   string
}

func NewInterceptor() *Interceptor {
	return &Interceptor{
		enabled: make(map[int64]bool),
		pending: make(map[int64][]*InterceptRequest),
		waiters: make(map[string]chan interceptDecision),
	}
}

func (i *Interceptor) SetEnabled(projectID int64, enabled bool) {
	i.mu.Lock()
	i.enabled[projectID] = enabled
	if !enabled {
		for _, req := range i.pending[projectID] {
			if ch, ok := i.waiters[req.ID]; ok {
				delete(i.waiters, req.ID)
				ch <- interceptDecision{Allow: true, Raw: req.RawReq}
			}
		}
		i.pending[projectID] = nil
	}
	i.mu.Unlock()
}

func (i *Interceptor) IsEnabled(projectID int64) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.enabled[projectID]
}

func (i *Interceptor) Intercept(projectID int64, rawReq string) (interceptDecision, bool) {
	i.mu.Lock()
	i.pendingID++
	id := fmt.Sprintf("%d-%d", projectID, i.pendingID)
	req := &InterceptRequest{
		ID:        id,
		ProjectID: projectID,
		CreatedAt: time.Now().Format(time.RFC3339),
		RawReq:    rawReq,
	}
	ch := make(chan interceptDecision, 1)
	i.waiters[id] = ch
	i.pending[projectID] = append(i.pending[projectID], req)
	i.mu.Unlock()

	select {
	case decision := <-ch:
		return decision, true
	case <-time.After(120 * time.Second):
		i.cleanup(projectID, id)
		return interceptDecision{}, false
	}
}

func (i *Interceptor) List(projectID int64) []*InterceptRequest {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]*InterceptRequest(nil), i.pending[projectID]...)
}

func (i *Interceptor) Decide(projectID int64, id string, allow bool, rawReq string) error {
	i.mu.Lock()
	ch, ok := i.waiters[id]
	if !ok {
		i.mu.Unlock()
		return fmt.Errorf("not found")
	}
	delete(i.waiters, id)
	pending := i.pending[projectID]
	filtered := pending[:0]
	for _, p := range pending {
		if p.ID != id {
			filtered = append(filtered, p)
		}
	}
	i.pending[projectID] = filtered
	i.mu.Unlock()

	ch <- interceptDecision{Allow: allow, Raw: rawReq}
	return nil
}

func (i *Interceptor) cleanup(projectID int64, id string) {
	i.mu.Lock()
	delete(i.waiters, id)
	pending := i.pending[projectID]
	filtered := pending[:0]
	for _, p := range pending {
		if p.ID != id {
			filtered = append(filtered, p)
		}
	}
	i.pending[projectID] = filtered
	i.mu.Unlock()
}

// Automator

type Automator struct {
	mu   sync.Mutex
	runs map[int64]context.CancelFunc
	app  *App
}

type AutomatorConfig struct {
	RawRequest  string          `json:"raw_request"`
	AttackType  string          `json:"attack_type"`
	RatePerSec  int             `json:"rate_per_sec"`
	DelaySec    float64         `json:"delay_sec"`
	JitterSec   float64         `json:"jitter_sec"`
	Positions   []PayloadConfig `json:"positions"`
	Placeholder string          `json:"placeholder"`
	TabID       int64           `json:"tab_id"`
}

type PayloadConfig struct {
	Kind    string       `json:"kind"`
	Numbers NumberConfig `json:"numbers"`
	Words   []string     `json:"words"`
}

type NumberConfig struct {
	Start     int `json:"start"`
	End       int `json:"end"`
	Step      int `json:"step"`
	MinDigits int `json:"min_digits"`
	MaxDigits int `json:"max_digits"`
}

func NewAutomator() *Automator {
	return &Automator{
		runs: make(map[int64]context.CancelFunc),
	}
}

func (a *Automator) Start(db *sql.DB, projectID int64, cfg AutomatorConfig) (int64, error) {
	if strings.TrimSpace(cfg.RawRequest) == "" {
		return 0, fmt.Errorf("raw request required")
	}
	if cfg.RatePerSec <= 0 {
		cfg.RatePerSec = 10
	}
	if cfg.Placeholder == "" {
		cfg.Placeholder = "§"
	}
	cfgJSON, _ := json.Marshal(cfg)
	res, err := db.Exec(`
		INSERT INTO automator_runs (project_id, tab_id, created_at, status, total_requests, completed_requests, config_json)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, projectID, cfg.TabID, time.Now(), "running", 0, 0, string(cfgJSON))
	if err != nil {
		return 0, err
	}
	runID, _ := res.LastInsertId()
	ctx, cancel := context.WithCancel(context.Background())
	a.mu.Lock()
	a.runs[runID] = cancel
	a.mu.Unlock()
	go a.run(ctx, db, runID, projectID, cfg)
	return runID, nil
}

func (a *Automator) run(ctx context.Context, db *sql.DB, runID int64, projectID int64, cfg AutomatorConfig) {
	defer func() {
		a.mu.Lock()
		delete(a.runs, runID)
		a.mu.Unlock()
		_, _ = db.Exec(`UPDATE automator_runs SET status = ? WHERE id = ? AND status = ?`, "finished", runID, "running")
	}()
	requests, err := buildAutomatorRequests(cfg)
	if err != nil {
		_, _ = db.Exec(`UPDATE automator_runs SET status = ? WHERE id = ?`, "failed", runID)
		return
	}
	_, _ = db.Exec(`UPDATE automator_runs SET total_requests = ?, completed_requests = 0 WHERE id = ?`, len(requests), runID)
	interval := time.Duration(float64(time.Second) / float64(cfg.RatePerSec))
	for idx, req := range requests {
		select {
		case <-ctx.Done():
			_, _ = db.Exec(`UPDATE automator_runs SET status = ? WHERE id = ?`, "stopped", runID)
			return
		default:
		}
		rawReq := req.Raw
		valuesJSON, _ := json.Marshal(req.Values)
		if cfg.DelaySec > 0 {
			time.Sleep(time.Duration(cfg.DelaySec * float64(time.Second)))
		}
		if cfg.JitterSec > 0 {
			jitter := randomFloat(-cfg.JitterSec, cfg.JitterSec)
			time.Sleep(time.Duration(jitter * float64(time.Second)))
		}
		select {
		case <-ctx.Done():
			_, _ = db.Exec(`UPDATE automator_runs SET status = ? WHERE id = ?`, "stopped", runID)
			return
		default:
		}
		respRaw, duration, respLen, err := a.app.sendRawRequest(rawReq, projectID)
		status := "ok"
		statusCode := extractStatus(respRaw)
		if err != nil {
			status = "error: " + err.Error()
			respRaw = ""
			respLen = 0
			duration = 0
			statusCode = 0
		}
		_, _ = db.Exec(`
			INSERT INTO automator_requests (run_id, index_no, status, status_code, payload_values, duration_ms, resp_len, req_raw, resp_raw)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, runID, idx+1, status, statusCode, string(valuesJSON), duration.Milliseconds(), respLen, rawReq, respRaw)
		_, _ = db.Exec(`UPDATE automator_runs SET completed_requests = completed_requests + 1 WHERE id = ?`, runID)
		time.Sleep(interval)
	}
}

func (a *Automator) Stop(runID int64) bool {
	a.mu.Lock()
	cancel, ok := a.runs[runID]
	a.mu.Unlock()
	if !ok {
		return false
	}
	cancel()
	return true
}

type AutomatorRequest struct {
	Raw    string
	Values []string
}

func buildAutomatorRequests(cfg AutomatorConfig) ([]AutomatorRequest, error) {
	prefixes, defaults, ok := parsePlaceholders(cfg.RawRequest, cfg.Placeholder)
	if !ok || len(defaults) == 0 {
		return []AutomatorRequest{{Raw: cfg.RawRequest}}, nil
	}
	if len(cfg.Positions) != len(defaults) {
		return nil, fmt.Errorf("positions count mismatch")
	}
	payloads, err := buildPayloads(cfg.Positions)
	if err != nil {
		return nil, err
	}
	return combineRequests(cfg.AttackType, prefixes, defaults, payloads), nil
}

func parsePlaceholders(raw, placeholder string) ([]string, []string, bool) {
	if placeholder == "" || !strings.Contains(raw, placeholder) {
		return nil, nil, false
	}
	parts := strings.Split(raw, placeholder)
	if len(parts)%2 == 0 {
		return nil, nil, false
	}
	prefixes := []string{parts[0]}
	var defaults []string
	for i := 1; i < len(parts); i += 2 {
		defaults = append(defaults, parts[i])
		prefixes = append(prefixes, parts[i+1])
	}
	return prefixes, defaults, true
}

func buildPayloads(configs []PayloadConfig) ([][]string, error) {
	var out [][]string
	for _, cfg := range configs {
		switch cfg.Kind {
		case "numbers":
			payload := buildNumberPayload(cfg.Numbers)
			out = append(out, payload)
		case "dictionary":
			out = append(out, cfg.Words)
		default:
			return nil, fmt.Errorf("unknown payload kind")
		}
	}
	return out, nil
}

func buildNumberPayload(cfg NumberConfig) []string {
	start := cfg.Start
	end := cfg.End
	step := cfg.Step
	if step == 0 {
		step = 1
	}
	if end < start {
		end = start
	}
	var out []string
	for i := start; i <= end; i += step {
		value := strconv.Itoa(i)
		if cfg.MinDigits > 0 && len(value) < cfg.MinDigits {
			value = strings.Repeat("0", cfg.MinDigits-len(value)) + value
		}
		if cfg.MaxDigits > 0 && len(value) > cfg.MaxDigits {
			value = value[:cfg.MaxDigits]
		}
		out = append(out, value)
	}
	return out
}

func combineRequests(attack string, prefixes []string, defaults []string, payloads [][]string) []AutomatorRequest {
	if len(payloads) == 0 {
		return []AutomatorRequest{{Raw: assemble(prefixes, defaults), Values: defaults}}
	}
	switch attack {
	case "battering":
		return combineBattering(prefixes, defaults, payloads)
	case "pitchfork":
		return combinePitchfork(prefixes, defaults, payloads)
	case "cluster":
		return combineCluster(prefixes, defaults, payloads, 0, nil)
	default:
		return combineSniper(prefixes, defaults, payloads)
	}
}

func combineSniper(prefixes []string, defaults []string, payloads [][]string) []AutomatorRequest {
	var out []AutomatorRequest
	for i, payload := range payloads {
		for _, val := range payload {
			values := append([]string{}, defaults...)
			if i < len(values) {
				values[i] = val
			}
			out = append(out, AutomatorRequest{Raw: assemble(prefixes, values), Values: values})
		}
	}
	return out
}

func combineBattering(prefixes []string, defaults []string, payloads [][]string) []AutomatorRequest {
	var out []AutomatorRequest
	// Use first payload list and apply to all positions
	if len(payloads) == 0 {
		return out
	}
	for _, val := range payloads[0] {
		values := make([]string, len(defaults))
		for i := range values {
			values[i] = val
		}
		out = append(out, AutomatorRequest{Raw: assemble(prefixes, values), Values: values})
	}
	return out
}

func combinePitchfork(prefixes []string, defaults []string, payloads [][]string) []AutomatorRequest {
	var out []AutomatorRequest
	minLen := math.MaxInt
	for _, p := range payloads {
		if len(p) < minLen {
			minLen = len(p)
		}
	}
	for i := 0; i < minLen; i++ {
		values := append([]string{}, defaults...)
		for pos := range values {
			if pos < len(payloads) && i < len(payloads[pos]) {
				values[pos] = payloads[pos][i]
			}
		}
		out = append(out, AutomatorRequest{Raw: assemble(prefixes, values), Values: values})
	}
	return out
}

func combineCluster(prefixes []string, defaults []string, payloads [][]string, idx int, current []string) []AutomatorRequest {
	if idx == len(payloads) {
		values := append([]string{}, defaults...)
		for i := range current {
			if i < len(values) {
				values[i] = current[i]
			}
		}
		return []AutomatorRequest{{Raw: assemble(prefixes, values), Values: values}}
	}
	var out []AutomatorRequest
	for _, val := range payloads[idx] {
		next := append(append([]string{}, current...), val)
		out = append(out, combineCluster(prefixes, defaults, payloads, idx+1, next)...)
	}
	return out
}

func assemble(prefixes []string, values []string) string {
	var b strings.Builder
	for i, part := range prefixes {
		b.WriteString(part)
		if i < len(values) {
			b.WriteString(values[i])
		}
	}
	return b.String()
}

func randomFloat(min, max float64) float64 {
	if min > max {
		min, max = max, min
	}
	n, _ := crand.Int(crand.Reader, big.NewInt(1000000))
	ratio := float64(n.Int64()) / 1000000.0
	return min + (max-min)*ratio
}

// Request utilities

func parseRawRequest(raw string) (*http.Request, error) {
	reader := bufio.NewReader(strings.NewReader(raw))
	req, err := http.ReadRequest(reader)
	if err != nil {
		return nil, err
	}
	if req.URL == nil {
		req.URL = &url.URL{}
	}
	if req.URL.Scheme == "" {
		req.URL.Scheme = "http"
	}
	if req.URL.Host == "" {
		req.URL.Host = req.Host
	}
	req.RequestURI = ""
	return req, nil
}

func detectRequestParams(raw string) (bool, bool) {
	req, err := parseRawRequest(raw)
	if err != nil {
		return false, false
	}
	hasGet := req.URL != nil && len(req.URL.Query()) > 0
	hasPost := false
	if req.Method == http.MethodPost || req.Method == http.MethodPut || req.Method == http.MethodPatch {
		body, _ := io.ReadAll(req.Body)
		if len(body) > 0 {
			hasPost = true
		}
	}
	return hasGet, hasPost
}

func (app *App) sendRawRequest(raw string, projectID int64) (string, time.Duration, int, error) {
	req, err := parseRawRequest(raw)
	if err != nil {
		return "", 0, 0, err
	}
	return app.sendRequest(req, projectID)
}

func (app *App) sendRawRequestWithContext(ctx context.Context, raw string, projectID int64) (string, time.Duration, int, error) {
	req, err := parseRawRequest(raw)
	if err != nil {
		return "", 0, 0, err
	}
	return app.sendRequestWithContext(ctx, req, projectID)
}

func (app *App) sendRawRequestWithContextBody(ctx context.Context, raw string, projectID int64) (string, time.Duration, int, error) {
	req, err := parseRawRequest(raw)
	if err != nil {
		return "", 0, 0, err
	}
	return app.sendRequestWithContextBody(ctx, req, projectID)
}

func (p *proxyConnPool) get(proxyAddr string) net.Conn {
	p.mu.Lock()
	list := p.conns[proxyAddr]
	for len(list) > 0 {
		pc := list[len(list)-1]
		list = list[:len(list)-1]
		p.conns[proxyAddr] = list
		p.mu.Unlock()
		if time.Since(pc.putAt) < p.idleTimeout {
			return pc.conn
		}
		pc.conn.Close()
		p.mu.Lock()
		list = p.conns[proxyAddr]
	}
	p.mu.Unlock()
	return nil
}

func (p *proxyConnPool) put(proxyAddr string, conn net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	list := p.conns[proxyAddr]
	// Evict idle connections that exceeded timeout
	i := 0
	for _, pc := range list {
		if now.Sub(pc.putAt) < p.idleTimeout {
			list[i] = pc
			i++
		} else {
			pc.conn.Close()
		}
	}
	list = list[:i]
	if len(list) >= p.max {
		conn.Close()
		return
	}
	p.conns[proxyAddr] = append(list, &pooledConn{conn: conn, putAt: now})
}

func (app *App) getProxyConn(proxyAddr string) net.Conn {
	return app.proxyConnPool.get(proxyAddr)
}

func (app *App) refillProxyPool(proxyAddr string, timeout time.Duration) {
	app.proxyConnPool.dialSem <- struct{}{}
	defer func() { <-app.proxyConnPool.dialSem }()
	conn, err := net.DialTimeout("tcp", proxyAddr, timeout)
	if err != nil {
		return
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	app.proxyConnPool.put(proxyAddr, conn)
}

func (app *App) dialThroughProxy(projectID int64, network, addr string, timeout time.Duration) (net.Conn, error) {
	settings, err := app.getProjectProxySettings(projectID)
	if err != nil || !settings.Enabled {
		return net.DialTimeout(network, addr, timeout)
	}
	if !strings.Contains(addr, ":") {
		addr = addr + ":443"
	}
	proxyAddr := fmt.Sprintf("%s:%d", settings.Host, settings.Port)
	if settings.Type == "socks5" {
		var auth *proxy.Auth
		if settings.User != "" {
			auth = &proxy.Auth{User: settings.User, Password: settings.Pass}
		}
		dialer, err := proxy.SOCKS5("tcp", proxyAddr, auth, &net.Dialer{Timeout: timeout})
		if err != nil {
			return nil, err
		}
		conn, err := dialer.Dial(network, addr)
		if err != nil {
			return nil, fmt.Errorf("SOCKS5 proxy: %w", err)
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
		}
		return conn, nil
	}
	var conn net.Conn
	var fromPool bool
	for try := 0; try < 3; try++ {
		conn = app.getProxyConn(proxyAddr)
		fromPool = conn != nil
		if conn == nil {
			app.proxyConnPool.dialSem <- struct{}{}
			defer func() { <-app.proxyConnPool.dialSem }()
			var err error
			conn, err = net.DialTimeout("tcp", proxyAddr, timeout)
			if err != nil {
				return nil, fmt.Errorf("HTTP proxy connect: %w", err)
			}
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
		}
		if fromPool {
			go app.refillProxyPool(proxyAddr, timeout)
		}
		req, _ := http.NewRequest("CONNECT", "http://"+addr, nil)
		req.Host = addr
		if settings.User != "" {
			req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(settings.User+":"+settings.Pass)))
		}
		if err := req.Write(conn); err != nil {
			conn.Close()
			if try == 0 {
				continue
			}
			return nil, fmt.Errorf("HTTP proxy CONNECT write: %w", err)
		}
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, req)
		if err != nil {
			conn.Close()
			if try == 0 {
				continue
			}
			return nil, fmt.Errorf("HTTP proxy CONNECT response: %w", err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			conn.Close()
			if try == 0 {
				continue
			}
			return nil, fmt.Errorf("HTTP proxy CONNECT failed: %s", resp.Status)
		}
		if br.Buffered() > 0 {
			conn = &bufferedConn{conn: conn, br: br}
		}
		return conn, nil
	}
	return nil, fmt.Errorf("HTTP proxy CONNECT failed after retry")
}

type bufferedConn struct {
	conn net.Conn
	br   *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (n int, err error) {
	return c.br.Read(p)
}

func (c *bufferedConn) Write(p []byte) (n int, err error) {
	return c.conn.Write(p)
}

func (c *bufferedConn) Close() error {
	return c.conn.Close()
}

func (c *bufferedConn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

func (c *bufferedConn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

func (c *bufferedConn) SetDeadline(t time.Time) error {
	return c.conn.SetDeadline(t)
}

func (c *bufferedConn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

func (c *bufferedConn) SetWriteDeadline(t time.Time) error {
	return c.conn.SetWriteDeadline(t)
}

func (app *App) buildTransport(projectID int64) (*http.Transport, error) {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}
	settings, err := app.getProjectProxySettings(projectID)
	if err != nil || !settings.Enabled {
		return transport, err
	}
	addr := fmt.Sprintf("%s:%d", settings.Host, settings.Port)
	if settings.Type == "socks5" {
		var auth *proxy.Auth
		if settings.User != "" {
			auth = &proxy.Auth{User: settings.User, Password: settings.Pass}
		}
		dialer, err := proxy.SOCKS5("tcp", addr, auth, &net.Dialer{Timeout: 30 * time.Second})
		if err != nil {
			return transport, err
		}
		transport.Proxy = nil
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		}
		transport.MaxIdleConns = 500
		transport.MaxIdleConnsPerHost = 100
		transport.IdleConnTimeout = 2 * time.Second
		return transport, nil
	}
	proxyURL := &url.URL{Scheme: "http", Host: addr}
	if settings.User != "" {
		proxyURL.User = url.UserPassword(settings.User, settings.Pass)
	}
	transport.Proxy = http.ProxyURL(proxyURL)
	transport.MaxIdleConns = 500
	transport.MaxIdleConnsPerHost = 100
	transport.IdleConnTimeout = 2 * time.Second
	return transport, nil
}

func (app *App) sendRequest(req *http.Request, projectID int64) (string, time.Duration, int, error) {
	raw, duration, length, _, _, err := app.sendRequestInfo(req, projectID)
	return raw, duration, length, err
}

func (app *App) sendRequestWithContext(ctx context.Context, req *http.Request, projectID int64) (string, time.Duration, int, error) {
	raw, duration, length, _, _, err := app.sendRequestWithContextInfo(ctx, req, projectID)
	return raw, duration, length, err
}

func (app *App) sendRequestInfo(req *http.Request, projectID int64) (string, time.Duration, int, string, string, error) {
	return app.sendRequestWithContextInfo(context.Background(), req, projectID)
}

func (app *App) sendRequestWithContextInfo(ctx context.Context, req *http.Request, projectID int64) (string, time.Duration, int, string, string, error) {
	if err := app.applyHeaderRules(req, projectID); err != nil {
		return "", 0, 0, "", "", err
	}
	transport, err := app.buildTransport(projectID)
	if err != nil {
		return "", 0, 0, "", "", err
	}
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
	}
	var remoteAddr string
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			if info.Conn != nil {
				remoteAddr = info.Conn.RemoteAddr().String()
			}
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(ctx, trace))
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, 0, "", "", err
	}
	defer resp.Body.Close()
	dump, err := httputil.DumpResponse(resp, true)
	if err != nil {
		return "", 0, 0, "", "", err
	}
	duration := time.Since(start)
	respRaw := string(dump)
	respLen := len(dump)
	respMime := resp.Header.Get("Content-Type")
	respIP := remoteAddr
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		respIP = host
	}
	return respRaw, duration, respLen, respMime, respIP, nil
}

func (app *App) sendRequestWithContextBody(ctx context.Context, req *http.Request, projectID int64) (string, time.Duration, int, error) {
	if err := app.applyHeaderRules(req, projectID); err != nil {
		return "", 0, 0, err
	}
	transport, err := app.buildTransport(projectID)
	if err != nil {
		return "", 0, 0, err
	}
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
	}
	req = req.WithContext(ctx)
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, 0, err
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, 0, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	dump, err := httputil.DumpResponse(resp, true)
	if err != nil {
		return "", 0, 0, err
	}
	duration := time.Since(start)
	respRaw := string(dump)
	respLen := len(bodyBytes)
	return respRaw, duration, respLen, nil
}
