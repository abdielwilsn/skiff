// Command skiff is a tiny self-hosted server panel for deploying Laravel apps
// from GitHub.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"skiff/internal/dotenv"
	"skiff/internal/executor"
	"skiff/internal/jobs"
	"skiff/internal/store"
	"skiff/internal/web"
)

var version = "dev"

const usage = `Skiff: a tiny self-hosted server panel for Laravel apps.

Usage:
  skiff serve [flags]         Run the panel (default command)
  skiff reset-admin [flags]   Remove the admin account so /setup runs again
  skiff reset-panel-domain    Stop redirecting the panel to its HTTPS domain
                              (use if the domain stops working)
  skiff version               Print the version
  skiff tls-fingerprint       Print the SHA-256 fingerprint of the panel's
                              self-signed certificate, to check the browser's
  skiff disable-2fa           Turn off two-factor login (lost phone)
  skiff env-exec FILE CMD...  Run CMD with the variables in FILE (a .env).
                              Used by app services, as the site user, so the
                              file is read with that user's permissions

Run "skiff serve -h" for flags.
`

func main() {
	cmd, args := "serve", os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "reset-admin":
		err = resetAdmin(args)
	case "reset-panel-domain":
		err = resetPanelDomain(args)
	case "version":
		fmt.Println(version)
	case "env-exec":
		err = envExec(args)
	case "tls-fingerprint":
		err = tlsFingerprint(args)
	case "disable-2fa":
		err = disable2FA(args)
	case "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "skiff:", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func dataFlag(fs *flag.FlagSet) *string {
	return fs.String("data", envOr("SKIFF_DATA", "/var/lib/skiff"), "directory for the database and job logs")
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", envOr("SKIFF_ADDR", ":8090"), "address to listen on")
	dataDir := dataFlag(fs)
	baseURL := fs.String("url", os.Getenv("SKIFF_URL"), "public URL of the panel, e.g. https://panel.example.com (default: taken from each request)")
	dryRun := fs.Bool("dry-run", os.Getenv("SKIFF_DRY_RUN") == "1", "print server commands instead of running them (for local UI development)")
	acmeServer := fs.String("acme-server", os.Getenv("SKIFF_ACME_SERVER"),
		"ACME directory for HTTPS certificates (default: Let's Encrypt production). For testing use https://acme-staging-v02.api.letsencrypt.org/directory")
	_ = fs.Parse(args)
	// Only for local test CAs such as Pebble, which use self-signed certificates.
	acmeInsecure := os.Getenv("SKIFF_ACME_INSECURE") == "1"

	if err := os.MkdirAll(filepath.Join(*dataDir, "logs"), 0o700); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(*dataDir, "skiff.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.FailInterrupted(); err != nil {
		return err
	}

	var ex executor.Executor = executor.Local{}
	if *dryRun {
		ex = executor.DryRun{}
	} else if os.Geteuid() != 0 {
		slog.Warn("not running as root: provisioning and deploys will fail (use --dry-run for local development)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runner := jobs.NewRunner(ctx, st, filepath.Join(*dataDir, "logs"))
	handler, err := web.New(web.Config{
		Store: st, Runner: runner, Exec: ex,
		DataDir: *dataDir, ListenAddr: *addr, BaseURL: *baseURL, DryRun: *dryRun, Version: version,
		ACMEServer: *acmeServer, ACMEInsecure: acmeInsecure, Context: ctx,
	})
	if err != nil {
		return err
	}

	// HTTPS (self-signed until the panel has a domain) and plain HTTP for
	// local clients, on the same port.
	tlsCfg, fingerprint, err := web.PanelTLS(*dataDir)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(web.NewMuxListener(ln, tlsCfg)) }()
	slog.Info("skiff is running", "addr", *addr, "version", version, "data", *dataDir, "dry_run", *dryRun, "tls_fingerprint", fingerprint)

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// resetPanelDomain is the escape hatch for a panel domain that stopped working
// (DNS change, expired domain): the running panel stops redirecting within a
// few seconds. The Nginx proxy and certificate are left in place; remove them
// from Settings or re-add the domain there.
func resetPanelDomain(args []string) error {
	fs := flag.NewFlagSet("reset-panel-domain", flag.ExitOnError)
	dataDir := dataFlag(fs)
	_ = fs.Parse(args)
	st, err := store.Open(filepath.Join(*dataDir, "skiff.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	domain, err := st.Setting("panel_domain")
	if err != nil {
		return err
	}
	if domain == "" {
		fmt.Println("No panel domain is set.")
		return nil
	}
	if err := st.DeleteSettings("panel_domain"); err != nil {
		return err
	}
	fmt.Printf("The panel no longer redirects to https://%s.\n", domain)
	fmt.Println("Within a few seconds it is reachable directly at https://<server-ip>:<port> again.")
	return nil
}

func resetAdmin(args []string) error {
	fs := flag.NewFlagSet("reset-admin", flag.ExitOnError)
	dataDir := dataFlag(fs)
	_ = fs.Parse(args)
	st, err := store.Open(filepath.Join(*dataDir, "skiff.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.ResetUsers(); err != nil {
		return err
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	token := hex.EncodeToString(b)
	if err := os.WriteFile(filepath.Join(*dataDir, "setup-token"), []byte(token+"\n"), 0o600); err != nil {
		return err
	}
	fmt.Println("Admin account removed. Create a new one at:")
	fmt.Printf("  https://<this-server>:<port>/setup?token=%s\n", token)
	return nil
}

// envExec loads a .env file and replaces itself with the command. Apps' systemd
// services start through it so the file is opened by the (unprivileged) site
// user rather than by systemd as root, which would follow a symlink the site
// planted to any root-only file. A missing file is fine.
func envExec(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: skiff env-exec FILE CMD [ARGS...]")
	}
	env := os.Environ()
	f, err := os.Open(args[0])
	switch {
	case err == nil:
		vars, perr := dotenv.Parse(f)
		f.Close()
		if perr != nil {
			return fmt.Errorf("read %s: %w", args[0], perr)
		}
		for _, kv := range vars {
			env = append(env, kv[0]+"="+kv[1])
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return err
	}
	path := args[1]
	if !strings.Contains(path, "/") {
		if path, err = exec.LookPath(path); err != nil {
			return err
		}
	}
	return syscall.Exec(path, args[1:], env)
}

func tlsFingerprint(args []string) error {
	fs := flag.NewFlagSet("tls-fingerprint", flag.ExitOnError)
	dataDir := dataFlag(fs)
	_ = fs.Parse(args)
	fp, err := web.TLSFingerprint(*dataDir)
	if err != nil {
		return err
	}
	fmt.Println(fp)
	return nil
}

// disable2FA turns off two-factor login for every account, for when the
// phone with the authenticator is lost. It needs root on the server.
func disable2FA(args []string) error {
	fs := flag.NewFlagSet("disable-2fa", flag.ExitOnError)
	dataDir := dataFlag(fs)
	_ = fs.Parse(args)
	st, err := store.Open(filepath.Join(*dataDir, "skiff.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	n, err := st.DisableAllTOTP()
	if err != nil {
		return err
	}
	fmt.Printf("Two-factor login turned off for %d account(s). Log in with just the password, then set it up again in Settings.\n", n)
	return nil
}
