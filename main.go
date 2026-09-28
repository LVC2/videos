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
	ID        string
	Name      string
	RTSP      string
	Enabled   bool
	Autostart bool
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
	mux.HandleFunc("/api/cameras", s.cameras)
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
	const query = "SELECT slug, name, rtsp_url, COALESCE(rtsp_username, ''), COALESCE(rtsp_password, ''), enabled, autostart FROM cameras ORDER BY sort_order, id"

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
		)

		if err := rows.Scan(&slug, &name, &rtspURL, &username, &password, &enabled, &autostart); err != nil {
			return nil, err
		}

		cameras = append(cameras, Camera{
			ID:        slug,
			Name:      name,
			RTSP:      buildRTSPURL(rtspURL, username, password),
			Enabled:   enabled,
			Autostart: autostart,
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

func (s *Server) cameras(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	type item struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Enabled   bool   `json:"enabled"`
		Autostart bool   `json:"autostart"`
		Running   bool   `json:"running"`
		Started   string `json:"started,omitempty"`
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
