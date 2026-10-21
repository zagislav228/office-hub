package main

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
)

//go:embed catalog.json
var catalogJSON []byte

//go:embed ui/index.html
var indexHTML string

const tailLimit = 9000

type Category struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type App struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Category string   `json:"category"`
	Replaces string   `json:"replaces"`
	Formats  []string `json:"formats"`
	Fidelity int      `json:"fidelity"`
	License  string   `json:"license"`
	Summary  string   `json:"summary"`
	Caveats  string   `json:"caveats"`
}

type Catalog struct {
	Categories []Category `json:"categories"`
	Apps       []App      `json:"apps"`
}

// JobView is the JSON shape the interface reads.
type JobView struct {
	ID        string `json:"id"`
	AppID     string `json:"appId"`
	AppName   string `json:"appName"`
	Mode      string `json:"mode"`
	Command   string `json:"command"`
	Status    string `json:"status"`
	ExitCode  int    `json:"exitCode"`
	Tail      string `json:"tail"`
	StartedAt int64  `json:"startedAt"`
	EndedAt   int64  `json:"endedAt"`
}

type job struct {
	JobView
	cmd  *exec.Cmd
	tail strings.Builder
}

type Hub struct {
	catalog Catalog
	byID    map[string]App
	idRe    map[string]*regexp.Regexp
	winget  string
	dry     bool

	mutex     sync.Mutex
	installed map[string]string
	scannedAt int64
	scanErr   string
	jobs      map[string]*job
	order     []string
}

func newHub(dry bool) (*Hub, error) {
	var catalog Catalog
	if err := json.Unmarshal(catalogJSON, &catalog); err != nil {
		return nil, fmt.Errorf("catalog.json повреждён: %w", err)
	}
	h := &Hub{
		catalog:   catalog,
		byID:      make(map[string]App, len(catalog.Apps)),
		idRe:      make(map[string]*regexp.Regexp, len(catalog.Apps)),
		winget:    findWinget(),
		dry:       dry,
		installed: map[string]string{},
		jobs:      map[string]*job{},
	}
	for _, app := range catalog.Apps {
		h.byID[app.ID] = app
		h.idRe[app.ID] = regexp.MustCompile(`(?:^|\s)` + regexp.QuoteMeta(app.ID) + `(?:\s|$)`)
	}
	return h, nil
}

func findWinget() string {
	if custom := os.Getenv("OFFICEHUB_WINGET"); custom != "" {
		return custom
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidate := filepath.Join(home, "AppData", "Local", "Microsoft", "WindowsApps", "winget.exe")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if path, err := exec.LookPath("winget"); err == nil {
		return path
	}
	return "winget"
}

type collector struct {
	mutex sync.Mutex
	sb    strings.Builder
}

func (c *collector) Write(p []byte) (int, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.sb.Write(p)
}

func (c *collector) String() string {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.sb.String()
}

func (h *Hub) runWinget(args ...string) (string, error) {
	cmd := exec.Command(h.winget, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	var out collector
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	text := out.String()
	if err != nil && text == "" {
		return "", err
	}
	return text, nil
}

func (h *Hub) rescan() {
	out, err := h.runWinget("list", "--accept-source-agreements", "--disable-interactivity")
	if err != nil {
		h.mutex.Lock()
		h.scanErr = fmt.Sprintf("winget list не удался: %v", err)
		h.scannedAt = time.Now().UnixMilli()
		h.mutex.Unlock()
		return
	}

	lines := strings.Split(out, "\n")
	found := map[string]string{}
	for _, app := range h.catalog.Apps {
		re := h.idRe[app.ID]
		for _, line := range lines {
			if !re.MatchString(line) {
				continue
			}
			cells := strings.Fields(strings.TrimSpace(line))
			version := ""
			if len(cells) > 2 {
				version = cells[2]
			}
			found[app.ID] = version
			break
		}
	}

	h.mutex.Lock()
	h.installed = found
	h.scannedAt = time.Now().UnixMilli()
	h.scanErr = ""
	h.mutex.Unlock()
}

func installArgs(appID, mode string, silent bool) []string {
	verb := "install"
	if mode == "upgrade" {
		verb = "upgrade"
	}
	args := []string{
		verb, "-e", "--id", appID, "--source", "winget",
		"--accept-source-agreements", "--accept-package-agreements",
		"--disable-interactivity",
	}
	if silent {
		args = append(args, "--silent")
	}
	return args
}

func (h *Hub) appendTail(id, chunk string) {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	j, ok := h.jobs[id]
	if !ok {
		return
	}
	j.tail.WriteString(chunk)
	if j.tail.Len() > tailLimit {
		trimmed := j.tail.String()[j.tail.Len()-tailLimit:]
		j.tail.Reset()
		j.tail.WriteString(trimmed)
	}
}

func (h *Hub) finish(id, status string, exitCode int) {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	j, ok := h.jobs[id]
	if !ok || j.Status == "canceled" {
		return
	}
	j.Status = status
	j.ExitCode = exitCode
	j.EndedAt = time.Now().UnixMilli()
}

func (h *Hub) startJob(appID, mode string, silent bool) JobView {
	app := h.byID[appID]
	args := installArgs(appID, mode, silent)
	j := &job{JobView: JobView{
		ID:        fmt.Sprintf("%d-%s", time.Now().UnixNano(), appID),
		AppID:     appID,
		AppName:   app.Name,
		Mode:      mode,
		Command:   `"` + h.winget + `" ` + strings.Join(args, " "),
		Status:    "running",
		ExitCode:  -1,
		StartedAt: time.Now().UnixMilli(),
	}}

	h.mutex.Lock()
	h.jobs[j.ID] = j
	h.order = append(h.order, j.ID)
	for len(h.order) > 40 {
		delete(h.jobs, h.order[0])
		h.order = h.order[1:]
	}
	h.mutex.Unlock()

	if h.dry {
		h.appendTail(j.ID, "Режим --dry: команда подготовлена, но winget не запускался.\n"+j.Command)
		h.finish(j.ID, "dry-run", 0)
		return h.viewOf(j.ID)
	}

	cmd := exec.Command(h.winget, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	stdout, outErr := cmd.StdoutPipe()
	stderr, errErr := cmd.StderrPipe()
	if outErr != nil || errErr != nil {
		h.appendTail(j.ID, "Не удалось наладить вывод winget: "+firstErr(outErr, errErr).Error())
		h.finish(j.ID, "failed", -1)
		return h.viewOf(j.ID)
	}
	h.mutex.Lock()
	j.cmd = cmd
	h.mutex.Unlock()

	if err := cmd.Start(); err != nil {
		h.appendTail(j.ID, "Не удалось запустить winget: "+err.Error())
		h.finish(j.ID, "failed", -1)
		return h.viewOf(j.ID)
	}

	go func() {
		var wg sync.WaitGroup
		pump := func(r io.Reader) {
			defer wg.Done()
			scanner := bufio.NewScanner(r)
			scanner.Buffer(make([]byte, 64*1024), 1024*1024)
			for scanner.Scan() {
				h.appendTail(j.ID, scanner.Text()+"\n")
			}
		}
		wg.Add(2)
		go pump(stdout)
		go pump(stderr)
		wg.Wait()

		err := cmd.Wait()
		code := 0
		if err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				code = exit.ExitCode()
			} else {
				code = -1
			}
		}
		if code == 0 {
			h.finish(j.ID, "done", 0)
			go h.rescan()
		} else {
			h.finish(j.ID, "failed", code)
		}
	}()

	return h.viewOf(j.ID)
}

func firstErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

func (h *Hub) cancelJob(id string) (JobView, error) {
	h.mutex.Lock()
	j, ok := h.jobs[id]
	if ok {
		j.Status = "canceled"
		j.EndedAt = time.Now().UnixMilli()
	}
	h.mutex.Unlock()
	if !ok || j.cmd == nil || j.cmd.Process == nil {
		return JobView{}, errors.New("задача не найдена или уже завершена")
	}
	_ = j.cmd.Process.Kill()
	return h.viewOf(id), nil
}

// viewOf copies the serializable part of a job while holding the lock.
func (h *Hub) viewOf(id string) JobView {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	j, ok := h.jobs[id]
	if !ok {
		return JobView{}
	}
	v := j.JobView
	v.Tail = j.tail.String()
	return v
}

func (h *Hub) views() []JobView {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	out := make([]JobView, 0, len(h.order))
	for i := len(h.order) - 1; i >= 0; i-- {
		j, ok := h.jobs[h.order[i]]
		if !ok {
			continue
		}
		v := j.JobView
		v.Tail = j.tail.String()
		out = append(out, v)
	}
	return out
}

func (h *Hub) writeJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}

type request struct {
	ID     string `json:"id"`
	Silent bool   `json:"silent"`
	JobID  string `json:"jobId"`
}

func decodeRequest(w http.ResponseWriter, r *http.Request) (request, bool) {
	var req request
	body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil || len(body) == 0 {
		return req, true
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return req, false
	}
	return req, true
}

func (h *Hub) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			h.writeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		w.Header().Set("content-type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, indexHTML)
	})

	mux.HandleFunc("/api/catalog", func(w http.ResponseWriter, r *http.Request) {
		h.mutex.Lock()
		payload := map[string]any{
			"categories": h.catalog.Categories,
			"apps":       h.catalog.Apps,
			"installed":  h.installed,
			"scannedAt":  h.scannedAt,
			"scanError":  h.scanErr,
			"winget":     h.winget,
			"dry":        h.dry,
		}
		h.mutex.Unlock()
		h.writeJSON(w, 200, payload)
	})

	mux.HandleFunc("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
		h.writeJSON(w, 200, h.views())
	})

	mux.HandleFunc("/api/install", func(w http.ResponseWriter, r *http.Request) { h.act(w, r, "install") })
	mux.HandleFunc("/api/upgrade", func(w http.ResponseWriter, r *http.Request) { h.act(w, r, "upgrade") })

	mux.HandleFunc("/api/cancel", func(w http.ResponseWriter, r *http.Request) {
		req, ok := decodeRequest(w, r)
		if !ok {
			h.writeJSON(w, 400, map[string]string{"error": "некорректный запрос"})
			return
		}
		view, err := h.cancelJob(req.JobID)
		if err != nil {
			h.writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		h.writeJSON(w, 200, view)
	})

	return mux
}

func (h *Hub) act(w http.ResponseWriter, r *http.Request, mode string) {
	req, ok := decodeRequest(w, r)
	if !ok {
		h.writeJSON(w, 400, map[string]string{"error": "некорректный запрос"})
		return
	}
	if _, known := h.byID[req.ID]; !known {
		h.writeJSON(w, 400, map[string]string{"error": "id отсутствует в каталоге — установка запрещена"})
		return
	}
	h.writeJSON(w, 202, h.startJob(req.ID, mode, req.Silent))
}

func messageBox(text, title string) {
	user32 := syscall.NewLazyDLL("user32.dll")
	proc := user32.NewProc("MessageBoxW")
	textPtr, _ := syscall.UTF16PtrFromString(text)
	titlePtr, _ := syscall.UTF16PtrFromString(title)
	_, _, _ = proc.Call(0, uintptr(unsafe.Pointer(textPtr)), uintptr(unsafe.Pointer(titlePtr)), 0)
}

func userDataDir() string {
	base, err := os.UserConfigDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "officehub-webview2")
	}
	dir := filepath.Join(base, "OfficeHub", "WebView2")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

func main() {
	args := os.Args[1:]
	dry := false
	for _, a := range args {
		if a == "--dry" || a == "-dry" {
			dry = true
		}
	}

	hub, err := newHub(dry)
	if err != nil {
		messageBox(err.Error(), "Office Hub не запустился")
		os.Exit(1)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		messageBox("Не удалось поднять локальный сервер: "+err.Error(), "Office Hub не запустился")
		os.Exit(1)
	}
	server := &http.Server{Handler: hub.handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()

	go hub.rescan()

	view := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     false,
		AutoFocus: true,
		DataPath:  userDataDir(),
		WindowOptions: webview2.WindowOptions{
			Title:  "Office Hub — бесплатные замены Microsoft Office",
			Width:  1320,
			Height: 880,
			Center: true,
		},
	})
	if view == nil {
		messageBox("Не удалось создать окно приложения. Нужен WebView2 Runtime — он ставится вместе с Microsoft Edge.", "Office Hub не запустился")
		os.Exit(1)
	}
	defer view.Destroy()

	port := listener.Addr().(*net.TCPAddr).Port
	view.Navigate(fmt.Sprintf("http://127.0.0.1:%d/", port))
	view.Run()
}
