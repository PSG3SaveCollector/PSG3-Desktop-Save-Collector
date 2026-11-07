package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/mail"
	"net/http"
    "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

//go:embed index.html
var htmlUI string

const version = "1.3.5"
const defaultPrivateRepo = "example-owner/example-private-repo"
const maxSaveSize = 16 << 20

// HTTPS submission service address, injected as a build variable after deployment.
// No GitHub credentials are embedded in the desktop application.
var receiverURL string

var safePart = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)
var privateRepoPattern = regexp.MustCompile(`^example-owner/[A-Za-z0-9_.-]+$`)

type selectedSave struct {
	Path string `json:"-"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	SHA  string `json:"sha256"`
}
type session struct {
	sync.Mutex
	Player      string        `json:"player"`
	Email       string        `json:"email"`
	Slot        int           `json:"slot"`
	Root        string        `json:"root"`
	Old         *selectedSave `json:"old"`
	New         *selectedSave `json:"new"`
	Backup      string        `json:"backup"`
	Package     string        `json:"package"`
	Ready       bool          `json:"ready"`
	Confirmed   bool          `json:"confirmed"`
	Uploading   bool          `json:"uploading"`
	UploadedURL string        `json:"uploadedUrl,omitempty"`
}
type app struct {
	s      *session
	token  string
	server *http.Server
}
type message struct {
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
	Data    any    `json:"data,omitempty"`
}

func main() {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		log.Fatal(err)
	}
	a := &app{s: &session{}, token: hex.EncodeToString(b)}
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.index)
	mux.HandleFunc("/api/status", a.status)
	mux.HandleFunc("/api/start", a.start)
	mux.HandleFunc("/api/upload/old", a.upload("old"))
	mux.HandleFunc("/api/upload/new", a.upload("new"))
	mux.HandleFunc("/api/confirm", a.confirm)
	mux.HandleFunc("/api/finalize", a.finalize)
	mux.HandleFunc("/api/submit", a.submit)
	mux.HandleFunc("/api/open", a.open)
	mux.HandleFunc("/api/quit", a.quit)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	a.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	url := "http://" + listener.Addr().String() + "/"
	go func() { time.Sleep(500 * time.Millisecond); openURL(url) }()
	if err := a.server.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
func (a *app) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, strings.Replace(htmlUI, "__SECURE_TOKEN__", a.token, 1))
}
func respond(w http.ResponseWriter, status int, val any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(val)
}
func fail(w http.ResponseWriter, status int, err error) {
	respond(w, status, message{Error: err.Error()})
}
func (a *app) auth(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != "POST" {
		fail(w, 405, errors.New("POST required"))
		return false
	}
	if r.Header.Get("X-PSG3-Token") != a.token {
		fail(w, 403, errors.New("Request verification failed"))
		return false
	}
	return true
}
func (a *app) status(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "GET required", 405)
		return
	}
	a.s.Lock()
	defer a.s.Unlock()
	respond(w, 200, a.s)
}
func sanitize(s string) string {
	s = safePart.ReplaceAllString(strings.TrimSpace(s), "_")
	s = strings.Trim(s, "_-")
	if len(s) > 42 {
		s = s[:42]
	}
	if s == "" {
		s = "Player"
	}
	return s
}

func validReturnEmail(value string) bool {
    if len(value) < 5 || len(value) > 254 || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n\t ") {
        return false
    }
    address, err := mail.ParseAddress(value)
    if err != nil || address.Address != value { return false }
    at := strings.LastIndexByte(value, '@')
    return at > 0 && at < len(value)-1 && strings.Contains(value[at+1:], ".")
}

func rootDir(player string, slot int) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	downloads := filepath.Join(home, "Downloads")
	root := filepath.Join(downloads, "PSG3 Collector Backups", fmt.Sprintf("%s_Slot%d_%s", sanitize(player), slot, time.Now().Format("20060102_150405")))
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	return root, nil
}
func (a *app) start(w http.ResponseWriter, r *http.Request) {
	if !a.auth(w, r) {
		return
	}
	var req struct {
		Player string `json:"player"`
		Email  string `json:"email"`
		Slot   int    `json:"slot"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		fail(w, 400, err)
		return
	}
	if strings.TrimSpace(req.Player) == "" || len(req.Player) > 100 || req.Slot < 1 || req.Slot > 3 {
		fail(w, 400, errors.New("Enter a player name and choose slot 1, 2 or 3"))
		return
	}
	if !validReturnEmail(req.Email) {
		fail(w, 400, errors.New("Enter a valid email address for your returned save"))
		return
	}
	a.s.Lock()
	defer a.s.Unlock()
	if a.s.Old != nil {
		fail(w, 409, errors.New("Old save is already backed up; restart this application to begin a different session"))
		return
	}
	root, err := rootDir(req.Player, req.Slot)
	if err != nil {
		fail(w, 500, err)
		return
	}
	a.s.Player = req.Player
	a.s.Email = req.Email
	a.s.Slot = req.Slot
	a.s.Root = root
	respond(w, 200, message{Message: "Session created. Old save backups will be stored under Downloads/PSG3 Collector Backups."})
}
func fileHash(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}
func writeZip(path string, files map[string]string, metadata any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	z := zip.NewWriter(f)
	if metadata != nil {
		e, err := z.Create("SUBMISSION_INFO.json")
		if err != nil {
			f.Close()
			return err
		}
		b, err := json.MarshalIndent(metadata, "", "  ")
		if err != nil {
			f.Close()
			return err
		}
		if _, err = e.Write(b); err != nil {
			f.Close()
			return err
		}
	}
	for entry, source := range files {
		fin, err := os.Open(source)
		if err != nil {
			f.Close()
			return err
		}
		out, err := z.Create(entry)
		if err == nil {
			_, err = io.Copy(out, fin)
		}
		fin.Close()
		if err != nil {
			f.Close()
			return err
		}
	}
	if err = z.Close(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
func verifyZip(zipPath, entry, sha string) error {
	f, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, z := range f.File {
		if z.Name != entry {
			continue
		}
		rc, err := z.Open()
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, rc)
		rc.Close()
		if err != nil {
			return err
		}
		if hex.EncodeToString(h.Sum(nil)) != sha {
			return errors.New("ZIP backup checksum mismatch")
		}
		return nil
	}
	return errors.New("Backup missing source save")
}
func (a *app) capture(kind string, src io.Reader, filename string) error {
	a.s.Lock()
	defer a.s.Unlock()
	if a.s.Root == "" {
		return errors.New("Start a session first")
	}
	if kind == "new" && (!a.s.Ready || !a.s.Confirmed) {
		return errors.New("Verify the old backup and confirm before collecting the new file")
	}
	if !strings.HasSuffix(strings.ToLower(filename), ".es") {
		return errors.New("Select a .es character save")
	}
	if kind == "old" && a.s.Old != nil {
		return errors.New("Old backup is already captured; restart to create a different session")
	}
	label := "OLD"
	if kind == "new" {
		label = "NEW"
	}
	dir := filepath.Join(a.s.Root, label)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	path := filepath.Join(dir, "character.es")
	if kind == "new" && a.s.New != nil {
		return errors.New("New save has already been captured")
	}
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(out, io.LimitReader(src, maxSaveSize+1))
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil || n == 0 || n > maxSaveSize {
		os.Remove(path)
		return errors.New("Could not copy save (file must be 1 byte to 16 MB)")
	}
	sha, size, err := fileHash(path)
	if err != nil {
		os.Remove(path)
		return err
	}
	record := &selectedSave{Path: path, Name: filename, Size: size, SHA: sha}
	if kind == "old" {
		backup := filepath.Join(a.s.Root, "BACKUP_OLD_Slot"+fmt.Sprint(a.s.Slot)+".zip")
		meta := map[string]any{"player": a.s.Player, "slot": a.s.Slot, "sourceFile": filename, "sha256": sha, "createdAt": time.Now().UTC().Format(time.RFC3339), "purpose": "Verified old-save backup; original game save was not modified"}
		if err := writeZip(backup, map[string]string{"OLD/character.es": path}, meta); err != nil {
			return err
		}
		if err := verifyZip(backup, "OLD/character.es", sha); err != nil {
			return err
		}
		a.s.Old = record
		a.s.Backup = backup
		a.s.Ready = true
	} else {
		if a.s.Old.SHA == sha {
			os.Remove(path)
			return errors.New("The new file is identical to the old file; select the newly created character save")
		}
		a.s.New = record
	}
	return nil
}
func (a *app) upload(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.auth(w, r) {
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxSaveSize+(2<<20))
		file, header, err := r.FormFile("save")
		if err != nil {
			fail(w, 400, errors.New("Choose an .es file"))
			return
		}
		defer file.Close()
		if err := a.capture(kind, file, filepath.Base(header.Filename)); err != nil {
			fail(w, 400, err)
			return
		}
		if kind == "old" {
			a.s.Lock()
			backup := a.s.Backup
			a.s.Unlock()
			respond(w, 200, message{Message: "Old save safely backed up and verified in Downloads: " + backup})
		} else {
			respond(w, 200, message{Message: "NEW save copied and verified locally"})
		}
	}
}
func (a *app) confirm(w http.ResponseWriter, r *http.Request) {
	if !a.auth(w, r) {
		return
	}
	var req struct {
		Confirmed bool `json:"confirmed"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil {
		fail(w, 400, err)
		return
	}
	a.s.Lock()
	defer a.s.Unlock()
	if !a.s.Ready {
		fail(w, 409, errors.New("Backup must be verified first"))
		return
	}
	a.s.Confirmed = req.Confirmed
	respond(w, 200, message{Message: "Backup confirmation recorded"})
}

// finalizeLocked builds a verified local ZIP. Caller must hold a.s.Mutex.
func (a *app) finalizeLocked() (string, error) {
	if a.s.Old == nil || a.s.New == nil || !a.s.Ready || !a.s.Confirmed {
		return "", errors.New("Old/new saves, verified Downloads backup and confirmation are all required")
	}
	if a.s.Package != "" {
		return a.s.Package, nil
	}
	filename := sanitize(a.s.Player) + ".zip"
	dest := filepath.Join(a.s.Root, filename)
	meta := map[string]any{
		"player": a.s.Player, "characterSlot": a.s.Slot, "email": a.s.Email,
		"createdAtUTC": time.Now().UTC().Format(time.RFC3339),
		"oldFilename":  a.s.Old.Name, "newFilename": a.s.New.Name,
		"oldSHA256": a.s.Old.SHA, "newSHA256": a.s.New.SHA,
		"instructions": "Player provided old and new character saves. Original game data was not modified by this tool.",
	}
	if err := writeZip(dest, map[string]string{"old/character.es": a.s.Old.Path, "new/character.es": a.s.New.Path}, meta); err != nil {
		return "", err
	}
	if err := verifyZip(dest, "old/character.es", a.s.Old.SHA); err != nil {
		return "", err
	}
	if err := verifyZip(dest, "new/character.es", a.s.New.SHA); err != nil {
		return "", err
	}
	a.s.Package = dest
	return dest, nil
}
func (a *app) finalize(w http.ResponseWriter, r *http.Request) {
	if !a.auth(w, r) {
		return
	}
	a.s.Lock()
	defer a.s.Unlock()
	dest, err := a.finalizeLocked()
	if err != nil {
		fail(w, 409, err)
		return
	}
	respond(w, 200, message{Message: "Submission ZIP created and verified in Downloads.", Data: map[string]any{"package": dest, "backup": a.s.Backup}})
}

// uploadPrivateGitHub requires an already authenticated GitHub CLI and refuses public repositories.
// A distributed EXE has no embedded token and cannot grant write access to third-party players.
func uploadPrivateGitHub(ctx context.Context, repo, zipPath string) (string, error) {
	if !privateRepoPattern.MatchString(repo) {
		return "", errors.New("GitHub repository must be owned by example-owner")
	}
	gh, err := exec.LookPath("gh")
	if err != nil {
		return "", errors.New("GitHub CLI not found. Install gh from cli.github.com and sign in using 'gh auth login' on the OWNER PC")
	}
	check := exec.CommandContext(ctx, gh, "api", "repos/"+repo)
	result, err := check.CombinedOutput()
	if err != nil {
		return "", errors.New("GitHub access failed. Make sure the private repository exists and you ran 'gh auth login' on this PC")
	}
	var info struct {
		Private bool `json:"private"`
	}
	if err := json.Unmarshal(result, &info); err != nil {
		return "", errors.New("Could not check GitHub repository privacy")
	}
	if !info.Private {
		return "", errors.New("UPLOAD BLOCKED: repository is PUBLIC. Create a separate private submissions repository before uploading player saves")
	}
	stat, err := os.Stat(zipPath)
	if err != nil {
		return "", err
	}
	if stat.Size() > 75<<20 {
		return "", errors.New("ZIP is too large for this GitHub uploader (75 MB max)")
	}
	raw, err := os.ReadFile(zipPath)
	if err != nil {
		return "", err
	}
	// Unique directory prevents collisions while keeping each player's ZIP separate.
	uploadPath := "submissions/" + time.Now().UTC().Format("2006-01-02") + "/" +
		time.Now().UTC().Format("150405") + "_" + filepath.Base(zipPath)
	payload, err := json.Marshal(map[string]string{
		"message": "Add private player save submission",
		"content": base64.StdEncoding.EncodeToString(raw),
	})
	if err != nil {
		return "", err
	}
	command := exec.CommandContext(ctx, gh, "api", "--method", "PUT",
		"repos/"+repo+"/contents/"+uploadPath, "--input", "-")
	command.Stdin = bytes.NewReader(payload)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", errors.New("Private GitHub upload failed. Check gh authentication and private repository write access; local ZIP remains safely in Downloads")
	}
	var response struct {
		Content struct {
			HTMLURL string `json:"html_url"`
		} `json:"content"`
	}
	if err := json.Unmarshal(output, &response); err != nil || response.Content.HTMLURL == "" {
		return "", errors.New("GitHub returned an unexpected upload response; check the repository before retrying")
	}
	return response.Content.HTMLURL, nil
}

type intakeReply struct {
    URL string `json:"url"`
    Error string `json:"error"`
}

// uploadToIntake sends the already verified ZIP to the collection owner's HTTPS
// intake service. The receiver performs the private GitHub upload on its side.
func uploadToIntake(ctx context.Context, endpoint, zipPath string) (string, error) {
    client := &http.Client{Timeout: 120 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
    return uploadToIntakeWithClient(ctx, endpoint, zipPath, client)
}
func uploadToIntakeWithClient(ctx context.Context, endpoint, zipPath string, client *http.Client) (string, error) {
    u, err := url.Parse(strings.TrimSpace(endpoint))
    if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
        return "", errors.New("The submission service URL is not configured correctly")
    }
    f, err := os.Open(zipPath)
    if err != nil { return "", err }
    defer f.Close()
    stat, err := f.Stat()
    if err != nil { return "", err }
    if stat.Size() < 1 || stat.Size() > 40<<20 {
        return "", errors.New("Submission ZIP is outside the accepted size range")
    }
    req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, f)
    if err != nil { return "", err }
    req.Header.Set("Content-Type", "application/zip")
    req.Header.Set("X-PSG3-Filename", filepath.Base(zipPath))
    req.ContentLength = stat.Size()
    resp, err := client.Do(req)
    if err != nil { return "", errors.New("Could not reach the save submission service; the ZIP remains in Downloads") }
    defer resp.Body.Close()
    var receipt intakeReply
    if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&receipt); err != nil {
        return "", errors.New("The submission service returned an invalid receipt")
    }
    if resp.StatusCode != http.StatusCreated || receipt.URL == "" {
        if resp.StatusCode == http.StatusTooManyRequests { return "", errors.New("The submission service is busy. Please try again later") }
        if resp.StatusCode == http.StatusRequestEntityTooLarge { return "", errors.New("Submission ZIP is too large") }
        return "", errors.New("Submission was not accepted; your backup and ZIP remain in Downloads")
    }
    uploaded, err := url.Parse(receipt.URL)
    if err != nil || uploaded.Scheme != "https" || uploaded.Host != "github.com" {
        return "", errors.New("The submission service returned an unverified GitHub destination")
    }
    return receipt.URL, nil
}

func (a *app) submit(w http.ResponseWriter, r *http.Request) {
	if !a.auth(w, r) {
		return
	}
	a.s.Lock()
	if a.s.UploadedURL != "" {
		url := a.s.UploadedURL
		a.s.Unlock()
		respond(w, 200, message{Message: "Already uploaded to private GitHub", Data: map[string]string{"url": url}})
		return
	}
	if a.s.Uploading {
		a.s.Unlock()
		fail(w, 409, errors.New("Upload is already running"))
		return
	}
	pkg, err := a.finalizeLocked()
	if err != nil {
		a.s.Unlock()
		fail(w, 409, err)
		return
	}
	a.s.Uploading = true
	a.s.Unlock()
	defer func() { a.s.Lock(); a.s.Uploading = false; a.s.Unlock() }()
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	var url string
	if strings.TrimSpace(receiverURL) != "" {
		url, err = uploadToIntake(ctx, receiverURL, pkg)
	} else {
		err = errors.New("Online submissions are not set up yet. Your backup and named ZIP have been safely saved in Downloads.")
	}
	if err != nil {
		fail(w, 400, err)
		return
	}
	a.s.Lock()
	a.s.UploadedURL = url
	a.s.Unlock()
	respond(w, 200, message{Message: "Submitted", Data: map[string]string{"url": url}})
}
func (a *app) open(w http.ResponseWriter, r *http.Request) {
	if !a.auth(w, r) {
		return
	}
	var req struct {
		What string `json:"what"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil {
		fail(w, 400, err)
		return
	}
	switch req.What {
	case "backup":
		a.s.Lock()
		p := a.s.Root
		a.s.Unlock()
		if p == "" {
			fail(w, 400, errors.New("Start a session first"))
			return
		}
		if runtime.GOOS == "windows" {
			_ = exec.Command("explorer.exe", p).Start()
		}
	default:
		fail(w, 400, errors.New("Unknown action"))
		return
	}
	respond(w, 200, message{Message: "Opened"})
}
func openURL(url string) {
	if runtime.GOOS == "windows" {
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	} else if runtime.GOOS == "darwin" {
		_ = exec.Command("open", url).Start()
	} else {
		_ = exec.Command("xdg-open", url).Start()
	}
}
func (a *app) quit(w http.ResponseWriter, r *http.Request) {
	if !a.auth(w, r) {
		return
	}
	respond(w, 200, message{Message: "Closing"})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		a.server.Shutdown(ctx)
	}()
}