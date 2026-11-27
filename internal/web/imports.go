package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"skiff/internal/laravel"
	"skiff/internal/store"
)

// Importing a database from a file: uploaded in the browser, or copied to
// laravel.ImportRoot with scp for big dumps. The browser sends the file in
// chunks small enough for the panel's Nginx proxy (client_max_body_size 4m),
// so any size works with no proxy changes, and appends them to
// ImportRoot/upload-<id>.part. Starting the import turns it into a backup of
// the target database and restores that (see importDatabase).

const (
	uploadRoute    = "/backups/import/upload"
	uploadChunkMax = 4 << 20 // bytes per request; the browser sends 3 MiB
	uploadPrefix   = "upload-"
	uploadSuffix   = ".part"
	// Room left on the disk beyond twice the upload (the file plus its
	// gzipped copy), so an import can't fill the disk.
	uploadHeadroom = 512 << 20
)

var uploadIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

func uploadPath(id string) string {
	return filepath.Join(laravel.ImportRoot, uploadPrefix+id+uploadSuffix)
}

// importFile is a file in ImportRoot someone copied there.
type importFile struct {
	Name     string
	Size     int64
	Modified time.Time
}

// importFiles lists the files copied to ImportRoot, newest first. Uploads in
// progress aren't listed, and nor are symlinks or anything but plain files.
func importFiles() []importFile {
	entries, err := os.ReadDir(laravel.ImportRoot)
	if err != nil {
		return nil
	}
	var out []importFile
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || strings.HasPrefix(name, uploadPrefix) || !laravel.ValidImportName(name) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, importFile{Name: name, Size: info.Size(), Modified: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out
}

// cleanStaleUploads removes uploads that were abandoned more than a day ago.
func cleanStaleUploads() {
	entries, _ := os.ReadDir(laravel.ImportRoot)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), uploadPrefix) {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
			_ = os.Remove(filepath.Join(laravel.ImportRoot, e.Name()))
		}
	}
}

type importData struct {
	Sources    []backupSource
	SourcesErr string
	Selected   string
	Files      []importFile
	Dir        string
	Host       string
	Busy       bool
}

func (s *Server) importPage(w http.ResponseWriter, r *http.Request, sess store.Session) {
	cleanStaleUploads()
	d := importData{Selected: r.URL.Query().Get("source"), Files: importFiles(), Dir: laravel.ImportRoot}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	var err error
	if d.Sources, err = s.backupSources(ctx, nil); err != nil {
		d.SourcesErr = err.Error()
	}
	d.Host = r.Host
	if h, _, ok := strings.Cut(d.Host, ":"); ok {
		d.Host = h
	}
	d.Busy, _ = s.backupJobPending()
	s.render(w, r, "backups_import", http.StatusOK, pageFor(sess, "Import a database", "backups", d))
}

// uploadChunk appends one chunk of an upload. The first chunk (no id) starts
// a new upload and says how big the whole file is, so a file that wouldn't
// fit on the disk is refused before anything is written.
func (s *Server) uploadChunk(w http.ResponseWriter, r *http.Request, _ store.Session) {
	fail := func(code int, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
	}
	q := r.URL.Query()
	id := q.Get("id")
	offset, err := strconv.ParseInt(q.Get("offset"), 10, 64)
	if err != nil || offset < 0 {
		fail(http.StatusBadRequest, "Bad offset.")
		return
	}
	var f *os.File
	if id == "" {
		total, err := strconv.ParseInt(q.Get("total"), 10, 64)
		if err != nil || total <= 0 || offset != 0 {
			fail(http.StatusBadRequest, "Bad upload size.")
			return
		}
		if err := os.MkdirAll(laravel.ImportRoot, 0o700); err != nil {
			fail(http.StatusInternalServerError, "Can't create "+laravel.ImportRoot+": "+err.Error())
			return
		}
		if free, ok := freeSpace(laravel.ImportRoot); ok && uint64(total)*2+uploadHeadroom > free {
			fail(http.StatusInsufficientStorage, fmt.Sprintf("Not enough disk space: the import needs about %s and %s is free.", humanBytes(total*2+uploadHeadroom), humanBytes(int64(free))))
			return
		}
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		id = hex.EncodeToString(b)
		f, err = os.OpenFile(uploadPath(id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	} else {
		if !uploadIDRe.MatchString(id) {
			fail(http.StatusBadRequest, "Bad upload id.")
			return
		}
		f, err = os.OpenFile(uploadPath(id), os.O_WRONLY|os.O_APPEND, 0)
	}
	if err != nil {
		fail(http.StatusNotFound, "That upload isn't on the server any more. Start again.")
		return
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || st.Size() != offset {
		fail(http.StatusConflict, "The upload got out of step. Start again.")
		return
	}
	n, err := io.Copy(f, http.MaxBytesReader(w, r.Body, uploadChunkMax))
	if err != nil {
		_ = f.Truncate(offset) // drop a partial chunk so a retry lines up
		fail(http.StatusBadRequest, "The upload was interrupted: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "size": offset + n})
}

func freeSpace(dir string) (uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), true
}

// startImport queues the import of an uploaded file or one copied to
// ImportRoot into a database.
func (s *Server) startImport(w http.ResponseWriter, r *http.Request, _ store.Session) {
	back := func(msg string) {
		setFlash(w, "err", msg)
		redirect(w, r, "/backups/import?source="+r.FormValue("source"))
	}
	src, err := s.sourceOf(r.FormValue("source"))
	if err != nil {
		back("Can't import: " + err.Error())
		return
	}
	var file string
	upload := false
	if id := r.FormValue("upload"); id != "" {
		if !uploadIDRe.MatchString(id) {
			back("Bad upload id.")
			return
		}
		file, upload = uploadPath(id), true
	} else if name := r.FormValue("file"); laravel.ValidImportName(name) && !strings.HasPrefix(name, uploadPrefix) {
		file = filepath.Join(laravel.ImportRoot, name)
	} else {
		back("Choose a file to upload or one on the server.")
		return
	}
	if st, err := os.Lstat(file); err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
		back("That file isn't on the server any more, or it's empty.")
		return
	}
	if r.FormValue("confirm") != "1" {
		back("Tick the box to confirm that " + src.Label + "'s current data is replaced.")
		return
	}
	if busy, _ := s.backupJobPending(); busy {
		back("A backup or restore is running. Try again when it's finished.")
		return
	}
	title := "Import " + filepath.Base(file) + " into " + src.Label
	if upload {
		title = "Import an uploaded file into " + src.Label
	}
	jobID, err := s.runner.Enqueue(src.SiteID, "restore", title,
		func(ctx context.Context, w io.Writer) error { return s.importDatabase(ctx, w, src, file, upload) }, nil)
	if err != nil {
		if upload {
			_ = os.Remove(file)
		}
		s.serverError(w, err)
		return
	}
	redirect(w, r, "/jobs/"+strconv.FormatInt(jobID, 10))
}

// importDatabase turns file into a backup of src and restores it. An uploaded
// file is deleted afterwards; one copied to the server is left where it is.
func (s *Server) importDatabase(ctx context.Context, w io.Writer, src backupSource, file string, upload bool) error {
	if upload {
		defer os.Remove(file)
	}
	dst := backupFile(src, store.BackupImport)
	id, err := s.st.CreateBackup(store.Backup{Source: src.ID, Label: src.Label, SiteID: src.SiteID, Reason: store.BackupImport, Path: dst})
	if err != nil {
		return err
	}
	size, err := s.deployer.PrepareImport(ctx, w, src.Kind, file, dst)
	if err != nil {
		_ = s.st.FinishBackup(id, store.BackupFailed, 0, err.Error())
		return fmt.Errorf("%w; nothing was changed", err)
	}
	if err := s.st.FinishBackup(id, store.BackupOK, size, ""); err != nil {
		return err
	}
	b, err := s.st.Backup(id)
	if err != nil {
		return err
	}
	if err := s.restore(ctx, w, src, b); err != nil {
		return err
	}
	if !upload {
		fmt.Fprintf(w, "%s is still in %s; delete it when you no longer need it.\n", filepath.Base(file), laravel.ImportRoot)
	}
	s.pruneBackups(ctx, w, src.ID)
	return nil
}
