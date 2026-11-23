package laravel

import (
	"regexp"
	"strings"

	"skiff/internal/store"
)

// Framework is a preset on the New site form: it picks the project type and
// fills in sensible defaults, which stay editable.
type Framework struct {
	ID, Name string
	Kind     string // store.KindLaravel, KindNode or KindStatic
	Start    string // Node apps: start command
	Output   string // static sites: build output folder
}

// Frameworks offered on the New site form, in display order.
var Frameworks = []Framework{
	{"laravel", "Laravel", store.KindLaravel, "", ""},
	{"nextjs", "Next.js", store.KindNode, "npm run start", ""},
	{"nuxt", "Nuxt", store.KindNode, "node .output/server/index.mjs", ""},
	{"sveltekit", "SvelteKit (adapter-node)", store.KindNode, "node build", ""},
	{"remix", "Remix / React Router", store.KindNode, "npm run start", ""},
	{"astro-ssr", "Astro (server, @astrojs/node)", store.KindNode, "node ./dist/server/entry.mjs", ""},
	{"node", "Express, Fastify, NestJS or other Node server", store.KindNode, "npm run start", ""},
	{"vite", "Vite: React, Vue, Svelte, Solid… (static)", store.KindStatic, "", "dist"},
	{"astro", "Astro (static)", store.KindStatic, "", "dist"},
	{"cra", "Create React App", store.KindStatic, "", "build"},
	{"next-export", "Next.js static export", store.KindStatic, "", "out"},
	{"html", "Plain HTML (no build)", store.KindStatic, "", "."},
}

// FrameworkByID returns the preset with this id, or the Laravel one.
func FrameworkByID(id string) Framework {
	for _, f := range Frameworks {
		if f.ID == id {
			return f
		}
	}
	return Frameworks[0]
}

// DefaultJSDeployScript installs dependencies with the project's package
// manager and runs its build script, if it has one. Used for Node apps and
// static sites.
const DefaultJSDeployScript = `# Runs as the "skiff" user inside the new release, with this site's Node.js
# first on PATH. Any failing command stops the deploy.
# Available: $SKIFF_SITE_PATH $SKIFF_BRANCH $SKIFF_DOMAIN $SKIFF_NODE

[ -f package.json ] || { echo "No package.json; nothing to install or build."; exit 0; }

# Install with the package manager the lockfile belongs to.
if [ -f pnpm-lock.yaml ]; then
    PM="npx --yes pnpm"
    $PM install --frozen-lockfile
elif [ -f yarn.lock ]; then
    PM="npx --yes yarn"
    $PM install --frozen-lockfile
elif [ -f package-lock.json ]; then
    PM=npm
    npm ci
else
    PM=npm
    npm install
fi

# Build, if package.json has a "build" script.
if node -e "process.exit(require('./package.json').scripts?.build ? 0 : 1)"; then
    $PM run build
fi
`

// DefaultDeployScriptFor returns the default deploy script for a site kind.
func DefaultDeployScriptFor(kind string) string {
	if kind == store.KindNode || kind == store.KindStatic {
		return DefaultJSDeployScript
	}
	return DefaultDeployScript
}

var (
	outputDirRe     = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,100}$`)
	workerOptionsRe = regexp.MustCompile(`^[A-Za-z0-9=_,.:/ -]{0,200}$`)
)

// ValidOutputDir accepts a relative folder inside the release ("dist",
// "apps/web/dist", or "." for the repository root).
func ValidOutputDir(dir string) bool {
	if !outputDirRe.MatchString(dir) || strings.HasPrefix(dir, "/") {
		return false
	}
	for _, part := range strings.Split(dir, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

// ValidStartCommand accepts one line of shell; it runs as the site user.
func ValidStartCommand(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	return cmd != "" && len(cmd) <= 500 && !strings.ContainsAny(cmd, "\r\n")
}

// ValidWorkerOptions accepts queue:work arguments such as
// "redis --queue=high,default --tries=3 --timeout=90".
func ValidWorkerOptions(opts string) bool { return workerOptionsRe.MatchString(opts) }

// MaxWorkers bounds queue workers per site; each is a PHP process.
const MaxWorkers = 10
