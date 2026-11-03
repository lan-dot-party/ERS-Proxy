package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/lan-dot-party/ers-proxy/internal/auth"
	"github.com/lan-dot-party/ers-proxy/internal/config"
	"github.com/lan-dot-party/ers-proxy/internal/secret"
	"github.com/lan-dot-party/ers-proxy/internal/store"
)

func init() {
	configCmd.AddCommand(configInitCmd)
	configInitCmd.Flags().Bool("force", false, "overwrite an existing config.yaml")
	rootCmd.AddCommand(configCmd, migrateCmd, backupCmd, userCmd)

	userCreateCmd.Flags().String("role", store.RoleUser, "user or admin")
	userCreateCmd.Flags().String("language", "en", "en or de")
	userCmd.AddCommand(userCreateCmd, userPasswdCmd, userListCmd, userDeleteCmd)
}

var configCmd = &cobra.Command{Use: "config", Short: "Configuration helpers"}

var configInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Write an example config.yaml and create secret.key",
	RunE: func(cmd *cobra.Command, args []string) error {
		force, _ := cmd.Flags().GetBool("force")
		if _, err := os.Stat(configPath); err == nil && !force {
			return fmt.Errorf("%s exists (use --force to overwrite)", configPath)
		}
		if err := os.MkdirAll(filepath.Dir(configPath), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(configPath, []byte(config.Example), 0o640); err != nil {
			return err
		}
		fmt.Println("wrote", configPath)
		cfg, err := config.Load(configPath)
		if err != nil {
			return err
		}
		keyPath := cfg.ResolvePath(cfg.SecretKeyFile)
		if _, err := os.Stat(keyPath); errors.Is(err, os.ErrNotExist) {
			if err := secret.Generate(keyPath); err != nil {
				return err
			}
			fmt.Println("created", keyPath)
		}
		return nil
	},
}

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Apply database migrations (the server also does this on start)",
	RunE: func(cmd *cobra.Command, args []string) error {
		e, err := open(cmd.Context())
		if err != nil {
			return err
		}
		defer e.close()
		v, err := e.db.SchemaVersion(cmd.Context())
		fmt.Println("schema version", v)
		return err
	},
}

var backupCmd = &cobra.Command{
	Use:   "backup [file]",
	Short: "Write a consistent copy of the SQLite database while the server runs",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(configPath)
		if err != nil {
			return err
		}
		db, err := store.Open(store.Options{Path: cfg.ResolvePath(cfg.Storage.Path)})
		if err != nil {
			return err
		}
		defer db.Close()
		target := filepath.Join(filepath.Dir(cfg.ResolvePath(cfg.Storage.Path)), "backups",
			"ers-proxy-"+time.Now().Format("2006-01-02-150405")+".db")
		if len(args) == 1 {
			target = args[0]
		}
		if err := db.Backup(cmd.Context(), target); err != nil {
			return err
		}
		fmt.Println("backup written to", target)
		return nil
	},
}

var userCmd = &cobra.Command{Use: "user", Short: "Manage web users"}

var userCreateCmd = &cobra.Command{
	Use:   "create <username>",
	Short: "Create a user (password from prompt or stdin)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		role, _ := cmd.Flags().GetString("role")
		lang, _ := cmd.Flags().GetString("language")
		if lang != "en" && lang != "de" {
			return errors.New("--language must be en or de")
		}
		e, err := open(cmd.Context())
		if err != nil {
			return err
		}
		defer e.close()
		pw, err := readSecret("Password for "+args[0]+": ", true)
		if err != nil {
			return err
		}
		hash, err := auth.HashPassword(pw)
		if err != nil {
			return err
		}
		u := &store.User{Username: args[0], PasswordHash: hash, Role: role, Language: lang}
		if err := e.db.CreateUser(cmd.Context(), u); err != nil {
			if errors.Is(err, store.ErrExists) {
				return fmt.Errorf("user %s already exists", args[0])
			}
			return err
		}
		e.audit(cmd.Context(), store.AuditEntry{Actor: "cli", Action: "user.create", Target: u.Username, Details: "role=" + role})
		fmt.Printf("user %s created (role %s)\n", u.Username, role)
		return nil
	},
}

var userPasswdCmd = &cobra.Command{
	Use:   "passwd <username>",
	Short: "Set a new password (ends all sessions of the user)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		e, err := open(cmd.Context())
		if err != nil {
			return err
		}
		defer e.close()
		u, err := e.db.UserByName(cmd.Context(), args[0])
		if err != nil {
			return fmt.Errorf("user %s: %w", args[0], err)
		}
		pw, err := readSecret("New password for "+u.Username+": ", true)
		if err != nil {
			return err
		}
		hash, err := auth.HashPassword(pw)
		if err != nil {
			return err
		}
		if err := e.db.SetPassword(cmd.Context(), u.ID, hash); err != nil {
			return err
		}
		e.audit(cmd.Context(), store.AuditEntry{Actor: "cli", Action: "user.password", Target: u.Username})
		fmt.Println("password changed")
		return nil
	},
}

var userListCmd = &cobra.Command{
	Use:   "list",
	Short: "List users",
	RunE: func(cmd *cobra.Command, args []string) error {
		e, err := open(cmd.Context())
		if err != nil {
			return err
		}
		defer e.close()
		users, err := e.db.ListUsers(cmd.Context())
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tUSERNAME\tROLE\tDISABLED\tLAST LOGIN")
		for _, u := range users {
			last := "-"
			if u.LastLogin != nil {
				last = u.LastLogin.Local().Format("2006-01-02 15:04")
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\t%v\t%s\n", u.ID, u.Username, u.Role, u.Disabled, last)
		}
		return tw.Flush()
	},
}

var userDeleteCmd = &cobra.Command{
	Use:   "delete <username>",
	Short: "Delete a user",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		e, err := open(cmd.Context())
		if err != nil {
			return err
		}
		defer e.close()
		u, err := e.db.UserByName(cmd.Context(), args[0])
		if err != nil {
			return fmt.Errorf("user %s: %w", args[0], err)
		}
		if u.Role == store.RoleAdmin && !u.Disabled {
			if n, err := e.db.CountAdmins(cmd.Context()); err == nil && n <= 1 {
				return errors.New("refusing to delete the last admin")
			}
		}
		if err := e.db.DeleteUser(cmd.Context(), u.ID); err != nil {
			return err
		}
		e.audit(cmd.Context(), store.AuditEntry{Actor: "cli", Action: "user.delete", Target: u.Username})
		fmt.Println("user deleted")
		return nil
	},
}
