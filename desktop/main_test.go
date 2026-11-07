package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCaptureWorkflowAndZIPIntegrity(t *testing.T) {
	root := t.TempDir()
	a := &app{s: &session{Player: "Test Player", Email: "test.player@example.com", Slot: 1, Root: root}}
	old := []byte("ES2 OLD sample\x00\x12")
	if err := a.capture("new", bytes.NewReader(old), "d.es"); err == nil {
		t.Fatal("allowed new save before verified backup")
	}
	if err := a.capture("old", bytes.NewReader(old), "bad.txt"); err == nil {
		t.Fatal("accepted non-es file")
	}
	if err := a.capture("old", bytes.NewReader(old), "d.es"); err != nil {
		t.Fatal(err)
	}
	if !a.s.Ready || a.s.Backup == "" {
		t.Fatal("did not create backup")
	}
	if _, err := os.Stat(a.s.Backup); err != nil {
		t.Fatal("backup missing", err)
	}
	if err := verifyZip(a.s.Backup, "OLD/character.es", a.s.Old.SHA); err != nil {
		t.Fatal("backup verification failed", err)
	}
	if err := a.capture("new", bytes.NewReader([]byte("ES2 NEW sample")), "d.es"); err == nil {
		t.Fatal("allowed new before confirmation")
	}
	a.s.Confirmed = true
	if err := a.capture("new", bytes.NewReader(old), "d.es"); err == nil {
		t.Fatal("allowed identical new save")
	}
	if err := a.capture("new", bytes.NewReader([]byte("ES2 NEW sample")), "d.es"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/finalize", strings.NewReader("{}"))
	req.Header.Set("X-PSG3-Token", "x")
	w := httptest.NewRecorder()
	a.token = "x"
	a.finalize(w, req)
	if w.Code != 200 {
		t.Fatalf("finalize failed %d: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(a.s.Package); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ entry, hash string }{{"old/character.es", a.s.Old.SHA}, {"new/character.es", a.s.New.SHA}} {
		if err := verifyZip(a.s.Package, item.entry, item.hash); err != nil {
			t.Fatal(err)
		}
	}

    reader,err:=zip.OpenReader(a.s.Package)
    if err!=nil{t.Fatal(err)}
    defer reader.Close()
    foundEmail:=false
    for _,file:=range reader.File {
        if file.Name!="SUBMISSION_INFO.json"{continue}
        rd,err:=file.Open();if err!=nil{t.Fatal(err)}
        data,err:=io.ReadAll(rd);_ = rd.Close();if err!=nil{t.Fatal(err)}
        var info struct{Email string `json:"email"`}
        if err:=json.Unmarshal(data,&info);err!=nil{t.Fatal(err)}
        foundEmail=info.Email=="test.player@example.com"
    }
    if !foundEmail{t.Fatal("submission ZIP must include return email")}
    if filepath.Base(a.s.Package) != "Test_Player.zip" {
		t.Fatalf("submission filename not based on player: %s", a.s.Package)
	}
	if filepath.Dir(a.s.Package) != root {
		t.Fatalf("unexpected package dir: %s", a.s.Package)
	}
}
func TestAPIProtectionAndLabels(t *testing.T) {
	a := &app{s: &session{}, token: "secret"}
	req := httptest.NewRequest("POST", "/api/start", strings.NewReader(`{"player":"X","slot":1}`))
	w := httptest.NewRecorder()
	a.start(w, req)
	if w.Code != 403 {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	if sanitize("!! A / B @") != "A_B" {
		t.Fatalf("unexpected player label %s", sanitize("!! A / B @"))
	}
	if sanitize("../\\") != "Player" {
		t.Fatal("path traversal sanitizer invalid")
	}
	var m message
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil || m.Error == "" {
		t.Fatalf("missing structured error: %+v %v", m, err)
	}
}
func TestNoBodyLeakInStatus(t *testing.T) {
	a := &app{s: &session{Player: "Test", Slot: 1, Root: "C:/Backups", Old: &selectedSave{Path: "C:/private/file.es", Name: "d.es", SHA: "abc", Size: 10}}}
	w := httptest.NewRecorder()
	a.status(w, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	data, err := io.ReadAll(w.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("C:/private/file.es")) {
		t.Fatal("leaked internal staged source path")
	}
}

func TestBackupsUnderDownloads(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root, err := rootDir("Saved Player", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(root, filepath.Join("Downloads", "PSG3 Collector Backups")) {
		t.Fatalf("backup is not under Downloads: %s", root)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateGitHubUploadAndPublicRefusal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test mocks GitHub CLI via POSIX script")
	}
	dir := t.TempDir()
	cli := filepath.Join(dir, "gh")
	calls := filepath.Join(dir, "calls")
	body := filepath.Join(dir, "upload.json")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$GH_CALLS"
case "$*" in
  *--method*PUT*) cat > "$GH_BODY"; printf '{"content":{"html_url":"https://github.com/example-owner/example-private-repo/blob/main/submissions/file.zip"}}' ;;
  *) printf '{"private":%s}' "$GH_PRIVATE" ;;
esac
`
	if err := os.WriteFile(cli, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_CALLS", calls)
	t.Setenv("GH_BODY", body)
	zipPath := filepath.Join(dir, "test.zip")
	if err := os.WriteFile(zipPath, []byte("sample private ZIP bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_PRIVATE", "false")
	if _, err := uploadPrivateGitHub(context.Background(), defaultPrivateRepo, zipPath); err == nil || !strings.Contains(err.Error(), "PUBLIC") {
		t.Fatalf("public repository should block upload; err=%v", err)
	}
	if _, err := os.Stat(body); !os.IsNotExist(err) {
		t.Fatal("sent content to public repository")
	}
	t.Setenv("GH_PRIVATE", "true")
	url, err := uploadPrivateGitHub(context.Background(), defaultPrivateRepo, zipPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(url, "example-private-repo") {
		t.Fatal("invalid upload URL:", url)
	}
	payload, err := os.ReadFile(body)
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(payload, &request); err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(request.Content)
	if err != nil || string(decoded) != "sample private ZIP bytes" {
		t.Fatalf("payload mismatched: %v", err)
	}
	if _, err := uploadPrivateGitHub(context.Background(), "other-user/public-repo", zipPath); err == nil {
		t.Fatal("accepted arbitrary owner")
	}
}

func TestNewInterfaceHasNoADBAndPrivateUpload(t *testing.T) {
	if strings.Contains(strings.ToLower(htmlUI), "adb") {
		t.Fatal("ADB references remain in desktop interface")
	}
	for _, value := range []string{"How the save collector works", "Back up OLD .es to Downloads", "USB file transfer", "id=\"submit\""} {
		if !strings.Contains(htmlUI, value) {
			t.Fatalf("missing UI: %s", value)
		}
	}
}
func TestSubmissionInterfaceOneButton(t *testing.T) {
 if strings.Contains(htmlUI, "Build ZIP + Upload to private GitHub") || strings.Contains(htmlUI, "id=\"finalize\"") || strings.Contains(htmlUI, "id=\"repo\"") {
  t.Fatal("legacy submission controls remain")
 }
 if strings.Count(htmlUI, "USB file transfer") < 2 {
  t.Fatal("old and new collection steps should have file-transfer instructions")
 }
 if !strings.Contains(htmlUI, "id=\"submit\"") || !strings.Contains(htmlUI, "Submitted") {
  t.Fatal("missing single submit button and confirmed status")
 }
}


func TestHTTPIntakeSubmission(t *testing.T) {
    expected:=[]byte("ZIP_TEST_BYTES")
    endpoint:=httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
        if r.Method!=http.MethodPost||r.URL.Path!="/submit"{t.Errorf("unexpected route %s %s",r.Method,r.URL.Path);return}
        if r.Header.Get("Content-Type")!="application/zip"{t.Error("unexpected content type")}
        data,err:=io.ReadAll(r.Body);if err!=nil{t.Error(err);return}
        if !bytes.Equal(data,expected){t.Error("payload mismatch")}
        w.Header().Set("Content-Type","application/json")
        w.WriteHeader(http.StatusCreated)
        _,_=io.WriteString(w,`{"url":"https://github.com/example-owner/example-private-repo/blob/main/submissions/abc.zip"}`)
    }))
    defer endpoint.Close()
    p:=filepath.Join(t.TempDir(),"Alex.zip")
    if err:=os.WriteFile(p,expected,0600);err!=nil{t.Fatal(err)}
    url,err:=uploadToIntakeWithClient(context.Background(),endpoint.URL+"/submit",p,endpoint.Client())
    if err!=nil{t.Fatal(err)}
    if !strings.HasPrefix(url,"https://github.com/"){t.Fatal("invalid receipt:",url)}
    if _,err:=uploadToIntakeWithClient(context.Background(),"http://example.com/submit",p,endpoint.Client());err==nil {
        t.Fatal("non-HTTPS intake URL accepted")
    }
}
func TestUIShowsProductNameWithoutExtraFooter(t *testing.T) {
    if !strings.Contains(htmlUI, "PSG3 Save Collector") {
        t.Fatal("missing application name")
    }
    if strings.Contains(htmlUI, "if this PC is authorised") {
        t.Fatal("old submission wording still present")
    }
}

func TestExplorerButtonsRemoved(t *testing.T) {
    if strings.Contains(htmlUI, "id=\"openPhoneOld\"") || strings.Contains(htmlUI, "id=\"openPhoneNew\"") {
        t.Fatal("obsolete Explorer buttons remain")
    }
    if !strings.Contains(htmlUI, "This PC → your phone") {
        t.Fatal("phone browsing instructions should remain in the old/new steps")
    }
}

func TestSubmissionSuccessDoesNotShowDestination(t *testing.T) {
    if !strings.Contains(htmlUI, "Successfully submitted.") {
        t.Fatal("missing concise submission confirmation")
    }
    if strings.Contains(htmlUI, "Submitted successfully.\\n") || strings.Contains(htmlUI, "\\n\x27+state.uploadedUrl") {
        t.Fatal("success message still displays submission destination")
    }
}

func TestPlayerEmailRequiredAtStart(t *testing.T) {
    t.Setenv("HOME",t.TempDir())
    a:=&app{s:&session{},token:"secret"}
    for _,email:=range []string{"", "not-an-email", "x@localhost", "Name <name@example.com>", "x@example.com\r\nBcc:evil@example.com"} {
        payload,_:=json.Marshal(map[string]any{"player":"Tester","email":email,"slot":1})
        req:=httptest.NewRequest(http.MethodPost,"/api/start",bytes.NewReader(payload))
        req.Header.Set("X-PSG3-Token","secret")
        w:=httptest.NewRecorder()
        a.start(w,req)
        if w.Code!=http.StatusBadRequest{t.Fatalf("invalid email %q returned HTTP %d: %s",email,w.Code,w.Body.String())}
        if a.s.Root!=""{t.Fatal("invalid email created a session")}
    }
    payload:=`{"player":"Tester","email":"test+return@example.com","slot":2}`
    req:=httptest.NewRequest(http.MethodPost,"/api/start",strings.NewReader(payload))
    req.Header.Set("X-PSG3-Token","secret")
    w:=httptest.NewRecorder()
    a.start(w,req)
    if w.Code!=200||a.s.Email!="test+return@example.com"||a.s.Slot!=2{t.Fatalf("valid email not saved: %d %s",w.Code,w.Body.String())}
}
func TestEmailFieldShownInCollector(t *testing.T) {
    for _,item:=range []string{`id="email"`,`type="email"`,`email:$('email').value.trim()`,`Successfully submitted.`}{
        if !strings.Contains(htmlUI,item){t.Fatalf("missing collector UI component %s",item)}
    }
}
