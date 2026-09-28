package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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
	mux.HandleFunc("/api/sections", s.sectionsAPI)
	mux.HandleFunc("/api/sections/", s.sectionAction)
	mux.HandleFunc("/api/cameras", s.camerasAPI)
	mux.HandleFunc("/api/cameras/", s.cameraAction)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
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

	decoder := xml.NewDecoder(resp.Body)
	for {
		var event hikEvent
		if err := decoder.Decode(&event); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if event.EventType != "VMD" {
			continue
		}
		switch strings.ToLower(event.EventState) {
		case "active":
			s.motionActive(camera.ID)
		case "inactive":
			s.motionInactive(camera.ID)
		}
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

	dir := filepath.Join(s.cfg.MediaDir, "recordings", id, time.Now().Format("2006-01-02"))
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
