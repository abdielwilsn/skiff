package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"skiff/internal/laravel"
	"skiff/internal/store"
)

const keyMySQLInstalled = "mysql_installed"

func (s *Server) mysqlInstalled() bool {
	v, _ := s.st.Setting(keyMySQLInstalled)
	return v == "1" || s.cfg.DryRun
}

type databasesData struct {
	Installed   bool
	Provisioned bool
	InstallJob  *store.Job
	Databases   []store.Database
	AttachTo    []store.Site // deployed sites without a database
	SuggestName string
}

func (s *Server) databasesPage(w http.ResponseWriter, r *http.Request, sess store.Session) {
	d := databasesData{Installed: s.mysqlInstalled(), Provisioned: s.provisioned()}
	if j, err := s.st.LatestJob("mysql-install"); err == nil {
		d.InstallJob = &j
	}
	var err error
	if d.Databases, err = s.st.Databases(); err != nil {
		s.serverError(w, err)
		return
	}
	hasDB := map[int64]bool{}
	for _, db := range d.Databases {
		if db.SiteID != nil {
			hasDB[*db.SiteID] = true
		}
	}
	sites, _ := s.st.Sites()
	for _, x := range sites {
		if x.DeployedAt != nil && !hasDB[x.ID] {
			d.AttachTo = append(d.AttachTo, x)
		}
	}
	s.render(w, r, "databases", http.StatusOK, pageFor(sess, "Databases", "databases", d))
}

func (s *Server) installMySQL(w http.ResponseWriter, r *http.Request, _ store.Session) {
	if j, err := s.st.LatestJob("mysql-install"); err == nil && !j.Done() {
		redirect(w, r, "/jobs/"+strconv.FormatInt(j.ID, 10))
		return
	}
	id, err := s.runner.Enqueue(nil, "mysql-install", "Install MySQL", s.deployer.InstallMySQL,
		func(err error) {
			if err == nil {
				_ = s.st.SetSetting(keyMySQLInstalled, "1")
			}
		})
	if err != nil {
		s.serverError(w, err)
		return
	}
	redirect(w, r, "/jobs/"+strconv.FormatInt(id, 10))
}

func (s *Server) createDatabase(w http.ResponseWriter, r *http.Request, _ store.Session) {
	name := strings.ToLower(strings.TrimSpace(r.FormValue("name")))
	fail := func(msg string) {
		setFlash(w, "err", msg)
		redirect(w, r, "/databases")
	}
	if !s.mysqlInstalled() {
		fail("Install MySQL first.")
		return
	}
	if !laravel.ValidDBName(name) {
		fail("Use 1–32 lowercase letters, digits or underscores, starting with a letter (and not a reserved name like mysql).")
		return
	}
	if taken, _ := s.st.NameTaken(name); taken {
		fail("A database or user called " + name + " already exists.")
		return
	}
	db := store.Database{Name: name, Username: name, Password: laravel.NewPassword(), Status: store.DBPending}
	id, err := s.st.CreateDatabase(db)
	if errors.Is(err, store.ErrDuplicate) {
		fail("A database or user called " + name + " already exists.")
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	db.ID = id
	jobID, err := s.enqueueCreateDatabase(db)
	if err != nil {
		s.serverError(w, err)
		return
	}
	redirect(w, r, "/jobs/"+strconv.FormatInt(jobID, 10))
}

func (s *Server) enqueueCreateDatabase(db store.Database) (int64, error) {
	spec := laravel.SpecOf(db)
	return s.runner.Enqueue(nil, "db-create", "Create database "+db.Name,
		func(ctx context.Context, log io.Writer) error { return s.deployer.CreateDatabase(ctx, log, spec) },
		func(err error) {
			status := store.DBReady
			if err != nil {
				status = store.DBFailed
			}
			_ = s.st.SetDatabaseStatus(db.ID, status)
		})
}

func (s *Server) databaseFromPath(w http.ResponseWriter, r *http.Request) (store.Database, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return store.Database{}, false
	}
	db, err := s.st.Database(id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return db, false
	}
	if err != nil {
		s.serverError(w, err)
		return db, false
	}
	return db, true
}

// retryDatabase re-runs creation for a database whose creation failed.
func (s *Server) retryDatabase(w http.ResponseWriter, r *http.Request, _ store.Session) {
	db, ok := s.databaseFromPath(w, r)
	if !ok {
		return
	}
	if db.Status != store.DBFailed {
		redirect(w, r, "/databases")
		return
	}
	_ = s.st.SetDatabaseStatus(db.ID, store.DBPending)
	id, err := s.enqueueCreateDatabase(db)
	if err != nil {
		s.serverError(w, err)
		return
	}
	redirect(w, r, "/jobs/"+strconv.FormatInt(id, 10))
}

// attachDatabase points a deployed site's .env at the database and migrates.
func (s *Server) attachDatabase(w http.ResponseWriter, r *http.Request, _ store.Session) {
	db, ok := s.databaseFromPath(w, r)
	if !ok {
		return
	}
	fail := func(msg string) {
		setFlash(w, "err", msg)
		redirect(w, r, "/databases")
	}
	if db.Status != store.DBReady {
		fail("The database isn't ready yet.")
		return
	}
	if db.SiteID != nil {
		fail(db.Name + " is already attached to " + db.SiteDomain + ".")
		return
	}
	siteID, err := strconv.ParseInt(r.FormValue("site_id"), 10, 64)
	if err != nil {
		fail("Choose a site.")
		return
	}
	site, err := s.st.Site(siteID)
	if err != nil {
		fail("That site no longer exists.")
		return
	}
	if site.DeployedAt == nil {
		fail("Deploy " + site.Domain + " first.")
		return
	}
	if _, err := s.st.DatabaseForSite(siteID); err == nil {
		fail(site.Domain + " already has a database.")
		return
	}
	if busy, _ := s.st.HasActiveJob(siteID); busy {
		fail("Wait for " + site.Domain + "'s running job to finish.")
		return
	}
	spec := laravel.SpecOf(db)
	id, err := s.runner.Enqueue(&siteID, "db-attach", "Use database "+db.Name+" for "+site.Domain,
		func(ctx context.Context, log io.Writer) error {
			if err := s.deployer.WriteDatabaseEnv(ctx, log, site, spec); err != nil {
				return err
			}
			return s.deployer.Migrate(ctx, log, site)
		},
		func(err error) {
			if err == nil {
				_ = s.st.AttachDatabase(db.ID, &siteID)
			}
		})
	if err != nil {
		s.serverError(w, err)
		return
	}
	redirect(w, r, "/jobs/"+strconv.FormatInt(id, 10))
}

// detachDatabase only unlinks the record; the site's .env is left as it is.
func (s *Server) detachDatabase(w http.ResponseWriter, r *http.Request, _ store.Session) {
	db, ok := s.databaseFromPath(w, r)
	if !ok {
		return
	}
	if err := s.st.AttachDatabase(db.ID, nil); err != nil {
		s.serverError(w, err)
		return
	}
	setFlash(w, "ok", "Detached "+db.Name+". The site's .env still points at it until you change it.")
	redirect(w, r, "/databases")
}

func (s *Server) deleteDatabase(w http.ResponseWriter, r *http.Request, _ store.Session) {
	db, ok := s.databaseFromPath(w, r)
	if !ok {
		return
	}
	if r.FormValue("confirm") != db.Name {
		setFlash(w, "err", "Type the database name exactly to confirm deletion.")
		redirect(w, r, "/databases")
		return
	}
	if db.Status == store.DBDeleting {
		redirect(w, r, "/databases")
		return
	}
	_ = s.st.SetDatabaseStatus(db.ID, store.DBDeleting)
	spec := laravel.SpecOf(db)
	id, err := s.runner.Enqueue(nil, "db-delete", "Delete database "+db.Name,
		func(ctx context.Context, log io.Writer) error { return s.deployer.DropDatabase(ctx, log, spec) },
		func(err error) {
			if err != nil {
				_ = s.st.SetDatabaseStatus(db.ID, store.DBFailed)
			} else {
				_ = s.st.DeleteDatabase(db.ID)
			}
		})
	if err != nil {
		s.serverError(w, err)
		return
	}
	redirect(w, r, "/jobs/"+strconv.FormatInt(id, 10))
}
