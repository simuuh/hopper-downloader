package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"text/tabwriter"

	"github.com/simuuh/hopper-downloader/internal/queue"
)

func cmdAdd(args []string) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	priority := fs.String("priority", "normal", "low|normal|high")
	filename := fs.String("filename", "", "override destination filename")
	password := fs.String("password", "", "hoster/archive password")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: hopperctl add <url> [-priority low|normal|high] [-filename NAME] [-password PW]")
	}

	body := map[string]string{
		"url":      fs.Arg(0),
		"priority": *priority,
		"filename": *filename,
		"password": *password,
	}
	var item queue.Item
	if err := apiRequest(http.MethodPost, "/api/v1/links", body, &item); err != nil {
		return err
	}
	fmt.Printf("queued: %s (%s, priority=%s)\n", item.ID, item.URL, item.Priority)
	return nil
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	var items []queue.Item
	if err := apiRequest(http.MethodGet, "/api/v1/queue", nil, &items); err != nil {
		return err
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATE\tPRIORITY\tDOWNLOADED\tSIZE\tURL")
	for _, it := range items {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\n", it.ID, it.State, it.Priority, it.Downloaded, it.Size, it.URL)
	}
	return tw.Flush()
}

func cmdItemAction(args []string, action string) error {
	fs := flag.NewFlagSet(action, flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: hopperctl %s <id>", action)
	}
	id := fs.Arg(0)

	var method string
	path := "/api/v1/queue/" + id
	switch action {
	case "cancel":
		method = http.MethodDelete
	default:
		method = http.MethodPost
		path += "/" + action
	}

	if err := apiRequest(method, path, nil, nil); err != nil {
		return err
	}
	fmt.Printf("%s: %s\n", action, id)
	return nil
}

type statsResponse struct {
	TotalItems              int            `json:"total_items"`
	ByState                 map[string]int `json:"by_state"`
	TotalDownloadedBytes    int64          `json:"total_downloaded_bytes"`
	CurrentSpeedBytesPerSec int64          `json:"current_speed_bytes_per_sec"`
}

func cmdStats(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	var stats statsResponse
	if err := apiRequest(http.MethodGet, "/api/v1/stats", nil, &stats); err != nil {
		return err
	}
	fmt.Printf("total items:      %d\n", stats.TotalItems)
	for state, count := range stats.ByState {
		fmt.Printf("  %-12s %d\n", state, count)
	}
	fmt.Printf("downloaded bytes: %d\n", stats.TotalDownloadedBytes)
	fmt.Printf("current speed:    %d bytes/s\n", stats.CurrentSpeedBytesPerSec)
	return nil
}
