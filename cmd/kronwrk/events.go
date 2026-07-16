package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"kronwrk/internal/models"
)

// Event gating commands: `event emit|list` and `job condition add|list|remove`.
// A job with conditions has its runs enqueued waiting; the scheduler promotes
// them once every condition has a matching unconsumed event (see 0014).

// maxPayloadBytes bounds an event's JSON payload so a single emit can't bloat
// the events table (unconsumed events live forever — there is no GC yet).
const maxPayloadBytes = 64 << 10 // 64 KiB

// --- event ---

func eventCmd() *cobra.Command {
	c := &cobra.Command{Use: "event", Short: "Emit and inspect gating events"}
	c.AddCommand(eventEmitCmd(), eventListCmd())
	return c
}

func eventEmitCmd() *cobra.Command {
	var payload string
	c := &cobra.Command{
		Use:   "emit <name>",
		Short: "Emit an event (satisfies waiting runs conditioned on it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, cmdArgs []string) error {
			name := cmdArgs[0]
			var raw []byte
			if payload != "" {
				if len(payload) > maxPayloadBytes {
					return fmt.Errorf("--payload too large (%d bytes; max %d)", len(payload), maxPayloadBytes)
				}
				if !json.Valid([]byte(payload)) {
					return fmt.Errorf("--payload must be valid JSON")
				}
				raw = []byte(payload)
			}
			ctx := cmd.Context()
			store, err := getStore(ctx)
			if err != nil {
				return err
			}

			e, err := store.EmitEvent(ctx, name, raw)
			if err != nil {
				return permissionErr(ctx, store, err, "emit events", "the admin or operator role")
			}
			fmt.Printf("emitted event %d (%s); waiting runs are matched on the scheduler's next tick\n", e.ID, e.Name)
			return nil
		},
	}
	c.Flags().StringVar(&payload, "payload", "", "optional JSON payload")
	return c
}

func eventListCmd() *cobra.Command {
	var limit int
	c := &cobra.Command{
		Use:   "list",
		Short: "List emitted events, newest first",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			store, err := getStore(ctx)
			if err != nil {
				return err
			}

			events, err := store.ListEvents(ctx, limit)
			if err != nil {
				return err
			}
			if len(events) == 0 && stdoutIsTTY() {
				fmt.Println(faintText("No events yet — emit one with `event emit <name>`."))
				return nil
			}
			return printTable(eventsTableView(events))
		},
	}
	c.Flags().IntVar(&limit, "limit", 50, "maximum number of events to show")
	return c
}

// eventsTableView shapes events for display; consumed events are dimmed the
// way disabled jobs are — still visible, no longer available to gate a run.
func eventsTableView(events []models.Event) tableView {
	v := tableView{columns: []string{"ID", "NAME", "EMITTED BY", "EMITTED AT", "CONSUMED BY RUN", "PAYLOAD"}}
	for _, e := range events {
		v.rows = append(v.rows, []string{
			strconv.FormatInt(e.ID, 10),
			e.Name,
			e.EmittedBy,
			e.EmittedAt.Format("2006-01-02 15:04:05"),
			fmtInt64(e.ConsumedByRunID),
			truncate(string(e.Payload), 40),
		})
		v.dim = append(v.dim, e.ConsumedByRunID != nil)
	}
	return v
}

func fmtInt64(v *int64) string {
	if v == nil {
		return "-"
	}
	return strconv.FormatInt(*v, 10)
}

// --- job condition ---

func jobConditionCmd() *cobra.Command {
	c := &cobra.Command{Use: "condition", Short: "Gate a job's runs on events"}
	c.AddCommand(jobConditionAddCmd(), jobConditionListCmd(), jobConditionRemoveCmd())
	return c
}

func jobConditionAddCmd() *cobra.Command {
	var eventName, wait string
	c := &cobra.Command{
		Use:   "add <job-id>",
		Short: "Make the job's runs wait for an event before executing",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, cmdArgs []string) error {
			id, err := strconv.ParseInt(cmdArgs[0], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid job id: %w", err)
			}
			if eventName == "" {
				return fmt.Errorf("missing required flag: --event")
			}
			waitSeconds, err := parseWaitFlag(wait)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			store, err := getStore(ctx)
			if err != nil {
				return err
			}

			if err := store.AddJobCondition(ctx, id, eventName, waitSeconds); err != nil {
				return permissionErr(ctx, store, err, "add job conditions", "the admin role")
			}
			window := "forever"
			if waitSeconds > 0 {
				window = "up to " + (time.Duration(waitSeconds) * time.Second).String()
			}
			fmt.Printf("job %d now waits for event %q (%s) before each run\n", id, eventName, window)
			return nil
		},
	}
	c.Flags().StringVar(&eventName, "event", "", "event name the job's runs wait for (required)")
	c.Flags().StringVar(&wait, "wait", "", "how long past the scheduled time to wait before skipping, e.g. 30m (default: forever)")
	return c
}

// parseWaitFlag converts a --wait duration to whole seconds. Empty means wait
// forever (0). Sub-second and negative durations are rejected.
func parseWaitFlag(s string) (int, error) {
	if s == "" || s == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid --wait duration: %w", err)
	}
	if d < 0 {
		return 0, fmt.Errorf("--wait must not be negative")
	}
	if d%time.Second != 0 {
		return 0, fmt.Errorf("--wait must be whole seconds (got %s)", d)
	}
	return int(d / time.Second), nil
}

func jobConditionListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list <job-id>",
		Short: "List the events a job's runs wait for",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, cmdArgs []string) error {
			id, err := strconv.ParseInt(cmdArgs[0], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid job id: %w", err)
			}
			ctx := cmd.Context()
			store, err := getStore(ctx)
			if err != nil {
				return err
			}

			conds, err := store.ListJobConditions(ctx, id)
			if err != nil {
				return err
			}
			if len(conds) == 0 {
				fmt.Printf("job %d has no conditions — its runs are time-triggered only\n", id)
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "EVENT\tWAIT")
			for _, c := range conds {
				window := "forever"
				if c.WaitSeconds > 0 {
					window = (time.Duration(c.WaitSeconds) * time.Second).String()
				}
				fmt.Fprintf(tw, "%s\t%s\n", c.EventName, window)
			}
			return tw.Flush()
		},
	}
}

func jobConditionRemoveCmd() *cobra.Command {
	var eventName string
	c := &cobra.Command{
		Use:   "remove <job-id>",
		Short: "Drop a job condition (removing a job's last condition un-gates its waiting runs)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, cmdArgs []string) error {
			id, err := strconv.ParseInt(cmdArgs[0], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid job id: %w", err)
			}
			if eventName == "" {
				return fmt.Errorf("missing required flag: --event")
			}
			ctx := cmd.Context()
			store, err := getStore(ctx)
			if err != nil {
				return err
			}

			if err := store.RemoveJobCondition(ctx, id, eventName); err != nil {
				return permissionErr(ctx, store, err, "remove job conditions", "the admin role")
			}
			fmt.Printf("job %d no longer waits for event %q\n", id, eventName)
			return nil
		},
	}
	c.Flags().StringVar(&eventName, "event", "", "event name of the condition to remove (required)")
	return c
}
