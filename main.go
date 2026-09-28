package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

type Camera struct {
	ID          string
	Name        string
	RTSP        string
	Enabled     bool
	Autostart   bool
	SectionID   sql.NullInt64
	SectionName string
	SortOrder   int
}

type Config struct {
	Listen   string `json:"listen"`
	FFmpeg   string `json:"ffmpeg"`
	MediaDir string `json:"media_dir"`
	DBDSN    string `json:"db_dsn"`
}

type Stream struct {
	Camera  Camera
	Cmd     *exec.Cmd
	Started time.Time
}

type Server struct {
	cfg     Config
	db      *sql.DB
	cameras []Camera
	streams map[string]*Stream
	mu      sync.RWMutex
}

func main() {
	cfg, err := loadConfig("config.json")
	if err != nil {
		log.Fatal(err)
	}

	if cfg.Listen == "" {
		cfg.Listen = ":8090"
	}
	if cfg.FFmpeg == "" {
		cfg.FFmpeg = "ffmpeg"
	}
	if cfg.MediaDir == "" {
		cfg.MediaDir = "./runtime"
	}

	dsn := strings.TrimSpace(os.Getenv("VIDEOS_DB_DSN"))
	if dsn == "" {
		dsn = strings.TrimSpace(cfg.DBDSN)
	}
	if dsn == "" {
		log.Fatal("VIDEOS_DB_DSN is not configured")
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)

	if err := db.Ping(); err != nil {
		log.Fatalf("MariaDB connection failed: %v", err)
	}

	cameras, err := loadCameras(db)
	if err != nil {
		log.Fatalf("loading cameras from MariaDB failed: %v", err)
	}

	if err := os.MkdirAll(cfg.MediaDir, 0755); err != nil {
		log.Fatal(err)
	}

	s := &Server{
		cfg:     cfg,
		db:      db,
		cameras: cameras,
		streams: map[string]*Stream{},
	}

	for _, camera := range cameras {
		if camera.Enabled && camera.Autostart {
			if err := s.start(camera); err != nil {
				log.Printf("autostart camera %s failed: %v", camera.ID, err)
			}
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", s.health)
	mux.HandleFunc("/api/sections", s.sectionsAPI)
	mux.HandleFunc("/api/sections/", s.sectionAction)
	mux.HandleFunc("/api/cameras", s.camerasAPI)
	mux.HandleFunc("/api/cameras/", s.cameraAction)
	mux.Handle("/hls/", http.StripPrefix("/hls/", http.FileServer(http.Dir(cfg.MediaDir))))
	mux.Handle("/", http.FileServer(http.Dir("./web")))

	log.Printf("video-core listening on %s", cfg.Listen)
	log.Fatal(http.ListenAndServe(cfg.Listen, withHeaders(mux)))
}

func loadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}

	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, err
	}

	return c, nil
}

func loadCameras(db *sql.DB) ([]Camera, error) {
	const query = "SELECT c.slug, c.name, c.rtsp_url, COALESCE(c.rtsp_username, ''), COALESCE(c.rtsp_password, ''), c.enabled, c.autostart, c.section_id, COALESCE(s.name, ''), c.sort_order FROM cameras c LEFT JOIN sections s ON s.id = c.section_id ORDER BY c.sort_order, c.id"

	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cameras []Camera
	for rows.Next() {
		var (
			slug, name, rtspURL string
			username, password  string
			enabled, autostart  bool
			sectionID           sql.NullInt64
			sectionName         string
			sortOrder           int
		)

		if err := rows.Scan(&slug, &name, &rtspURL, &username, &password, &enabled, &autostart, &sectionID, &sectionName, &sortOrder); err != nil {
			return nil, err
		}

		cameras = append(cameras, Camera{
			ID:        slug,
			Name:      name,
			RTSP:      buildRTSPURL(rtspURL, username, password),
			Enabled:   enabled,
			Autostart:   autostart,
			SectionID:   sectionID,
			SectionName: sectionName,
			SortOrder:   sortOrder,
		})
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	log.Printf("loaded %d cameras from MariaDB", len(cameras))
	return cameras, nil
}

func buildRTSPURL(raw, username, password string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || username == "" {
		return raw
	}

	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}

	u.User = url.UserPassword(username, password)
	return u.String()
}

func (s *Server) findCamera(id string) (Camera, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, c := range s.cameras {
		if c.ID == id {
			return c, true
		}
	}

	return Camera{}, false
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	status := "ok"
	if err := s.db.Ping(); err != nil {
		status = "database_error"
	}

	jsonResponse(w, map[string]any{
		"status": status,
		"time":   time.Now().UTC(),
	})
}


type cameraPayload struct {
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	RTSPURL   string `json:"rtsp_url"`
	Username  string `json:"username"`
	Password  string `json:"password"`
	Enabled   *bool  `json:"enabled"`
	Autostart *bool  `json:"autostart"`
	SectionID *int64 `json:"section_id"`
	SortOrder *int    `json:"sort_order"`
}

type sectionPayload struct {
	Name      string `json:"name"`
	SortOrder *int   `json:"sort_order"`
}

func (s *Server) listSections(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	rows, err := s.db.Query("SELECT id, name, sort_order FROM sections WHERE is_active=1 ORDER BY sort_order, id")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type item struct {
		ID        int64  `json:"id"`
		Name      string `json:"name"`
		SortOrder int    `json:"sort_order"`
	}

	out := make([]item, 0)
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.ID, &it.Name, &it.SortOrder); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, out)
}

func (s *Server) sectionsAPI(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listSections(w, r)
	case http.MethodPost:
		s.createSection(w, r)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) createSection(w http.ResponseWriter, r *http.Request) {
	var p sectionPayload
	if err := decodeJSON(r, &p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	sortOrder := 0
	if p.SortOrder != nil {
		sortOrder = *p.SortOrder
	}

	if _, err := s.db.Exec("INSERT INTO sections (name, sort_order, is_active) VALUES (?, ?, 1)", p.Name, sortOrder); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	jsonResponse(w, map[string]any{"status": "ok"})
}

func (s *Server) sectionAction(w http.ResponseWriter, r *http.Request) {
	idText := strings.TrimPrefix(r.URL.Path, "/api/sections/")
	if idText == "" || strings.Contains(idText, "/") {
		http.Error(w, "invalid section id", http.StatusBadRequest)
		return
	}

	var id int64
	if _, err := fmt.Sscanf(idText, "%d", &id); err != nil || id <= 0 {
		http.Error(w, "invalid section id", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodPut:
		var p sectionPayload
		if err := decodeJSON(r, &p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		p.Name = strings.TrimSpace(p.Name)
		if p.Name == "" {
			http.Error(w, "name is required", http.StatusBadRequest)
			return
		}
		sortOrder := 0
		if p.SortOrder != nil {
			sortOrder = *p.SortOrder
		}
		result, err := s.db.Exec("UPDATE sections SET name=?, sort_order=? WHERE id=? AND is_active=1", p.Name, sortOrder, id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if n, _ := result.RowsAffected(); n == 0 {
			http.Error(w, "section not found", http.StatusNotFound)
			return
		}
		jsonResponse(w, map[string]any{"status": "ok"})

	case http.MethodDelete:
		result, err := s.db.Exec("DELETE FROM sections WHERE id=? AND is_active=1", id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if n, _ := result.RowsAffected(); n == 0 {
			http.Error(w, "section not found", http.StatusNotFound)
			return
		}
		jsonResponse(w, map[string]any{"status": "ok"})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) camerasAPI(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listCameras(w, r)
	case http.MethodPost:
		s.createCamera(w, r)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) createCamera(w http.ResponseWriter, r *http.Request) {
	var p cameraPayload
	if err := decodeJSON(r, &p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p.Slug = strings.TrimSpace(p.Slug)
	p.Name = strings.TrimSpace(p.Name)
	p.RTSPURL = strings.TrimSpace(p.RTSPURL)
	p.Username = strings.TrimSpace(p.Username)
	if p.Slug == "" || p.Name == "" || p.RTSPURL == "" {
		http.Error(w, "slug, name and rtsp_url are required", http.StatusBadRequest)
		return
	}

	enabled, autostart, sortOrder := payloadDefaults(p)
	var sectionID any
	if p.SectionID != nil && *p.SectionID > 0 {
		sectionID = *p.SectionID
	}

	_, err := s.db.Exec(
		"INSERT INTO cameras (section_id, slug, name, rtsp_url, rtsp_username, rtsp_password, enabled, autostart, sort_order) VALUES (?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?)",
		sectionID, p.Slug, p.Name, p.RTSPURL, p.Username, p.Password, enabled, autostart, sortOrder,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err := s.reloadCameras(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]any{"status": "ok"})
}

func (s *Server) cameraDetails(w http.ResponseWriter, id string) {
	var rtspURL, username string
	err := s.db.QueryRow("SELECT rtsp_url, COALESCE(rtsp_username, '') FROM cameras WHERE slug=?", id).Scan(&rtspURL, &username)
	if err == sql.ErrNoRows {
		http.Error(w, "camera not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]any{"id": id, "rtsp_url": rtspURL, "username": username})
}

func (s *Server) updateCamera(w http.ResponseWriter, r *http.Request, id string) {
	var p cameraPayload
	if err := decodeJSON(r, &p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p.Name = strings.TrimSpace(p.Name)
	p.RTSPURL = strings.TrimSpace(p.RTSPURL)
	p.Username = strings.TrimSpace(p.Username)
	if p.Name == "" || p.RTSPURL == "" {
		http.Error(w, "name and rtsp_url are required", http.StatusBadRequest)
		return
	}
	if _, ok := s.findCamera(id); !ok {
		http.Error(w, "camera not found", http.StatusNotFound)
		return
	}

	enabled, autostart, sortOrder := payloadDefaults(p)
	var sectionID any
	if p.SectionID != nil && *p.SectionID > 0 {
		sectionID = *p.SectionID
	}

	_, err := s.db.Exec(
		"UPDATE cameras SET section_id=?, name=?, rtsp_url=?, rtsp_username=NULLIF(?, ''), rtsp_password=CASE WHEN ? <> '' THEN ? ELSE rtsp_password END, enabled=?, autostart=?, sort_order=? WHERE slug=?",
		sectionID, p.Name, p.RTSPURL, p.Username, p.Password, p.Password, enabled, autostart, sortOrder, id,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	s.stop(id)
	if err := s.reloadCameras(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]any{"status": "ok"})
}

func (s *Server) deleteCamera(w http.ResponseWriter, id string) {
	if _, ok := s.findCamera(id); !ok {
		http.Error(w, "camera not found", http.StatusNotFound)
		return
	}
	s.stop(id)
	if _, err := s.db.Exec("DELETE FROM cameras WHERE slug=?", id); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err := s.reloadCameras(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]any{"status": "ok"})
}

func decodeJSON(r *http.Request, v any) error {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return fmt.Errorf("content-type must be application/json")
	}
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

func payloadDefaults(p cameraPayload) (bool, bool, int) {
	enabled := true
	if p.Enabled != nil {
		enabled = *p.Enabled
	}
	autostart := false
	if p.Autostart != nil {
		autostart = *p.Autostart
	}
	sortOrder := 0
	if p.SortOrder != nil {
		sortOrder = *p.SortOrder
	}
	return enabled, autostart, sortOrder
}

func (s *Server) reloadCameras() error {
	cameras, err := loadCameras(s.db)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.cameras = cameras
	s.mu.Unlock()
	return nil
}

func (s *Server) listCameras(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	type item struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Enabled     bool   `json:"enabled"`
		Autostart   bool   `json:"autostart"`
		Running     bool   `json:"running"`
		Started     string `json:"started,omitempty"`
		SectionID   *int64 `json:"section_id,omitempty"`
		SectionName string `json:"section_name,omitempty"`
		SortOrder   int    `json:"sort_order"`
	}

	s.mu.RLock()
	out := make([]item, 0, len(s.cameras))
	for _, c := range s.cameras {
		st, ok := s.streams[c.ID]
		it := item{
			ID:        c.ID,
			Name:      c.Name,
			Enabled:   c.Enabled,
			Autostart: c.Autostart,
			Running:   ok && st.Cmd != nil && st.Cmd.Process != nil,
			SectionName: c.SectionName,
			SortOrder: c.SortOrder,
		}

		if c.SectionID.Valid {
			v := c.SectionID.Int64
			it.SectionID = &v
		}
		if ok {
			it.Started = st.Started.Format(time.RFC3339)
		}

		out = append(out, it)
	}
	s.mu.RUnlock()

	jsonResponse(w, out)
}

func (s *Server) cameraAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/cameras/"), "/")
	if len(parts) != 2 || parts[0] == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	id, action := parts[0], parts[1]

	if action == "details" && r.Method == http.MethodGet {
		s.cameraDetails(w, id)
		return
	}
	if action == "update" && r.Method == http.MethodPut {
		s.updateCamera(w, r, id)
		return
	}
	if action == "delete" && r.Method == http.MethodDelete {
		s.deleteCamera(w, id)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	camera, ok := s.findCamera(id)
	if !ok {
		http.Error(w, "camera not found", http.StatusNotFound)
		return
	}

	switch action {
	case "start":
		if !camera.Enabled {
			http.Error(w, "camera disabled", http.StatusConflict)
			return
		}
		if err := s.start(camera); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	case "stop":
		s.stop(id)
	default:
		http.Error(w, "unknown action", http.StatusNotFound)
		return
	}

	jsonResponse(w, map[string]any{"status": "ok"})
}


func (s *Server) start(camera Camera) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if st, ok := s.streams[camera.ID]; ok && st.Cmd != nil && st.Cmd.Process != nil {
		return nil
	}

	if strings.TrimSpace(camera.RTSP) == "" {
		return fmt.Errorf("camera %s has empty RTSP URL", camera.ID)
	}

	dir := filepath.Join(s.cfg.MediaDir, camera.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	_ = os.RemoveAll(filepath.Join(dir, "index.m3u8"))

	playlist := filepath.Join(dir, "index.m3u8")
	segment := filepath.Join(dir, "seg_%06d.ts")

	args := []string{
		"-hide_banner",
		"-loglevel", "warning",
		"-rtsp_transport", "tcp",
		"-i", camera.RTSP,
		"-an",
		"-c:v", "copy",
		"-f", "hls",
		"-hls_time", "1",
		"-hls_list_size", "4",
		"-hls_flags", "delete_segments+append_list+omit_endlist",
		"-hls_segment_filename", segment,
		playlist,
	}

	cmd := exec.Command(s.cfg.FFmpeg, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return err
	}

	st := &Stream{
		Camera:  camera,
		Cmd:     cmd,
		Started: time.Now(),
	}

	s.streams[camera.ID] = st

	go func(id string, c *exec.Cmd) {
		err := c.Wait()
		log.Printf("stream %s stopped: %v", id, err)

		s.mu.Lock()
		if current, ok := s.streams[id]; ok && current.Cmd == c {
			delete(s.streams, id)
		}
		s.mu.Unlock()
	}(camera.ID, cmd)

	return nil
}

func (s *Server) stop(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	st, ok := s.streams[id]
	if !ok || st.Cmd == nil || st.Cmd.Process == nil {
		return
	}

	_ = st.Cmd.Process.Kill()
	delete(s.streams, id)
}

func jsonResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func withHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		next.ServeHTTP(w, r)
	})
}
