package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/k8s-ai/k8s-ai/internal/config"
	"github.com/k8s-ai/k8s-ai/internal/store"
)

// newDBCmd 聚合持久化运维子命令（P6.5）。与顶层 aopsctl migrate 分工明确：
// db migrate 只升级 schema，migrate 负责把三期历史数据导入四期 Store。
func newDBCmd(deps Dependencies) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "db",
		Short: "Platform database operations (upgrade Job / initContainer entrypoint)",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newDBMigrateCmd(deps))
	cmd.AddCommand(newDBCheckCmd(deps))
	return cmd
}

// newDBCheckCmd waits for schema readiness without running migrations.
func newDBCheckCmd(deps Dependencies) *cobra.Command {
	var wait time.Duration
	cmd := &cobra.Command{Use: "check", Short: "Check database schema readiness without applying migrations", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if wait < 0 {
				return fmt.Errorf("--wait must not be negative")
			}
			if err := cmd.Context().Err(); err != nil {
				return fmt.Errorf("database schema check canceled: %w", err)
			}
			cfg, err := deps.LoadConfig(loadOptionsFromFlags(cmd, false))
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			ctx := cmd.Context()
			if wait > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, wait)
				defer cancel()
			}
			for {
				if err := ctx.Err(); err != nil {
					return fmt.Errorf("database schema check canceled: %w", err)
				}
				st, _, openErr := openPlatformStoreContext(ctx, cfg)
				checkErr := openErr
				if checkErr == nil {
					checkErr = store.IsReady(ctx, st)
					_ = st.Close()
				}
				if checkErr == nil {
					fmt.Fprintln(cmd.OutOrStdout(), "database schema ready")
					return nil
				}
				if wait == 0 {
					return fmt.Errorf("database schema not ready (run aopsctl db migrate): %w", checkErr)
				}
				select {
				case <-ctx.Done():
					return fmt.Errorf("database schema not ready (run aopsctl db migrate): %w (last check: %v)", ctx.Err(), checkErr)
				case <-time.After(time.Second):
				}
			}
		}}
	cmd.Flags().DurationVar(&wait, "wait", 0, "maximum time to wait for connectivity and schema readiness")
	return cmd
}

// newDBMigrateCmd 应用尚未执行的 schema 与配套数据升级，不导入旧平台数据。
func newDBMigrateCmd(deps Dependencies) *cobra.Command {
	var dbPath string
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Apply pending database schema and associated data migrations",
		Long: "Upgrade the configured store (platform.store.driver) to the current schema version.\n" +
			"\n" +
			"Scope: versioned schema and associated data upgrades. Does not import legacy data (use\n" +
			"'aopsctl migrate') and it never creates the database itself; the schema and\n" +
			"its character set must already exist. Re-running is a no-op, so this command\n" +
			"is meant to be used as a pre-upgrade Job or initContainer.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := deps.LoadConfig(loadOptionsFromFlags(cmd, false))
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			// --db 只是 sqlite 兜底路径的便捷覆盖，mysql 走 DSN。
			if cmd.Flags().Changed("db") && !cfg.Platform.Store.IsMySQL() {
				cfg.Platform.Store.SQLitePath = dbPath
			}
			st, target, err := openPlatformStoreContext(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			w := cmd.OutOrStdout()
			before, expected, err := st.MigrationStatus(cmd.Context())
			if err != nil {
				return fmt.Errorf("read schema version: %w", err)
			}
			if err := st.Migrate(cmd.Context()); err != nil {
				return fmt.Errorf("migrate schema: %w", err)
			}
			after, expected, err := st.MigrationStatus(cmd.Context())
			if err != nil {
				return fmt.Errorf("read schema version: %w", err)
			}
			if after < expected {
				return fmt.Errorf("schema still at version %d, expected %d", after, expected)
			}
			fmt.Fprintf(w, "db migrate ok: driver=%s target=%s schema_version=%d->%d (expected %d)\n",
				driverName(cfg), target, before, after, expected)
			return nil
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite database path override (sqlite driver only; default from config platform.store.sqlite_path)")
	return cmd
}

func driverName(cfg *config.Config) string {
	if cfg.Platform.Store.IsMySQL() {
		return config.StoreDriverMySQL
	}
	return config.StoreDriverSQLite
}
