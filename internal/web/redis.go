package web

import (
	"context"
	"io"
	"net/http"

	"skiff/internal/store"
)

// useRedis points a deployed site's .env at the server's Redis and restarts
// its processes. Laravel sites can also move their cache, sessions and queue
// to Redis.
func (s *Server) useRedis(w http.ResponseWriter, r *http.Request, _ store.Session) {
	site, ok := s.siteFromPath(w, r)
	if !ok {
		return
	}
	fail := func(msg string) {
		setFlash(w, "err", msg)
		redirect(w, r, s.siteURL(site))
	}
	pw := s.redisPassword()
	switch {
	case pw == "":
		fail("Install Redis on the Databases page first.")
		return
	case site.IsStatic():
		fail("Static sites can't use Redis.")
		return
	case site.DeployedAt == nil:
		fail("Deploy the site first.")
		return
	}
	drivers := site.IsLaravel() && r.FormValue("drivers") == "1"
	setFlash(w, "ok", "Connecting "+site.Domain+" to Redis.")
	s.enqueueSiteJob(w, r, site, "redis", "Use Redis for "+site.Domain,
		func(ctx context.Context, log io.Writer, cur store.Site) error {
			if cur.IsLaravel() {
				if err := s.deployer.EnsurePHPRedis(ctx, log, cur.PHPVersion); err != nil {
					return err
				}
			}
			if err := s.deployer.WriteRedisEnv(ctx, log, cur, pw, drivers); err != nil {
				return err
			}
			if err := s.st.SetSiteRedis(cur.ID, true); err != nil {
				return err
			}
			cur.Redis = true
			// Long-running processes read the environment when they start.
			switch {
			case cur.HasApp():
				return s.deployer.StartApp(ctx, log, cur)
			case cur.IsLaravel() && (cur.WorkerCount > 0 || cur.Scheduler):
				return s.deployer.ApplyLaravelProcesses(ctx, log, cur)
			}
			return nil
		})
}
