package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type eventJSON struct {
	ID      int64          `json:"id"`
	TS      int64          `json:"ts"`
	Level   string         `json:"level"`
	Kind    string         `json:"kind"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data"`
}

func newEventsCmd() *cobra.Command {
	var follow, asJSON bool
	var limit int
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Show prox events (-f to follow)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, c, err := clientConfig()
			if err != nil {
				return err
			}
			ctx := context.Background()
			if !follow {
				var events []eventJSON
				if err := c.Get(ctx, "/v1/events?limit="+strconv.Itoa(limit), &events); err != nil {
					return err
				}
				for _, ev := range events {
					printEvent(ev, asJSON)
				}
				return nil
			}
			stream, err := c.Stream(ctx, "/v1/events/stream")
			if err != nil {
				return err
			}
			defer stream.Close()
			sc := bufio.NewScanner(stream)
			sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
			for sc.Scan() {
				line := sc.Text()
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var ev eventJSON
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
					continue
				}
				printEvent(ev, asJSON)
			}
			return sc.Err()
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow the event stream")
	cmd.Flags().BoolVarP(&asJSON, "json", "j", false, "output JSON")
	cmd.Flags().IntVarP(&limit, "limit", "n", 50, "number of events to show")
	return cmd
}

func printEvent(ev eventJSON, asJSON bool) {
	if asJSON {
		b, _ := json.Marshal(ev)
		fmt.Println(string(b))
		return
	}
	ts := time.Unix(ev.TS, 0).Format("2006-01-02 15:04:05")
	level := ev.Level
	if level == "" {
		level = "info"
	}
	msg := ev.Message
	fmt.Printf("%s %-5s %-12s %s\n", ts, strings.ToUpper(level), ev.Kind, msg)
}
