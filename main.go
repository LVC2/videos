package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"mime"
	"mime/multipart"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"strconv"
	"sync"
	"time"

	dac "github.com/Snawoot/go-http-digest-auth-client"
	_ "github.com/go-sql-driver/mysql"
)

type Camera struct {
	ID          string
	Name        string
	IP          string
	Username    string
	Password    string
	RTSP        string
	SubRTSP     string
	Enabled     bool
	Autostart   bool
	SectionID   sql.NullInt64
	SectionName string
	SortOrder   int
}

type Config struct {
	Listen            string `json:"listen"`
	Go2RTC            string `json:"go2rtc"`
	MediaDir          string `json:"media_dir"`
	RecordingDir      string `json:"recording_dir"`
	MotionPostSeconds int    `json:"motion_post_seconds"`
	DBDSN             string `json:"db_dsn"`
}

type cameraRuntime struct {
	Cancel        context.CancelFunc
	Started       time.Time
	RecordCancel  context.CancelFunc
	StopTimer     *time.Timer
	Recording     bool
	RecordingFile string
}

type Server struct {
	cfg       Config
	db        *sql.DB
	cameras   []Camera
	runtimes  map[string]*cameraRuntime
	mu        sync.RWMutex
	http      *http.Client
}

func main() {
	cfg, err := loadConfig("config.json")
	if err != nil {
		log.Fatal(err)
	}
	if cfg.Listen == "" {
		cfg.Listen = ":8090"
	}
	if cfg.Go2RTC == "" {
		cfg.Go2RTC = "http://127.0.0.1:1984"
	}
	cfg.Go2RTC = strings.TrimRight(cfg.Go2RTC, "/")
	if cfg.MediaDir == "" {
		cfg.MediaDir = "./runtime"
	}
	if cfg.RecordingDir == "" {
		cfg.RecordingDir = cfg.MediaDir
	}
	if cfg.MotionPostSeconds <= 0 {
		cfg.MotionPostSeconds = 10
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
		cfg:      cfg,
		db:       db,
		cameras:  cameras,
		runtimes: map[string]*cameraRuntime{},
		http:     &http.Client{Timeout: 15 * time.Second},
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
	mux.HandleFunc("/api/auth/status", s.authStatus)
	mux.HandleFunc("/api/auth/login", s.authLogin)
	mux.HandleFunc("/api/auth/logout", s.authLogout)
	mux.HandleFunc("/api/auth/bootstrap", s.authBootstrap)
	mux.HandleFunc("/api/users", s.usersAPI)
	mux.HandleFunc("/api/users/", s.userAction)
	mux.HandleFunc("/api/sections", s.sectionsAPI)
	mux.HandleFunc("/api/sections/", s.sectionAction)
	mux.HandleFunc("/api/cameras", s.camerasAPI)
	mux.HandleFunc("/api/cameras/", s.cameraAction)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" { http.FileServer(http.Dir("./web")).ServeHTTP(w, r); return }
		if _, ok := s.currentUser(r); !ok { http.ServeFile(w, r, "./web/index.html"); return }
		http.FileServer(http.Dir("./web")).ServeHTTP(w, r)
	})

	log.Printf("video-core listening on %s", cfg.Listen)
	log.Printf("go2rtc API: %s", cfg.Go2RTC)
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
	const query = `SELECT c.slug, c.name, c.rtsp_url,
		COALESCE(c.rtsp_username, ''), COALESCE(c.rtsp_password, ''),
		c.enabled, c.autostart, c.section_id, COALESCE(s.name, ''), c.sort_order
		FROM cameras c
		LEFT JOIN sections s ON s.id = c.section_id
		ORDER BY c.sort_order, c.id`

	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cameras []Camera
	for rows.Next() {
		var slug, name, rtspURL, username, password, sectionName string
		var enabled, autostart bool
		var sectionID sql.NullInt64
		var sortOrder int
		if err := rows.Scan(&slug, &name, &rtspURL, &username, &password, &enabled, &autostart, &sectionID, &sectionName, &sortOrder); err != nil {
			return nil, err
		}

		u, err := url.Parse(rtspURL)
		if err != nil || u.Hostname() == "" {
			return nil, fmt.Errorf("invalid RTSP URL for camera %s", slug)
		}
		u.User = nil
		ip := u.Hostname()
		mainURL := buildRTSPURL("rtsp://"+u.Host+"/Streaming/Channels/101", username, password)
		subURL := buildRTSPURL("rtsp://"+u.Host+"/Streaming/Channels/102", username, password)

		cameras = append(cameras, Camera{
			ID:          slug,
			Name:        name,
			IP:          ip,
			Username:    username,
			Password:    password,
			RTSP:        mainURL,
			SubRTSP:     subURL,
			Enabled:     enabled,
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
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if username != "" {
		u.User = url.UserPassword(username, password)
	}
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
		"time": time.Now().UTC(),
	})
}

type cameraPayload struct {
	IP        string `json:"ip"`
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
	if _, ok := s.requireAuth(w, r); !ok { return }
	rows, err := s.db.Query("SELECT id, name, sort_order FROM sections WHERE is_active=1 ORDER BY sort_order, id")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type item struct {
		ID int64 `json:"id"`
		Name string `json:"name"`
		SortOrder int `json:"sort_order"`
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
	user, ok := s.requireAuth(w, r); if !ok { return }
	if r.Method != http.MethodGet && !user.Admin { http.Error(w, "forbidden", http.StatusForbidden); return }
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
	user, ok := s.requireAuth(w, r); if !ok { return }; if !user.Admin { http.Error(w, "forbidden", http.StatusForbidden); return }
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
	user, ok := s.requireAuth(w, r); if !ok { return }; if !user.Admin { http.Error(w, "forbidden", http.StatusForbidden); return }
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
		res, err := s.db.Exec("UPDATE sections SET name=?, sort_order=? WHERE id=? AND is_active=1", p.Name, sortOrder, id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			http.Error(w, "section not found", http.StatusNotFound)
			return
		}
		jsonResponse(w, map[string]any{"status": "ok"})
	case http.MethodDelete:
		res, err := s.db.Exec("DELETE FROM sections WHERE id=? AND is_active=1", id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			http.Error(w, "section not found", http.StatusNotFound)
			return
		}
		jsonResponse(w, map[string]any{"status": "ok"})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) camerasAPI(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAuth(w, r); if !ok { return }
	if r.Method != http.MethodGet && !user.Admin { http.Error(w, "forbidden", http.StatusForbidden); return }
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
	user, ok := s.requireAuth(w, r); if !ok { return }; if !user.Admin { http.Error(w, "forbidden", http.StatusForbidden); return }
	var p cameraPayload
	if err := decodeJSON(r, &p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p.IP = strings.TrimSpace(p.IP)
	p.Username = strings.TrimSpace(p.Username)
	if net.ParseIP(p.IP) == nil || p.Username == "" || p.Password == "" {
		http.Error(w, "ip, username and password are required", http.StatusBadRequest)
		return
	}

	enabled, autostart, sortOrder := payloadDefaults(p)
	var sectionID any
	if p.SectionID != nil && *p.SectionID > 0 {
		sectionID = *p.SectionID
	}
	slug := strings.ReplaceAll(p.IP, ".", "-")
	name := "Камера " + p.IP
	rtspURL := "rtsp://" + p.IP + ":554/Streaming/Channels/101"

	if _, err := s.db.Exec(
		"INSERT INTO cameras (section_id, slug, name, rtsp_url, rtsp_username, rtsp_password, enabled, autostart, sort_order) VALUES (?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?)",
		sectionID, slug, name, rtspURL, p.Username, p.Password, enabled, autostart, sortOrder,
	); err != nil {
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
	u, err := url.Parse(rtspURL)
	if err != nil || u.Hostname() == "" {
		http.Error(w, "invalid camera RTSP URL", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]any{"id": id, "ip": u.Hostname(), "username": username})
}

func (s *Server) updateCamera(w http.ResponseWriter, r *http.Request, id string) {
	var p cameraPayload
	if err := decodeJSON(r, &p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p.IP = strings.TrimSpace(p.IP)
	p.Username = strings.TrimSpace(p.Username)
	if net.ParseIP(p.IP) == nil || p.Username == "" {
		http.Error(w, "ip and username are required", http.StatusBadRequest)
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
	rtspURL := "rtsp://" + p.IP + ":554/Streaming/Channels/101"
	name := "Камера " + p.IP

	if _, err := s.db.Exec(
		"UPDATE cameras SET section_id=?, name=?, rtsp_url=?, rtsp_username=NULLIF(?, ''), rtsp_password=CASE WHEN ? <> '' THEN ? ELSE rtsp_password END, enabled=?, autostart=?, sort_order=? WHERE slug=?",
		sectionID, name, rtspURL, p.Username, p.Password, p.Password, enabled, autostart, sortOrder, id,
	); err != nil {
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
	user, ok := s.requireAuth(w, r); if !ok { return }
	type item struct {
		ID string `json:"id"`
		Name string `json:"name"`
		Enabled bool `json:"enabled"`
		Autostart bool `json:"autostart"`
		Running bool `json:"running"`
		Recording bool `json:"recording"`
		Started string `json:"started,omitempty"`
		RecordingFile string `json:"recording_file,omitempty"`
		SectionID *int64 `json:"section_id,omitempty"`
		SectionName string `json:"section_name,omitempty"`
		SortOrder int `json:"sort_order"`
		MainStream string `json:"main_stream"`
		SubStream string `json:"sub_stream"`
	}
	s.mu.RLock()
	out := make([]item, 0, len(s.cameras))
	for _, c := range s.cameras {
		if !s.userCanViewCamera(user, c.ID) { continue }
		rt := s.runtimes[c.ID]
		it := item{
			ID: c.ID, Name: c.Name, Enabled: c.Enabled, Autostart: c.Autostart,
			Running: rt != nil, SectionName: c.SectionName, SortOrder: c.SortOrder,
			MainStream: c.ID + "_main", SubStream: c.ID + "_sub",
		}
		if c.SectionID.Valid {
			v := c.SectionID.Int64
			it.SectionID = &v
		}
		if rt != nil {
			it.Started = rt.Started.Format(time.RFC3339)
			it.Recording = rt.Recording
			it.RecordingFile = rt.RecordingFile
		}
		out = append(out, it)
	}
	s.mu.RUnlock()
	jsonResponse(w, out)
}

func (s *Server) cameraAction(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAuth(w, r); if !ok { return }
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/cameras/"), "/")
	if len(parts) != 2 || parts[0] == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	id, action := parts[0], parts[1]

	if action == "details" && r.Method == http.MethodGet {
		if !user.Admin { http.Error(w, "forbidden", http.StatusForbidden); return }
		s.cameraDetails(w, id)
		return
	}
	if action == "update" && r.Method == http.MethodPut {
		if !user.Admin { http.Error(w, "forbidden", http.StatusForbidden); return }
		s.updateCamera(w, r, id)
		return
	}
	if action == "delete" && r.Method == http.MethodDelete {
		if !user.Admin { http.Error(w, "forbidden", http.StatusForbidden); return }
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
		if !user.Admin && !user.CanControl { http.Error(w, "forbidden", http.StatusForbidden); return }
		if !s.userCanViewCamera(user, camera.ID) { http.Error(w, "forbidden", http.StatusForbidden); return }
		if !camera.Enabled {
			http.Error(w, "camera disabled", http.StatusConflict)
			return
		}
		if err := s.start(camera); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	case "stop":
		if !user.Admin && !user.CanControl { http.Error(w, "forbidden", http.StatusForbidden); return }
		if !s.userCanViewCamera(user, camera.ID) { http.Error(w, "forbidden", http.StatusForbidden); return }
		s.stop(id)
	default:
		http.Error(w, "unknown action", http.StatusNotFound)
		return
	}
	jsonResponse(w, map[string]any{"status": "ok"})
}

func (s *Server) start(camera Camera) error {
	s.mu.Lock()
	if _, ok := s.runtimes[camera.ID]; ok {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	if err := s.ensureGo2RTCStream(camera.ID+"_main", camera.RTSP); err != nil {
		return fmt.Errorf("go2rtc main stream: %w", err)
	}
	if err := s.ensureGo2RTCStream(camera.ID+"_sub", camera.SubRTSP); err != nil {
		return fmt.Errorf("go2rtc sub stream: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	rt := &cameraRuntime{Cancel: cancel, Started: time.Now()}

	s.mu.Lock()
	if existing, ok := s.runtimes[camera.ID]; ok {
		s.mu.Unlock()
		cancel()
		_ = existing
		return nil
	}
	s.runtimes[camera.ID] = rt
	s.mu.Unlock()

	go s.motionLoop(ctx, camera)
	log.Printf("camera %s started via go2rtc: main=%s sub=%s", camera.ID, camera.ID+"_main", camera.ID+"_sub")
	return nil
}

func (s *Server) stop(id string) {
	s.mu.Lock()
	rt, ok := s.runtimes[id]
	if !ok {
		s.mu.Unlock()
		return
	}
	delete(s.runtimes, id)
	if rt.StopTimer != nil {
		rt.StopTimer.Stop()
	}
	if rt.RecordCancel != nil {
		rt.RecordCancel()
	}
	rt.Cancel()
	s.mu.Unlock()
	log.Printf("camera %s stopped", id)
}

func (s *Server) ensureGo2RTCStream(name, source string) error {
	u, err := url.Parse(s.cfg.Go2RTC + "/api/streams")
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("name", name)
	q.Set("src", source)
	u.RawQuery = q.Encode()

	req, err := http.NewRequest(http.MethodPut, u.String(), nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("HTTP %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

type hikEvent struct {
	EventType  string `xml:"eventType"`
	EventState string `xml:"eventState"`
	ChannelID  string `xml:"channelID"`
}

func (s *Server) motionLoop(ctx context.Context, camera Camera) {
	for {
		err := s.readMotionStream(ctx, camera)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("camera %s motion stream: %v", camera.ID, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func (s *Server) readMotionStream(ctx context.Context, camera Camera) error {
	eventURL := "http://" + camera.IP + "/ISAPI/Event/notification/alertStream"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, eventURL, nil)
	if err != nil {
		return err
	}
	client := &http.Client{
		Transport: dac.NewDigestTransport(camera.Username, camera.Password, http.DefaultTransport),
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("HTTP %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	if mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err == nil && strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return fmt.Errorf("multipart response has no boundary")
		}
		reader := multipart.NewReader(resp.Body, boundary)
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			data, err := io.ReadAll(part)
			part.Close()
			if err != nil {
				return err
			}
			s.handleMotionXML(camera.ID, data)
		}
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	s.handleMotionXML(camera.ID, data)
	return nil
}

func (s *Server) handleMotionXML(id string, data []byte) {
	start := strings.Index(string(data), "<EventNotificationAlert")
	end := strings.LastIndex(string(data), "</EventNotificationAlert>")
	if start < 0 || end < 0 {
		return
	}
	data = data[start : end+len("</EventNotificationAlert>")]

	var event hikEvent
	if err := xml.Unmarshal(data, &event); err != nil {
		return
	}
	if event.EventType != "VMD" {
		return
	}
	switch strings.ToLower(event.EventState) {
	case "active":
		s.motionActive(id)
	case "inactive":
		s.motionInactive(id)
	}
}

func (s *Server) motionActive(id string) {
	s.mu.Lock()
	rt := s.runtimes[id]
	if rt == nil {
		s.mu.Unlock()
		return
	}
	if rt.StopTimer != nil {
		rt.StopTimer.Stop()
		rt.StopTimer = nil
	}
	already := rt.Recording
	s.mu.Unlock()
	if !already {
		if err := s.startRecording(id); err != nil {
			log.Printf("camera %s start recording: %v", id, err)
		}
	}
}

func (s *Server) motionInactive(id string) {
	s.mu.Lock()
	rt := s.runtimes[id]
	if rt == nil {
		s.mu.Unlock()
		return
	}
	if rt.StopTimer != nil {
		rt.StopTimer.Stop()
	}
	delay := time.Duration(s.cfg.MotionPostSeconds) * time.Second
	rt.StopTimer = time.AfterFunc(delay, func() {
		s.stopRecording(id)
	})
	s.mu.Unlock()
}

func (s *Server) startRecording(id string) error {
	camera, ok := s.findCamera(id)
	if !ok {
		return fmt.Errorf("camera not found")
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	rt := s.runtimes[id]
	if rt == nil {
		s.mu.Unlock()
		cancel()
		return fmt.Errorf("camera is stopped")
	}
	if rt.Recording {
		s.mu.Unlock()
		cancel()
		return nil
	}
	rt.Recording = true
	rt.RecordCancel = cancel
	s.mu.Unlock()

	dir := filepath.Join(s.cfg.RecordingDir, id, time.Now().Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0755); err != nil {
		cancel()
		s.mu.Lock()
		if current := s.runtimes[id]; current != nil {
			current.Recording = false
			current.RecordCancel = nil
		}
		s.mu.Unlock()
		return err
	}

	fileName := time.Now().Format("15-04-05.000") + ".mp4"
	path := filepath.Join(dir, fileName)

	s.mu.Lock()
	if current := s.runtimes[id]; current != nil {
		current.RecordingFile = path
	}
	s.mu.Unlock()

	go s.recordLoop(ctx, camera, path)
	log.Printf("camera %s recording started: %s", id, path)
	return nil
}

func (s *Server) recordLoop(ctx context.Context, camera Camera, path string) {
	defer func() {
		s.mu.Lock()
		if rt := s.runtimes[camera.ID]; rt != nil {
			rt.Recording = false
			rt.RecordCancel = nil
			rt.RecordingFile = ""
		}
		s.mu.Unlock()
		log.Printf("camera %s recording stopped: %s", camera.ID, path)
	}()

	u := s.cfg.Go2RTC + "/api/stream.mp4?src=" + url.QueryEscape(camera.ID+"_main") + "&mp4=all"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		log.Printf("camera %s recording request: %v", camera.ID, err)
		return
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("camera %s recording stream: %v", camera.ID, err)
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		log.Printf("camera %s recording HTTP %s: %s", camera.ID, resp.Status, strings.TrimSpace(string(body)))
		return
	}

	f, err := os.Create(path)
	if err != nil {
		log.Printf("camera %s recording file: %v", camera.ID, err)
		return
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil && ctx.Err() == nil {
		log.Printf("camera %s recording copy: %v", camera.ID, err)
	}
}

func (s *Server) stopRecording(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rt := s.runtimes[id]
	if rt == nil || !rt.Recording {
		return
	}
	if rt.RecordCancel != nil {
		rt.RecordCancel()
	}
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


// --- Authentication and access control ---

type authUser struct {
	ID int64
	Username string
	DisplayName string
	Admin bool
	CanControl bool
	AllSections bool
	AllCameras bool
}

const sessionDuration = 12 * time.Hour

func (s *Server) currentUser(r *http.Request) (authUser, bool) {
	c, err := r.Cookie("video_session")
	if err != nil || c.Value == "" { return authUser{}, false }
	sum := sha256.Sum256([]byte(c.Value))
	var u authUser
	var admin, canControl, allSections, allCameras int
	err = s.db.QueryRow("SELECT u.id,u.username,u.display_name,EXISTS(SELECT 1 FROM user_roles ur JOIN roles rr ON rr.id=ur.role_id WHERE ur.user_id=u.id AND rr.code='admin'),EXISTS(SELECT 1 FROM user_roles ur JOIN roles rr ON rr.id=ur.role_id JOIN role_permissions rp ON rp.role_id=rr.id WHERE ur.user_id=u.id AND rp.permission='camera.control'),u.all_sections,u.all_cameras FROM sessions se JOIN users u ON u.id=se.user_id WHERE se.token_hash=? AND se.expires_at>NOW(3) AND u.is_active=1", fmt.Sprintf("%x",sum[:])).Scan(&u.ID,&u.Username,&u.DisplayName,&admin,&canControl,&allSections,&allCameras)
	if err != nil { return authUser{}, false }
	u.Admin=admin!=0; u.CanControl=canControl!=0; u.AllSections=allSections!=0; u.AllCameras=allCameras!=0
	return u,true
}

func (s *Server) requireAuth(w http.ResponseWriter,r *http.Request)(authUser,bool){
	u,ok:=s.currentUser(r)
	if !ok { jsonResponseStatus(w,http.StatusUnauthorized,map[string]any{"error":"unauthorized"}); return authUser{},false }
	return u,true
}

func jsonResponseStatus(w http.ResponseWriter,status int,v any){
	w.Header().Set("Content-Type","application/json; charset=utf-8"); w.Header().Set("Cache-Control","no-store"); w.WriteHeader(status); _=json.NewEncoder(w).Encode(v)
}

func randomBytes(n int)([]byte,error){b:=make([]byte,n);_,err:=rand.Read(b);return b,err}

func pbkdf2SHA256(password string,salt []byte,iterations,keyLen int)[]byte{
	out:=make([]byte,0,keyLen)
	for block:=1;len(out)<keyLen;block++{
		m:=hmac.New(sha256.New,[]byte(password));m.Write(salt);m.Write([]byte{byte(block>>24),byte(block>>16),byte(block>>8),byte(block)});u:=m.Sum(nil);t:=append([]byte(nil),u...)
		for i:=1;i<iterations;i++{m=hmac.New(sha256.New,[]byte(password));m.Write(u);u=m.Sum(nil);for j:=range t{t[j]^=u[j]}}
		out=append(out,t...)
	}
	return out[:keyLen]
}

func hashPassword(password string)(string,error){salt,err:=randomBytes(16);if err!=nil{return "",err};key:=pbkdf2SHA256(password,salt,120000,32);return fmt.Sprintf("pbkdf2-sha256$120000$%x$%x",salt,key),nil}

func verifyPassword(password,encoded string)bool{
	p:=strings.Split(encoded,"$");if len(p)!=4||p[0]!="pbkdf2-sha256"{return false};it,err:=strconv.Atoi(p[1]);if err!=nil||it<10000||it>1000000{return false};salt,err:=hex.DecodeString(p[2]);if err!=nil{return false};want,err:=hex.DecodeString(p[3]);if err!=nil{return false};got:=pbkdf2SHA256(password,salt,it,len(want));return subtle.ConstantTimeCompare(got,want)==1
}

func (s *Server) createSession(userID int64)(string,error){
	b,err:=randomBytes(32);if err!=nil{return "",err};token:=fmt.Sprintf("%x",b);sum:=sha256.Sum256([]byte(token));_,err=s.db.Exec("INSERT INTO sessions(user_id,token_hash,expires_at) VALUES(?,?,?)",userID,fmt.Sprintf("%x",sum[:]),time.Now().Add(sessionDuration));return token,err
}

func setSessionCookie(w http.ResponseWriter,token string){http.SetCookie(w,&http.Cookie{Name:"video_session",Value:token,Path:"/",HttpOnly:true,SameSite:http.SameSiteLaxMode,MaxAge:int(sessionDuration.Seconds())})}

func (s *Server) authStatus(w http.ResponseWriter,r *http.Request){
	var n int;if err:=s.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&n);err!=nil{http.Error(w,err.Error(),500);return};u,ok:=s.currentUser(r);var user any=nil;if ok{user=map[string]any{"id":u.ID,"username":u.Username,"display_name":u.DisplayName,"admin":u.Admin,"can_control":u.CanControl}};jsonResponse(w,map[string]any{"setup_required":n==0,"authenticated":ok,"user":user})
}

func (s *Server) authLogin(w http.ResponseWriter,r *http.Request){
	if r.Method!=http.MethodPost{w.WriteHeader(405);return};var p struct{Username string;Password string};if err:=decodeJSON(r,&p);err!=nil{http.Error(w,err.Error(),400);return};var id int64;var hash string;var active bool
	err:=s.db.QueryRow("SELECT id,password_hash,is_active FROM users WHERE username=?",strings.TrimSpace(p.Username)).Scan(&id,&hash,&active);if err!=nil||!active||!verifyPassword(p.Password,hash){jsonResponseStatus(w,401,map[string]any{"error":"Неверный логин или пароль"});return};token,err:=s.createSession(id);if err!=nil{http.Error(w,err.Error(),500);return};setSessionCookie(w,token);jsonResponse(w,map[string]any{"status":"ok"})
}

func (s *Server) authLogout(w http.ResponseWriter,r *http.Request){
	if c,err:=r.Cookie("video_session");err==nil{sum:=sha256.Sum256([]byte(c.Value));_,_=s.db.Exec("DELETE FROM sessions WHERE token_hash=?",fmt.Sprintf("%x",sum[:]))};http.SetCookie(w,&http.Cookie{Name:"video_session",Value:"",Path:"/",HttpOnly:true,SameSite:http.SameSiteLaxMode,MaxAge:-1});jsonResponse(w,map[string]any{"status":"ok"})
}

func (s *Server) authBootstrap(w http.ResponseWriter,r *http.Request){
	if r.Method!=http.MethodPost{w.WriteHeader(405);return};var n int;if err:=s.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&n);err!=nil{http.Error(w,err.Error(),500);return};if n!=0{http.Error(w,"setup already completed",409);return}
	var p struct{Username string;Password string;DisplayName string};if err:=decodeJSON(r,&p);err!=nil{http.Error(w,err.Error(),400);return};p.Username=strings.TrimSpace(p.Username);p.DisplayName=strings.TrimSpace(p.DisplayName);if p.Username==""||len(p.Password)<8{http.Error(w,"username and password (8+ chars) are required",400);return};if p.DisplayName==""{p.DisplayName=p.Username};hash,err:=hashPassword(p.Password);if err!=nil{http.Error(w,err.Error(),500);return}
	res,err:=s.db.Exec("INSERT INTO users(username,password_hash,display_name,all_sections,all_cameras) VALUES(?,?,?,1,1)",p.Username,hash,p.DisplayName);if err!=nil{http.Error(w,err.Error(),409);return};id,_:=res.LastInsertId();var roleID int64;if err=s.db.QueryRow("SELECT id FROM roles WHERE code='admin'").Scan(&roleID);err==nil{_,err=s.db.Exec("INSERT INTO user_roles(user_id,role_id) VALUES(?,?)",id,roleID)};if err!=nil{http.Error(w,err.Error(),500);return};token,err:=s.createSession(id);if err!=nil{http.Error(w,err.Error(),500);return};setSessionCookie(w,token);jsonResponse(w,map[string]any{"status":"ok"})
}

func (s *Server) userCanViewCamera(u authUser,cameraID string)bool{
	if u.Admin||u.AllCameras{return true};var sectionID sql.NullInt64;if err:=s.db.QueryRow("SELECT section_id FROM cameras WHERE slug=?",cameraID).Scan(&sectionID);err!=nil{return false};if u.AllSections&&sectionID.Valid{return true}
	var n int;if err:=s.db.QueryRow("SELECT COUNT(*) FROM user_cameras uc JOIN cameras c ON c.id=uc.camera_id WHERE uc.user_id=? AND c.slug=?",u.ID,cameraID).Scan(&n);err==nil&&n>0{return true};if sectionID.Valid{_=s.db.QueryRow("SELECT COUNT(*) FROM user_sections WHERE user_id=? AND section_id=?",u.ID,sectionID.Int64).Scan(&n);if n>0{return true}};return false
}

type userPayload struct{Username string;Password string;DisplayName string;Role string;Scope string;SectionIDs []int64;CameraIDs []int64;Active *bool}

func (s *Server) usersAPI(w http.ResponseWriter,r *http.Request){
	u,ok:=s.requireAuth(w,r);if !ok{return};if !u.Admin{http.Error(w,"forbidden",403);return};switch r.Method{case http.MethodGet:s.listUsers(w,r);case http.MethodPost:s.createUser(w,r);default:w.WriteHeader(405)}
}

func (s *Server) listUsers(w http.ResponseWriter,r *http.Request){
	rows,err:=s.db.Query("SELECT u.id,u.username,u.display_name,u.is_active,u.all_sections,u.all_cameras,COALESCE((SELECT r.code FROM user_roles ur JOIN roles r ON r.id=ur.role_id WHERE ur.user_id=u.id ORDER BY r.code LIMIT 1),'viewer') FROM users u ORDER BY u.username");if err!=nil{http.Error(w,err.Error(),500);return};defer rows.Close();out:=[]any{}
	for rows.Next(){var id int64;var username,name,role string;var active,allSec,allCam bool;if err:=rows.Scan(&id,&username,&name,&active,&allSec,&allCam,&role);err!=nil{http.Error(w,err.Error(),500);return};scope:="selected";if allCam{scope="all_cameras"}else if allSec{scope="section"};out=append(out,map[string]any{"id":id,"username":username,"display_name":name,"active":active,"role":role,"scope":scope})};jsonResponse(w,out)
}

func (s *Server) userDetails(w http.ResponseWriter,id int64){
	var username,name,role string;var active,allSec,allCam bool;if err:=s.db.QueryRow("SELECT username,display_name,is_active,all_sections,all_cameras FROM users WHERE id=?",id).Scan(&username,&name,&active,&allSec,&allCam);err!=nil{http.Error(w,"user not found",404);return};role="viewer";_=s.db.QueryRow("SELECT r.code FROM user_roles ur JOIN roles r ON r.id=ur.role_id WHERE ur.user_id=? ORDER BY r.code LIMIT 1",id).Scan(&role);scope:="selected";if allCam{scope="all_cameras"}else if allSec{scope="section"};sectionIDs:=[]int64{};cameraIDs:=[]int64{}
	rows,_:=s.db.Query("SELECT section_id FROM user_sections WHERE user_id=?",id);if rows!=nil{for rows.Next(){var x int64;_=rows.Scan(&x);sectionIDs=append(sectionIDs,x)};rows.Close()};rows,_=s.db.Query("SELECT camera_id FROM user_cameras WHERE user_id=?",id);if rows!=nil{for rows.Next(){var x int64;_=rows.Scan(&x);cameraIDs=append(cameraIDs,x)};rows.Close()}
	jsonResponse(w,map[string]any{"username":username,"display_name":name,"active":active,"role":role,"scope":scope,"section_ids":sectionIDs,"camera_ids":cameraIDs})
}

func (s *Server) createUser(w http.ResponseWriter,r *http.Request){var p userPayload;if err:=decodeJSON(r,&p);err!=nil{http.Error(w,err.Error(),400);return};if err:=s.saveUser(0,p);err!=nil{http.Error(w,err.Error(),400);return};jsonResponse(w,map[string]any{"status":"ok"})}

func (s *Server) userAction(w http.ResponseWriter,r *http.Request){
	u,ok:=s.requireAuth(w,r);if !ok{return};if !u.Admin{http.Error(w,"forbidden",403);return};id,err:=strconv.ParseInt(strings.TrimPrefix(r.URL.Path,"/api/users/"),10,64);if err!=nil||id<=0{http.Error(w,"invalid user id",400);return}
	switch r.Method{case http.MethodGet:s.userDetails(w,id);case http.MethodPut:var p userPayload;if err:=decodeJSON(r,&p);err!=nil{http.Error(w,err.Error(),400);return};if err:=s.saveUser(id,p);err!=nil{http.Error(w,err.Error(),400);return};jsonResponse(w,map[string]any{"status":"ok"});case http.MethodDelete:if id==u.ID{http.Error(w,"cannot delete current user",409);return};_,err=s.db.Exec("DELETE FROM users WHERE id=?",id);if err!=nil{http.Error(w,err.Error(),500);return};jsonResponse(w,map[string]any{"status":"ok"});default:w.WriteHeader(405)}
}

func (s *Server) saveUser(id int64,p userPayload)error{
	p.Username=strings.TrimSpace(p.Username);p.DisplayName=strings.TrimSpace(p.DisplayName);if p.Username==""||p.DisplayName==""{return fmt.Errorf("username and display name are required")};if id==0&&len(p.Password)<8{return fmt.Errorf("password must contain at least 8 characters")};if p.Role!="admin"&&p.Role!="operator"&&p.Role!="viewer"{p.Role="viewer"};if p.Scope!="all_cameras"&&p.Scope!="section"&&p.Scope!="selected"&&p.Scope!="none"{p.Scope="selected"};active:=true;if p.Active!=nil{active=*p.Active};hash:="";var err error;if p.Password!=""{hash,err=hashPassword(p.Password);if err!=nil{return err}}
	tx,err:=s.db.Begin();if err!=nil{return err};defer tx.Rollback()
	if id==0{if hash==""{return fmt.Errorf("password is required")};res,e:=tx.Exec("INSERT INTO users(username,password_hash,display_name,is_active,all_sections,all_cameras) VALUES(?,?,?,?,?,?)",p.Username,hash,p.DisplayName,active,p.Scope=="section",p.Scope=="all_cameras");if e!=nil{return e};id,_=res.LastInsertId()}else{if hash!=""{_,err=tx.Exec("UPDATE users SET username=?,password_hash=?,display_name=?,is_active=?,all_sections=?,all_cameras=? WHERE id=?",p.Username,hash,p.DisplayName,active,p.Scope=="section",p.Scope=="all_cameras",id)}else{_,err=tx.Exec("UPDATE users SET username=?,display_name=?,is_active=?,all_sections=?,all_cameras=? WHERE id=?",p.Username,p.DisplayName,active,p.Scope=="section",p.Scope=="all_cameras",id)};if err!=nil{return err}}
	if _,err=tx.Exec("DELETE FROM user_roles WHERE user_id=?",id);err!=nil{return err};var roleID int64;if err=tx.QueryRow("SELECT id FROM roles WHERE code=?",p.Role).Scan(&roleID);err!=nil{return err};if _,err=tx.Exec("INSERT INTO user_roles(user_id,role_id) VALUES(?,?)",id,roleID);err!=nil{return err};if _,err=tx.Exec("DELETE FROM user_sections WHERE user_id=?",id);err!=nil{return err};if _,err=tx.Exec("DELETE FROM user_cameras WHERE user_id=?",id);err!=nil{return err}
	if p.Scope=="section"{for _,sid:=range p.SectionIDs{if _,err=tx.Exec("INSERT IGNORE INTO user_sections(user_id,section_id) VALUES(?,?)",id,sid);err!=nil{return err}}};if p.Scope=="selected"{for _,cid:=range p.CameraIDs{if _,err=tx.Exec("INSERT IGNORE INTO user_cameras(user_id,camera_id) VALUES(?,?)",id,cid);err!=nil{return err}}};return tx.Commit()
}
