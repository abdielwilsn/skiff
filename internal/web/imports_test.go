package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"skiff/internal/laravel"
	"skiff/internal/store"
)

func useImportRoot(t *testing.T) string {
	t.Helper()
	old := laravel.ImportRoot
	laravel.ImportRoot = t.TempDir()
	t.Cleanup(func() { laravel.ImportRoot = old })
	return laravel.ImportRoot
}

// postChunk sends a chunk through the whole panel (auth, CSRF, body limits)
// as a logged-in admin.
var adminTokens = map[*Server]string{}

func postChunk(s *Server, query string, body []byte) (int, map[string]any) {
	if adminTokens[s] == "" {
		id, _ := s.st.CreateUser("admin", "x")
		adminTokens[s], _ = s.st.CreateSession(id, time.Hour)
		s.setupMu.Lock()
		s.setupToken = "" // as after /setup
		s.setupMu.Unlock()
	}
	sess, _ := s.st.Session(adminTokens[s])
	r := httptest.NewRequest(http.MethodPost, uploadRoute+"?"+query, bytes.NewReader(body))
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("Content-Type", "application/octet-stream")
	r.Header.Set("X-CSRF-Token", sess.CSRF)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: adminTokens[s]})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// A file uploaded in chunks lands in ImportRoot and imports into the chosen
// database: converted into an "import" backup, then restored with a safety
// backup first. The upload is deleted afterwards.
func TestUploadAndImport(t *testing.T) {
	dir := useImportRoot(t)
	s, f := newFakeServer(t)
	laravelSiteWithDatabases(t, s)

	// Chunks the size the browser sends (3 MiB), past the 2 MiB form limit.
	dump := bytes.Repeat([]byte("INSERT INTO t VALUES (1);\n"), (3<<20)/26+100)
	first := 3 << 20
	code, out := postChunk(s, "offset=0&total="+strconv.Itoa(len(dump)), dump[:first])
	if code != http.StatusOK {
		t.Fatalf("first chunk: %d %v", code, out)
	}
	id := out["id"].(string)
	// A chunk at the wrong offset is refused, not appended.
	if code, _ := postChunk(s, "id="+id+"&offset=5", dump[first:]); code != http.StatusConflict {
		t.Fatalf("out-of-step chunk: %d", code)
	}
	if code, out := postChunk(s, "id="+id+"&offset="+strconv.Itoa(first), dump[first:]); code != http.StatusOK || out["size"].(float64) != float64(len(dump)) {
		t.Fatalf("second chunk: %d %v", code, out)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "upload-"+id+".part"))
	if !bytes.Equal(got, dump) {
		t.Fatal("the uploaded file doesn't match what was sent")
	}
	// Bad ids never touch the file system.
	for _, bad := range []string{"../../etc/passwd", "abc"} {
		if code, _ := postChunk(s, "id="+url.QueryEscape(bad)+"&offset=0", []byte("x")); code != http.StatusBadRequest {
			t.Errorf("id %q: %d", bad, code)
		}
	}

	// Without the confirmation box nothing happens.
	form := url.Values{"source": {"mysql:app_example_com"}, "upload": {id}}
	if w := sitePostForm(s, s.startImport, form); w.Code != http.StatusSeeOther || f.ran("Preparing") != 0 {
		t.Fatalf("unconfirmed import: %d", w.Code)
	}
	form.Set("confirm", "1")
	if w := sitePostForm(s, s.startImport, form); w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/jobs/") {
		t.Fatalf("import: %d %s", w.Code, w.Header().Get("Location"))
	}
	jobs := waitJobs(t, s)
	if jobs[0].Status != store.JobSuccess {
		t.Fatalf("import job: %s", jobs[0].Status)
	}
	if f.ran("KIND=mysql") != 1 || f.ran("mysqldump") != 1 || f.ran("mysql_clean | runuser -u nobody") != 1 {
		t.Errorf("import didn't convert, take a safety backup and restore as the database user")
	}
	list, _ := s.st.BackupsOf("mysql:app_example_com")
	reasons := map[string]int{}
	for _, b := range list {
		reasons[b.Reason]++
	}
	if reasons[store.BackupImport] != 1 || reasons[store.BackupRestore] != 1 {
		t.Errorf("backups after import: %v", reasons)
	}
	if _, err := os.Stat(filepath.Join(dir, "upload-"+id+".part")); !os.IsNotExist(err) {
		t.Error("the uploaded file was left behind")
	}
}

// Files copied to ImportRoot are listed and importable; symlinks and uploads
// in progress aren't, and names can't leave the folder.
func TestImportFromServerFolder(t *testing.T) {
	dir := useImportRoot(t)
	s, f := newFakeServer(t)
	laravelSiteWithDatabases(t, s)
	os.WriteFile(filepath.Join(dir, "old-server.sql.gz"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(dir, "upload-0123456789abcdef0123456789abcdef.part"), []byte("x"), 0o600)
	os.Symlink("/etc/passwd", filepath.Join(dir, "passwd.sql"))

	files := importFiles()
	if len(files) != 1 || files[0].Name != "old-server.sql.gz" {
		t.Fatalf("listed: %+v", files)
	}
	for _, name := range []string{"passwd.sql", "../skiff.db", "upload-0123456789abcdef0123456789abcdef.part"} {
		form := url.Values{"source": {"mysql:app_example_com"}, "file": {name}, "confirm": {"1"}}
		sitePostForm(s, s.startImport, form)
	}
	if f.ran("Preparing") != 0 {
		t.Fatal("imported a file it shouldn't have")
	}
	form := url.Values{"source": {"mysql:app_example_com"}, "file": {"old-server.sql.gz"}, "confirm": {"1"}}
	sitePostForm(s, s.startImport, form)
	if jobs := waitJobs(t, s); len(jobs) != 1 || jobs[0].Status != store.JobSuccess {
		t.Fatalf("import from the server folder: %+v", jobs)
	}
	if _, err := os.Stat(filepath.Join(dir, "old-server.sql.gz")); err != nil {
		t.Error("a file copied to the server was deleted")
	}

	// The page lists it, with the scp hint.
	body := siteGet(t, s, s.importPage, 0)
	for _, want := range []string{"old-server.sql.gz", "scp backup.sql.gz root@", "mysql:app_example_com"} {
		if !strings.Contains(body, want) {
			t.Errorf("import page is missing %q", want)
		}
	}
}

func sitePostForm(s *Server, h handler, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h(w, r, store.Session{Username: "admin"})
	return w
}
