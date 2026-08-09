// yumiboard-webui — the YUMI OS Board web store.
// A single static binary that fronts the pi-apps engine: it lists the curated
// catalog, streams install/uninstall logs, and serves the embedded UI.
// State lives on the server side only; the page never stores anything locally.
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

//go:embed static
var staticFS embed.FS

type App struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Category    string `json:"category"`
	Status      string `json:"status"`
}

type jobState struct {
	App     string `json:"app"`
	Action  string `json:"action"`
	Running bool   `json:"running"`
	Exit    int    `json:"exit"`
	Offset  int    `json:"offset"`
	Chunk   string `json:"chunk"`
}

type server struct {
	dir     string   // pi-apps checkout
	catalog []string // allowlist of app names; empty = every installable app

	mu      sync.Mutex
	logBuf  []byte
	current *jobState
}

func main() {
	dir := flag.String("dir", "/home/pi/pi-apps", "path to the pi-apps checkout")
	port := flag.Int("port", 8080, "listen port")
	catalogPath := flag.String("catalog", "", "optional file with one app name per line (curated catalog)")
	ttyd := flag.String("ttyd", "http://127.0.0.1:7681", "ttyd base URL reverse-proxied under /terminal/ (empty disables the web terminal)")
	flag.Parse()

	s := &server{dir: *dir}
	if *catalogPath != "" {
		raw, err := os.ReadFile(*catalogPath)
		if err != nil {
			log.Fatalf("catalog: %v", err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				s.catalog = append(s.catalog, line)
			}
		}
	}

	ui, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatal(err)
	}
	http.Handle("/", http.FileServer(http.FS(ui)))
	http.HandleFunc("/api/apps", s.handleApps)
	http.HandleFunc("/api/icon/", s.handleIcon)
	http.HandleFunc("/api/action", s.handleAction)
	http.HandleFunc("/api/job", s.handleJob)

	// Web terminal: ttyd (started with -b /terminal) proxied on the same port,
	// so the whole UI stays a single origin with no extra exposed service.
	if *ttyd != "" {
		ttyURL, err := url.Parse(*ttyd)
		if err != nil {
			log.Fatalf("ttyd: %v", err)
		}
		http.Handle("/terminal/", httputil.NewSingleHostReverseProxy(ttyURL))
		http.Handle("/terminal", http.RedirectHandler("/terminal/", http.StatusMovedPermanently))
	}

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("yumiboard-webui listening on %s (engine: %s)", addr, s.dir)
	log.Fatal(http.ListenAndServe(addr, nil))
}

// installable reports whether the app has a script or package list usable here.
func (s *server) installable(app string) bool {
	for _, f := range []string{"install-32", "install", "packages"} {
		if _, err := os.Stat(filepath.Join(s.dir, "apps", app, f)); err == nil {
			return true
		}
	}
	return false
}

func (s *server) appNames() []string {
	if len(s.catalog) > 0 {
		return s.catalog
	}
	entries, err := os.ReadDir(filepath.Join(s.dir, "apps"))
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && e.Name() != "template" && s.installable(e.Name()) {
			names = append(names, e.Name())
		}
	}
	return names
}

func firstLine(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(raw), "\n")
	return strings.TrimSpace(line)
}

// categories parses etc/categories ("App Name|Category[/Sub]").
func (s *server) categories() map[string]string {
	out := map[string]string{}
	raw, err := os.ReadFile(filepath.Join(s.dir, "etc", "categories"))
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(raw), "\n") {
		name, cat, ok := strings.Cut(line, "|")
		if !ok {
			continue
		}
		cat, _, _ = strings.Cut(cat, "/")
		out[name] = strings.TrimSpace(cat)
	}
	return out
}

func (s *server) handleApps(w http.ResponseWriter, r *http.Request) {
	cats := s.categories()
	var apps []App
	for _, name := range s.appNames() {
		status := firstLine(filepath.Join(s.dir, "data", "status", name))
		if status == "" {
			status = "uninstalled"
		}
		apps = append(apps, App{
			Name:        name,
			Description: firstLine(filepath.Join(s.dir, "apps", name, "description")),
			Category:    cats[name],
			Status:      status,
		})
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].Name < apps[j].Name })
	writeJSON(w, apps)
}

func (s *server) handleIcon(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/icon/")
	name = filepath.Base(name) // no traversal
	http.ServeFile(w, r, filepath.Join(s.dir, "apps", name, "icon-64.png"))
}

func (s *server) handleAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct{ App, Action string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Action != "install" && req.Action != "uninstall" {
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	valid := false
	for _, n := range s.appNames() {
		if n == req.App {
			valid = true
			break
		}
	}
	if !valid {
		http.Error(w, "unknown app", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current != nil && s.current.Running {
		http.Error(w, "a job is already running", http.StatusConflict)
		return
	}
	s.logBuf = nil
	s.current = &jobState{App: req.App, Action: req.Action, Running: true}
	go s.run(req.App, req.Action)
	writeJSON(w, s.current)
}

func (s *server) run(app, action string) {
	cmd := exec.Command("./manage", action, app)
	cmd.Dir = s.dir
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	out, err := cmd.StdoutPipe()
	if err == nil {
		cmd.Stderr = cmd.Stdout
	}
	if err != nil || cmd.Start() != nil {
		s.finish(1)
		return
	}
	buf := make([]byte, 4096)
	for {
		n, rerr := out.Read(buf)
		if n > 0 {
			s.mu.Lock()
			s.logBuf = append(s.logBuf, buf[:n]...)
			s.mu.Unlock()
		}
		if rerr != nil {
			break
		}
	}
	exit := 0
	if werr := cmd.Wait(); werr != nil {
		exit = 1
		if ee, ok := werr.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		}
	}
	s.finish(exit)
}

func (s *server) finish(exit int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current.Running = false
	s.current.Exit = exit
}

// handleJob returns job state plus the log bytes past ?offset=N.
func (s *server) handleJob(w http.ResponseWriter, r *http.Request) {
	offset := 0
	fmt.Sscanf(r.URL.Query().Get("offset"), "%d", &offset)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil {
		writeJSON(w, nil)
		return
	}
	st := *s.current
	if offset < 0 || offset > len(s.logBuf) {
		offset = 0
	}
	st.Chunk = string(s.logBuf[offset:])
	st.Offset = len(s.logBuf)
	writeJSON(w, st)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
