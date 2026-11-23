package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"skiff/internal/store"
)

func postNewSite(s *Server, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/sites", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.createSite(w, r, store.Session{Username: "admin"})
	return w
}

func TestCreateSitesOfEveryKind(t *testing.T) {
	s := newTestServer(t)
	base := func(domain, fw string) url.Values {
		// No "php": JS projects must not need one.
		return url.Values{"repo": {"o/" + fw}, "branch": {"main"}, "domain": {domain}, "node": {"24"}, "framework": {fw}}
	}
	if w := postNewSite(s, base("next.example.com", "nextjs")); w.Code != http.StatusSeeOther {
		t.Fatalf("nextjs: %d %s", w.Code, w.Body.String())
	}
	if w := postNewSite(s, base("spa.example.com", "vite")); w.Code != http.StatusSeeOther {
		t.Fatalf("vite: %d", w.Code)
	}
	bad := base("bad.example.com", "vite")
	bad.Set("output_dir", "../../etc")
	if w := postNewSite(s, bad); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad output dir accepted: %d", w.Code)
	}
	noNode := base("nonode.example.com", "nuxt")
	noNode.Set("node", "")
	if w := postNewSite(s, noNode); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Node app without Node accepted: %d", w.Code)
	}

	sites, _ := s.st.Sites()
	byDomain := map[string]store.Site{}
	for _, x := range sites {
		byDomain[x.Domain] = x
	}
	n, v := byDomain["next.example.com"], byDomain["spa.example.com"]
	if n.Kind != store.KindNode || n.StartCommand != "npm run start" || int64(n.Port) != store.AppPortBase+n.ID || !strings.Contains(n.DeployScript, "pnpm") {
		t.Errorf("next site = %+v", n)
	}
	if v.Kind != store.KindStatic || v.OutputDir != "dist" || v.Port != 0 {
		t.Errorf("vite site = %+v", v)
	}

	// Every kind's site page renders.
	for _, x := range []store.Site{n, v} {
		_ = s.st.MarkSiteDeployed(x.ID)
		r := httptest.NewRequest(http.MethodGet, "/sites/x", nil)
		r.SetPathValue("id", strconv.FormatInt(x.ID, 10))
		w := httptest.NewRecorder()
		s.sitePage(w, r, store.Session{Username: "admin"})
		body := w.Body.String()
		if w.Code != 200 || !strings.Contains(body, x.KindLabel()) || strings.Contains(body, `name="php"`) {
			t.Errorf("%s page: %d (PHP picker shown: %v)", x.Domain, w.Code, strings.Contains(body, `name="php"`))
		}

		// The site's other sections render with the site sidebar, the right
		// item active and the content that moved there.
		id := strconv.FormatInt(x.ID, 10)
		for _, pg := range []struct {
			h      func(http.ResponseWriter, *http.Request, store.Session)
			active string
			want   string
		}{
			{s.sitePage, `/sites/` + id + `">`, "Recent activity"},
			{s.siteDeploymentsPage, `/sites/` + id + `/deployments">`, "Releases"},
			{s.siteSettingsPage, `/sites/` + id + `/settings">`, "Deploy script"},
		} {
			r := httptest.NewRequest(http.MethodGet, "/sites/x", nil)
			r.SetPathValue("id", id)
			w := httptest.NewRecorder()
			pg.h(w, r, store.Session{Username: "admin"})
			body := w.Body.String()
			if w.Code != 200 || !strings.Contains(body, `class="sb-link active" href="`+pg.active) || !strings.Contains(body, pg.want) || !strings.Contains(body, "All sites") {
				t.Errorf("%s %s: code %d, active link or %q missing", x.Domain, pg.active, w.Code, pg.want)
			}
		}
	}
}
