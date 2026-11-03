// Package cmd implements the ers-proxy command line.
package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/lan-dot-party/ers-proxy/internal/config"
	"github.com/lan-dot-party/ers-proxy/internal/secret"
	"github.com/lan-dot-party/ers-proxy/internal/store"
	"github.com/lan-dot-party/ers-proxy/pkg/version"
)

var configPath string

var rootCmd = &cobra.Command{
	Use:           "ers-proxy",
	Short:         "Portal for device web interfaces and EDM compatibility proxy for ERS switches",
	SilenceUsage:  true,
	SilenceErrors: true,
	Version:       version.GetVersion(),
}

// Execute runs the CLI.
func Execute() error {
	err := rootCmd.Execute()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
	}
	return err
}

func init() {
	def := os.Getenv("ERSPROXY_CONFIG")
	if def == "" {
		def = config.DefaultPath
	}
	rootCmd.PersistentFlags().StringVarP(&configPath, "config", "c", def, "path to config.yaml (env ERSPROXY_CONFIG)")
	rootCmd.AddCommand(versionCmd)
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("ers-proxy", version.GetVersion())
	},
}

func newLogger(c *config.Config) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(c.Log.Level)); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if c.Log.Format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

// env bundles what most commands need.
type env struct {
	cfg *config.Config
	db  *store.DB
	key *secret.Key
	log *slog.Logger
}

// audit writes an audit log entry for a CLI action; a failure is logged.
func (e *env) audit(ctx context.Context, entry store.AuditEntry) {
	if err := e.db.Audit(ctx, entry); err != nil {
		e.log.Error("audit log", "err", err, "action", entry.Action)
	}
}

// close closes the database. When the CLI runs as root (sudo ers-proxy user
// create …), files it created are handed to the owner of the data directory,
// so the service user can still write them.
func (e *env) close() {
	e.db.Close()
	if os.Geteuid() != 0 {
		return
	}
	dbPath := e.cfg.ResolvePath(e.cfg.Storage.Path)
	fi, err := os.Stat(filepath.Dir(dbPath))
	if err != nil {
		return
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid == 0 {
		return
	}
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if _, err := os.Stat(p); err == nil {
			if err := os.Chown(p, int(st.Uid), int(st.Gid)); err != nil {
				e.log.Warn("chown", "file", p, "err", err)
			}
		}
	}
}

// open loads the config, opens the database and applies migrations.
func open(ctx context.Context) (*env, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	log := newLogger(cfg)
	key, err := secret.LoadOrCreate(cfg.ResolvePath(cfg.SecretKeyFile))
	if err != nil {
		return nil, err
	}
	db, err := store.Open(store.Options{Path: cfg.ResolvePath(cfg.Storage.Path)})
	if err != nil {
		return nil, err
	}
	if err := db.Migrate(ctx, log); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &env{cfg: cfg, db: db, key: key, log: log}, nil
}

// readSecret reads a secret from stdin (when piped) or an interactive prompt.
func readSecret(prompt string, confirm bool) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if confirm {
		fmt.Fprint(os.Stderr, "Repeat: ")
		b2, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		if string(b) != string(b2) {
			return "", errors.New("passwords do not match")
		}
	}
	return string(b), nil
}
