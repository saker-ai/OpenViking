package migrate

import (
	"context"
	"fmt"
	"io"

	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"
)

// queuefsCmd implements `migrate queuefs`. Redis holds queue state as keys
// with TTLs, so there is no schema to migrate; the command verifies
// connectivity and reports the count of stale (expired-but-not-evicted)
// asynq queue keys.
func queuefsCmd() *cobra.Command {
	var (
		addr    string
		password string
		db      int
		flush   bool
		dryRun  bool
	)
	cmd := &cobra.Command{
		Use:   "queuefs",
		Short: "Verify queuefs Redis state and report stale queues",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runQueuefsMigrate(cmd.Context(), cmd.OutOrStdout(),
				addr, password, db, flush, dryRun)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "", "redis host:port (e.g. 127.0.0.1:6379)")
	cmd.Flags().StringVar(&password, "password", "", "redis password (optional)")
	cmd.Flags().IntVar(&db, "db", 0, "redis logical db index")
	cmd.Flags().BoolVar(&flush, "flush-stale", false, "delete stale queue keys (asynq:*, default false)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print connectivity + stale-key report without deleting")
	_ = cmd.MarkFlagRequired("addr")
	return cmd
}

// runQueuefsMigrate is the testable core of `migrate queuefs`.
func runQueuefsMigrate(ctx context.Context, out io.Writer,
	addr, password string, db int, flush, dryRun bool) error {
	if addr == "" {
		return fmt.Errorf("migrate queuefs: --addr is required")
	}
	cli := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})
	defer cli.Close()

	if err := cli.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("migrate queuefs: redis ping %s: %w", addr, err)
	}

	stale, err := countStaleQueues(ctx, cli)
	if err != nil {
		return fmt.Errorf("migrate queuefs: %w", err)
	}

	if dryRun {
		printQueuefsDryRun(out, addr, db, stale)
		return nil
	}

	if flush && stale > 0 {
		if err := deleteStaleQueues(ctx, cli); err != nil {
			return fmt.Errorf("migrate queuefs: flush: %w", err)
		}
		printQueuefsFlushed(out, addr, db, stale)
		return nil
	}
	printQueuefsReport(out, addr, db, stale)
	return nil
}

// countStaleQueues counts asynq-managed keys that look like dead-state
// queue entries. asynq stores pending tasks under "asynq:{queue}:pending"
// and similar keys; a "stale" key is one whose TTL is -1 (no expiry) on
// a queue that should be transient. We count keys with the asynq: prefix
// that have no TTL set, since queuefs tasks should always have a TTL or
// be archived.
func countStaleQueues(ctx context.Context, cli *redis.Client) (int, error) {
	var count int
	var cursor uint64
	for {
		keys, next, err := cli.Scan(ctx, cursor, "asynq:*", 256).Result()
		if err != nil {
			return 0, fmt.Errorf("scan asynq keys: %w", err)
		}
		for _, k := range keys {
			ttl, err := cli.TTL(ctx, k).Result()
			if err != nil {
				continue
			}
			// TTL -1 = no expiry, -2 = key doesn't exist. Only -1 counts
			// as stale; transient keys with TTLs are healthy.
			if ttl == -1 {
				count++
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return count, nil
}

// deleteStaleQueues removes asynq:* keys with no TTL. Used by the
// --flush-stale recovery flag.
func deleteStaleQueues(ctx context.Context, cli *redis.Client) error {
	var cursor uint64
	for {
		keys, next, err := cli.Scan(ctx, cursor, "asynq:*", 256).Result()
		if err != nil {
			return fmt.Errorf("scan asynq keys: %w", err)
		}
		var stale []string
		for _, k := range keys {
			ttl, err := cli.TTL(ctx, k).Result()
			if err != nil {
				continue
			}
			if ttl == -1 {
				stale = append(stale, k)
			}
		}
		if len(stale) > 0 {
			if err := cli.Del(ctx, stale...).Err(); err != nil {
				return fmt.Errorf("del stale: %w", err)
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return nil
}

func printQueuefsDryRun(out io.Writer, addr string, db, stale int) {
	fmt.Fprintf(out, "queuefs migrate --dry-run --addr %s --db %d\n", addr, db)
	fmt.Fprintf(out, "  ping: ok\n")
	fmt.Fprintf(out, "  stale queue keys: %d\n", stale)
	fmt.Fprintln(out, "  plan: no deletions (dry-run)")
}

func printQueuefsReport(out io.Writer, addr string, db, stale int) {
	fmt.Fprintf(out, "queuefs migrate --addr %s --db %d\n", addr, db)
	fmt.Fprintf(out, "  ping: ok\n")
	fmt.Fprintf(out, "  stale queue keys: %d (use --flush-stale to remove)\n", stale)
}

func printQueuefsFlushed(out io.Writer, addr string, db, stale int) {
	fmt.Fprintf(out, "queuefs migrate --addr %s --db %d\n", addr, db)
	fmt.Fprintf(out, "  ping: ok\n")
	fmt.Fprintf(out, "  flushed %d stale queue key(s)\n", stale)
}
