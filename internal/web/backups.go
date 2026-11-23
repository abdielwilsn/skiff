package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"skiff/internal/laravel"
	"skiff/internal/store"
)

// Backup settings.
const (
	keyBackupEnabled      = "backup_enabled" // "0" turns nightly backups off; on by default
	keyBackupTime         = "backup_time"    // "HH:MM", server time
	keyBackupKeep         = "backup_keep"    // scheduled/manual backups kept per database
	keyBackupBeforeDeploy = "backup_before_deploy"
	keyBackupLastRun      = "backup_last_run" // date of the last scheduled run
	keyS3Endpoint         = "s3_endpoint"
	keyS3Region           = "s3_region"
	keyS3Bucket           = "s3_bucket"
	keyS3Prefix           = "s3_prefix"
	keyS3AccessKey        = "s3_access_key"
	keyS3SecretKey        = "s3_secret_key"
)

const (
	defaultBackupTime = "03:00"
	defaultBackupKeep = 7
	maxBackupKeep     = 90
	// Backups taken automatically before a deploy or a restore are kept in
	// addition to the scheduled ones, but only the newest few.
	keepSafetyBackups = 3
)

var backupTimeRe = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

type backupSettings struct {
	Enabled      bool
	Time         string
	Keep         int
	BeforeDeploy bool
}

func (s *Server) backupSettings() backupSettings {
	b := backupSettings{Enabled: true, Time: defaultBackupTime, Keep: defaultBackupKeep, BeforeDeploy: true}
	if v, _ := s.st.Setting(keyBackupEnabled); v == "0" {
		b.Enabled = false
	}
	if v, _ := s.st.Setting(keyBackupBeforeDeploy); v == "0" {
		b.BeforeDeploy = false
	}
	if v, _ := s.st.Setting(keyBackupTime); backupTimeRe.MatchString(v) {
		b.Time = v
	}
	if v, _ := s.st.Setting(keyBackupKeep); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= maxBackupKeep {
			b.Keep = n
		}
	}
	return b
}

func (s *Server) s3Config() laravel.S3 {
	get := func(k string) string { v, _ := s.st.Setting(k); return v }
	return laravel.S3{
		Endpoint: get(keyS3Endpoint), Region: get(keyS3Region), Bucket: get(keyS3Bucket),
		Prefix: get(keyS3Prefix), AccessKey: get(keyS3AccessKey), SecretKey: get(keyS3SecretKey),
	}
}

// backupHost names this server inside the bucket, so several servers can
// share one bucket.
func backupHost() string {
	h, _ := os.Hostname()
	h = strings.ToLower(h)
	var b strings.Builder
	for _, r := range h {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "server"
	}
	return b.String()
}

// ---- sources ----------------------------------------------------------------

// backupSource is a database Skiff can back up.
type backupSource struct {
	ID         string // store.Backup.Source
	Label      string
	Kind       string // "mysql" or "sqlite"
	DB         string // MySQL database name
	Domain     string // SQLite: the site it belongs to
	Rel        string // SQLite: path inside the site's shared/
	SiteID     *int64
	SiteDomain string
	Latest     *store.Backup
}

func (b backupSource) slug() string {
	if b.Kind == "mysql" {
		return "mysql-" + b.DB
	}
	return "sqlite-" + b.Domain + "-" + strings.ReplaceAll(b.Rel, "/", "_")
}

func (b backupSource) ext() string {
	if b.Kind == "mysql" {
		return "sql"
	}
	return "sqlite"
}

func mysqlSource(db store.Database) backupSource {
	return backupSource{ID: "mysql:" + db.Name, Label: db.Name, Kind: "mysql", DB: db.Name, SiteID: db.SiteID, SiteDomain: db.SiteDomain}
}

func sqliteSource(site store.Site, rel string) backupSource {
	id := site.ID
	return backupSource{
		ID: "sqlite:" + site.Domain + ":" + rel, Label: site.Domain + " · " + filepath.Base(rel),
		Kind: "sqlite", Domain: site.Domain, Rel: rel, SiteID: &id, SiteDomain: site.Domain,
	}
}

// backupSources lists every database on the server: ready MySQL databases and
// the non-empty SQLite databases of deployed Laravel and Python sites. If only
// is non-nil, just that site's databases.
func (s *Server) backupSources(ctx context.Context, only *store.Site) ([]backupSource, error) {
	var out []backupSource
	dbs, err := s.st.Databases()
	if err != nil {
		return nil, err
	}
	for _, db := range dbs {
		if db.Status != store.DBReady {
			continue
		}
		if only != nil && (db.SiteID == nil || *db.SiteID != only.ID) {
			continue
		}
		out = append(out, mysqlSource(db))
	}
	sites, err := s.st.Sites()
	if err != nil {
		return nil, err
	}
	bySite := map[string]store.Site{}
	var domains []string
	for _, site := range sites {
		if site.DeployedAt == nil || !(site.IsLaravel() || site.IsPython()) {
			continue
		}
		if only != nil && site.ID != only.ID {
			continue
		}
		bySite[site.Domain] = site
		domains = append(domains, site.Domain)
	}
	files, err := s.deployer.SQLiteDatabases(ctx, domains)
	if err != nil {
		return out, fmt.Errorf("look for SQLite databases: %w", err)
	}
	for _, dom := range domains {
		for _, rel := range files[dom] {
			out = append(out, sqliteSource(bySite[dom], rel))
		}
	}
	latest, err := s.st.LatestBackups()
	if err == nil {
		for i := range out {
			if b, ok := latest[out[i].ID]; ok {
				out[i].Latest = &b
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out, nil
}

// sourceOf rebuilds a backup's source from its ID, checking it still exists.
func (s *Server) sourceOf(id string) (backupSource, error) {
	kind, rest, _ := strings.Cut(id, ":")
	switch kind {
	case "mysql":
		dbs, err := s.st.Databases()
		if err != nil {
			return backupSource{}, err
		}
		for _, db := range dbs {
			if db.Name == rest && db.Status == store.DBReady {
				return mysqlSource(db), nil
			}
		}
		return backupSource{}, fmt.Errorf("the MySQL database %s doesn't exist any more", rest)
	case "sqlite":
		domain, rel, _ := strings.Cut(rest, ":")
		if _, ok := laravel.SQLitePath(domain, rel); !ok {
			return backupSource{}, fmt.Errorf("unknown SQLite database")
		}
		sites, err := s.st.Sites()
		if err != nil {
			return backupSource{}, err
		}
		for _, site := range sites {
			if site.Domain == domain {
				return sqliteSource(site, rel), nil
			}
		}
		return backupSource{}, fmt.Errorf("the site %s doesn't exist any more", domain)
	}
	return backupSource{}, fmt.Errorf("unknown backup source")
}

// ---- running backups ----------------------------------------------------------

// runBackup dumps one database, copies it off-site when storage is set up, and
// prunes that database's old backups. An off-site failure is reported in the
// log but doesn't fail the backup: the local copy is fine.
func (s *Server) runBackup(ctx context.Context, w io.Writer, src backupSource, reason string) error {
	file := fmt.Sprintf("%s/%s/%s-%s.%s.gz", laravel.BackupRoot, src.slug(), time.Now().UTC().Format("20060102-150405"), reason, src.ext())
	id, err := s.st.CreateBackup(store.Backup{Source: src.ID, Label: src.Label, SiteID: src.SiteID, Reason: reason, Path: file})
	if err != nil {
		return err
	}
	var size int64
	if src.Kind == "mysql" {
		size, err = s.deployer.BackupMySQL(ctx, w, src.DB, file)
	} else {
		path, _ := laravel.SQLitePath(src.Domain, src.Rel)
		size, err = s.deployer.BackupSQLite(ctx, w, path, file)
	}
	if err != nil {
		_ = s.st.FinishBackup(id, store.BackupFailed, 0, err.Error())
		return fmt.Errorf("back up %s: %w", src.Label, err)
	}
	if err := s.st.FinishBackup(id, store.BackupOK, size, ""); err != nil {
		return err
	}
	if c := s.s3Config(); c.Configured() {
		key := c.ObjectKey(backupHost(), file)
		if err := s.deployer.Upload(ctx, w, c, file, key); err != nil {
			fmt.Fprintf(w, "Warning: the off-site copy failed (%v). The backup is kept on this server.\n", err)
		} else {
			_ = s.st.SetBackupRemote(id, key)
		}
	}
	s.pruneBackups(ctx, w, src.ID)
	return nil
}

// pruneBackups keeps a source's newest Keep scheduled/manual backups and its
// newest few safety backups (before deploys/restores), deleting the rest
// locally and off-site.
func (s *Server) pruneBackups(ctx context.Context, w io.Writer, source string) {
	list, err := s.st.BackupsOf(source)
	if err != nil {
		return
	}
	keep := s.backupSettings().Keep
	c := s.s3Config()
	var regular, safety int
	for _, b := range list {
		if b.Status == store.BackupRunning {
			continue
		}
		limit, n := keep, &regular
		if b.Reason == store.BackupDeploy || b.Reason == store.BackupRestore {
			limit, n = keepSafetyBackups, &safety
		}
		if b.Status == store.BackupOK {
			*n++
			if *n <= limit {
				continue
			}
		} else if time.Since(b.CreatedAt) < 7*24*time.Hour {
			continue // failed attempts stay visible for a week
		}
		s.removeBackup(ctx, w, b, c)
		fmt.Fprintf(w, "Removed old backup %s\n", filepath.Base(b.Path))
	}
}

func (s *Server) removeBackup(ctx context.Context, w io.Writer, b store.Backup, c laravel.S3) {
	if laravel.ValidBackupFile(b.Path) {
		_ = s.deployer.RemoveBackupFile(ctx, io.Discard, b.Path)
	}
	if b.Remote != "" && c.Configured() {
		if err := s.deployer.DeleteRemote(ctx, io.Discard, c, b.Remote); err != nil {
			fmt.Fprintf(w, "Couldn't delete the off-site copy %s: %v\n", b.Remote, err)
		}
	}
	_ = s.st.DeleteBackup(b.ID)
}

// backupAll backs up every database; one failing doesn't stop the others.
func (s *Server) backupAll(ctx context.Context, w io.Writer, reason string) error {
	srcs, err := s.backupSources(ctx, nil)
	if err != nil {
		return err
	}
	if len(srcs) == 0 {
		fmt.Fprintln(w, "No databases to back up yet: no MySQL databases, and no deployed site has a SQLite database.")
		return nil
	}
	failed := 0
	for _, src := range srcs {
		if err := s.runBackup(ctx, w, src, reason); err != nil {
			fmt.Fprintf(w, "✗ %v\n", err)
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d backups failed", failed, len(srcs))
	}
	fmt.Fprintf(w, "\nBacked up %d database(s).\n", len(srcs))
	return nil
}

// backupBeforeDeploy backs up a site's databases right before its deploy
// (and its migrations) runs. A failure stops the deploy.
func (s *Server) backupBeforeDeploy(ctx context.Context, w io.Writer, site store.Site) error {
	if site.DeployedAt == nil || !(site.IsLaravel() || site.IsPython()) || !s.backupSettings().BeforeDeploy {
		return nil
	}
	srcs, err := s.backupSources(ctx, &site)
	if err != nil || len(srcs) == 0 {
		return err
	}
	for _, src := range srcs {
		if err := s.runBackup(ctx, w, src, store.BackupDeploy); err != nil {
			return fmt.Errorf("%w. Nothing was deployed. To deploy without a backup, turn off \"Back up before each deploy\" on the Backups page", err)
		}
	}
	return nil
}

// backupLoop starts the nightly backup at the configured time.
func (s *Server) backupLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.maybeScheduleBackup(now)
		}
	}
}

func (s *Server) maybeScheduleBackup(now time.Time) {
	cfg := s.backupSettings()
	if !cfg.Enabled || now.Format("15:04") < cfg.Time {
		return
	}
	today := now.Format("2006-01-02")
	last, _ := s.st.Setting(keyBackupLastRun)
	if last == today {
		return
	}
	if err := s.st.SetSetting(keyBackupLastRun, today); err != nil {
		return
	}
	// Right after installing or upgrading, wait for the first scheduled time
	// instead of backing up the moment Skiff starts.
	if last == "" {
		return
	}
	if busy, _ := s.backupJobPending(); busy || !s.hasBackupCandidates() {
		return
	}
	if _, err := s.runner.Enqueue(nil, "backup", "Nightly backup", func(ctx context.Context, w io.Writer) error {
		return s.backupAll(ctx, w, store.BackupScheduled)
	}, nil); err != nil {
		slog.Error("schedule backup", "err", err)
	}
}

// hasBackupCandidates reports whether there could be anything to back up,
// without running anything on the server.
func (s *Server) hasBackupCandidates() bool {
	if dbs, err := s.st.Databases(); err == nil {
		for _, db := range dbs {
			if db.Status == store.DBReady {
				return true
			}
		}
	}
	sites, _ := s.st.Sites()
	for _, site := range sites {
		if site.DeployedAt != nil && (site.IsLaravel() || site.IsPython()) {
			return true
		}
	}
	return false
}

func (s *Server) backupJobPending() (bool, error) {
	j, err := s.st.LatestJob("backup")
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil && !j.Done(), err
}

// ---- pages ------------------------------------------------------------------

type backupsData struct {
	Sources    []backupSource
	SourcesErr string
	Recent     []store.Backup
	Settings   backupSettings
	S3         laravel.S3
	S3Secret   bool // a secret key is stored (it's never shown)
	S3Ready    bool
	ServerTime string
	NextRun    string
	Busy       bool
}

func (s *Server) backupsPage(w http.ResponseWriter, r *http.Request, sess store.Session) {
	d := backupsData{Settings: s.backupSettings(), S3: s.s3Config()}
	d.S3Secret = d.S3.SecretKey != ""
	d.S3.SecretKey = ""
	d.S3Ready = s.s3Config().Configured()
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	var err error
	if d.Sources, err = s.backupSources(ctx, nil); err != nil {
		d.SourcesErr = err.Error()
	}
	if d.Recent, err = s.st.Backups(50); err != nil {
		s.serverError(w, err)
		return
	}
	now := time.Now()
	d.ServerTime = now.Format("15:04 MST")
	if d.Settings.Enabled {
		next, _ := time.ParseInLocation("2006-01-02 15:04", now.Format("2006-01-02")+" "+d.Settings.Time, time.Local)
		if last, _ := s.st.Setting(keyBackupLastRun); last == now.Format("2006-01-02") || next.Before(now) {
			next = next.Add(24 * time.Hour)
		}
		d.NextRun = next.Format("Mon Jan 2, 15:04 MST")
	}
	d.Busy, _ = s.backupJobPending()
	s.render(w, r, "backups", http.StatusOK, pageFor(sess, "Backups", "backups", d))
}

func (s *Server) saveBackupSettings(w http.ResponseWriter, r *http.Request, _ store.Session) {
	at := strings.TrimSpace(r.FormValue("time"))
	keep, err := strconv.Atoi(strings.TrimSpace(r.FormValue("keep")))
	switch {
	case !backupTimeRe.MatchString(at):
		setFlash(w, "err", "Enter the time as HH:MM, e.g. 03:00.")
	case err != nil || keep < 1 || keep > maxBackupKeep:
		setFlash(w, "err", fmt.Sprintf("Keep between 1 and %d backups.", maxBackupKeep))
	default:
		on := func(v string) string {
			if v == "1" {
				return "1"
			}
			return "0"
		}
		for k, v := range map[string]string{
			keyBackupEnabled: on(r.FormValue("enabled")), keyBackupBeforeDeploy: on(r.FormValue("before_deploy")),
			keyBackupTime: at, keyBackupKeep: strconv.Itoa(keep),
		} {
			if err := s.st.SetSetting(k, v); err != nil {
				s.serverError(w, err)
				return
			}
		}
		// A new time later today should still run today.
		if now := time.Now(); at > now.Format("15:04") {
			_ = s.st.SetSetting(keyBackupLastRun, now.AddDate(0, 0, -1).Format("2006-01-02"))
		}
		setFlash(w, "ok", "Backup schedule saved.")
	}
	redirect(w, r, "/backups#schedule")
}

func (s *Server) saveBackupStorage(w http.ResponseWriter, r *http.Request, _ store.Session) {
	c := laravel.S3{
		Endpoint:  strings.TrimRight(strings.TrimSpace(r.FormValue("endpoint")), "/"),
		Region:    strings.TrimSpace(r.FormValue("region")),
		Bucket:    strings.TrimSpace(r.FormValue("bucket")),
		Prefix:    strings.Trim(strings.TrimSpace(r.FormValue("prefix")), "/"),
		AccessKey: strings.TrimSpace(r.FormValue("access_key")),
		SecretKey: strings.TrimSpace(r.FormValue("secret_key")),
	}
	if c.Region == "" {
		c.Region = "us-east-1"
	}
	if c.SecretKey == "" {
		c.SecretKey = s.s3Config().SecretKey // blank keeps the stored secret
	}
	if err := c.Validate(); err != nil {
		setFlash(w, "err", "Off-site storage: "+err.Error()+".")
		redirect(w, r, "/backups#storage")
		return
	}
	for k, v := range map[string]string{
		keyS3Endpoint: c.Endpoint, keyS3Region: c.Region, keyS3Bucket: c.Bucket,
		keyS3Prefix: c.Prefix, keyS3AccessKey: c.AccessKey, keyS3SecretKey: c.SecretKey,
	} {
		if err := s.st.SetSetting(k, v); err != nil {
			s.serverError(w, err)
			return
		}
	}
	// Check it right away, in a job whose log shows what the provider said.
	id, err := s.runner.Enqueue(nil, "backup-storage", "Test off-site storage ("+c.Bucket+")", func(ctx context.Context, w io.Writer) error {
		return s.deployer.TestStorage(ctx, w, c, backupHost())
	}, nil)
	if err != nil {
		s.serverError(w, err)
		return
	}
	setFlash(w, "ok", "Saved. Testing the connection now.")
	redirect(w, r, "/jobs/"+strconv.FormatInt(id, 10))
}

func (s *Server) removeBackupStorage(w http.ResponseWriter, r *http.Request, _ store.Session) {
	if err := s.st.DeleteSettings(keyS3Endpoint, keyS3Region, keyS3Bucket, keyS3Prefix, keyS3AccessKey, keyS3SecretKey); err != nil {
		s.serverError(w, err)
		return
	}
	setFlash(w, "ok", "Off-site storage removed. Copies already in the bucket were left there.")
	redirect(w, r, "/backups#storage")
}

// runBackupNow backs up one database (source=…) or all of them.
func (s *Server) runBackupNow(w http.ResponseWriter, r *http.Request, _ store.Session) {
	if busy, _ := s.backupJobPending(); busy {
		setFlash(w, "err", "A backup is already queued or running.")
		redirect(w, r, "/backups")
		return
	}
	var (
		title = "Back up all databases"
		fn    = func(ctx context.Context, w io.Writer) error { return s.backupAll(ctx, w, store.BackupManual) }
	)
	if id := r.FormValue("source"); id != "" {
		src, err := s.sourceOf(id)
		if err != nil {
			setFlash(w, "err", err.Error())
			redirect(w, r, "/backups")
			return
		}
		title = "Back up " + src.Label
		fn = func(ctx context.Context, w io.Writer) error { return s.runBackup(ctx, w, src, store.BackupManual) }
	}
	jobID, err := s.runner.Enqueue(nil, "backup", title, fn, nil)
	if err != nil {
		s.serverError(w, err)
		return
	}
	redirect(w, r, "/jobs/"+strconv.FormatInt(jobID, 10))
}

func (s *Server) backupFromPath(w http.ResponseWriter, r *http.Request) (store.Backup, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return store.Backup{}, false
	}
	b, err := s.st.Backup(id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return b, false
	}
	if err != nil {
		s.serverError(w, err)
		return b, false
	}
	return b, true
}

// restoreBackup replaces a database's contents with a backup, after taking a
// safety backup of what's there now.
func (s *Server) restoreBackup(w http.ResponseWriter, r *http.Request, _ store.Session) {
	b, ok := s.backupFromPath(w, r)
	if !ok {
		return
	}
	src, err := s.sourceOf(b.Source)
	if err == nil && b.Status != store.BackupOK {
		err = errors.New("only finished backups can be restored")
	}
	if err != nil {
		setFlash(w, "err", "Can't restore: "+err.Error())
		redirect(w, r, "/backups")
		return
	}
	jobID, err := s.runner.Enqueue(src.SiteID, "restore", "Restore "+src.Label+" from "+b.CreatedAt.Format("Jan 2 15:04"),
		func(ctx context.Context, w io.Writer) error { return s.restore(ctx, w, src, b) }, nil)
	if err != nil {
		s.serverError(w, err)
		return
	}
	redirect(w, r, "/jobs/"+strconv.FormatInt(jobID, 10))
}

func (s *Server) restore(ctx context.Context, w io.Writer, src backupSource, b store.Backup) error {
	// 1. Keep what's there now, in case the restore was a mistake.
	fmt.Fprintln(w, "Taking a safety backup of the current data first.")
	if err := s.runBackup(ctx, w, src, store.BackupRestore); err != nil {
		if src.Kind == "mysql" {
			return fmt.Errorf("%w; nothing was restored", err)
		}
		fmt.Fprintf(w, "No safety backup (%v); continuing.\n", err)
	}
	// 2. Get the file back from off-site storage if it's gone locally.
	if _, err := os.Stat(b.Path); err != nil && b.Remote != "" {
		c := s.s3Config()
		if !c.Configured() {
			return errors.New("the local file is gone and off-site storage isn't set up any more")
		}
		if err := s.deployer.Download(ctx, w, c, b.Remote, b.Path); err != nil {
			return err
		}
	}
	// 3. Restore.
	var err error
	if src.Kind == "mysql" {
		err = s.deployer.RestoreMySQL(ctx, w, src.DB, b.Path)
	} else {
		path, _ := laravel.SQLitePath(src.Domain, src.Rel)
		err = s.deployer.RestoreSQLite(ctx, w, path, b.Path)
	}
	if err != nil {
		return err
	}
	// 4. Long-running processes may hold connections or cached state.
	if src.SiteID != nil {
		if site, err := s.st.Site(*src.SiteID); err == nil && site.DeployedAt != nil {
			switch {
			case site.HasApp():
				return s.deployer.StartApp(ctx, w, site)
			case site.IsLaravel() && (site.WorkerCount > 0 || site.Scheduler):
				return s.deployer.ApplyLaravelProcesses(ctx, w, site)
			}
		}
	}
	fmt.Fprintln(w, "\nRestored.")
	return nil
}

func (s *Server) deleteBackupHandler(w http.ResponseWriter, r *http.Request, _ store.Session) {
	b, ok := s.backupFromPath(w, r)
	if !ok {
		return
	}
	if b.Status == store.BackupRunning {
		setFlash(w, "err", "That backup is still running.")
		redirect(w, r, "/backups")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	s.removeBackup(ctx, io.Discard, b, s.s3Config())
	setFlash(w, "ok", "Backup deleted.")
	redirect(w, r, "/backups")
}

// downloadBackup sends a backup file to the browser.
func (s *Server) downloadBackup(w http.ResponseWriter, r *http.Request, _ store.Session) {
	b, ok := s.backupFromPath(w, r)
	if !ok {
		return
	}
	if !laravel.ValidBackupFile(b.Path) {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(b.Path)
	if err != nil {
		setFlash(w, "err", "That backup's file isn't on this server any more.")
		redirect(w, r, "/backups")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		s.serverError(w, err)
		return
	}
	name := strings.TrimSuffix(filepath.Base(filepath.Dir(b.Path)), "/") + "-" + filepath.Base(b.Path)
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, name, st.ModTime(), f)
}
