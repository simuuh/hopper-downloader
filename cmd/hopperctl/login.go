package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

type loginResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func cmdLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	server := fs.String("server", envOr("HOPPER_SERVER", "http://127.0.0.1:9667"), "hopperd API base URL")
	fs.Usage = func() {
		fmt.Fprintf(os.Stdout, "%s\n", `Usage: printf '%s' 'your-password' | hopperctl login -server http://host:9667`)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	pw, err := readStdinTrimmed()
	if err != nil {
		return fmt.Errorf("read password from stdin: %w", err)
	}
	if pw == "" {
		return fmt.Errorf("no password provided on stdin")
	}

	sess := &session{Server: strings.TrimRight(*server, "/")}
	var resp loginResponse
	body := map[string]string{"username": "admin", "password": pw}

	if err := apiRequestUnauthenticated("POST", sess.Server+"/api/v1/auth/login", body, &resp); err != nil {
		return fmt.Errorf("login failed: %w", err)
	}

	sess.Token = resp.Token
	if err := saveSession(sess); err != nil {
		return fmt.Errorf("save session: %w", err)
	}
	fmt.Printf("Logged in to %s (token expires %s)\n", sess.Server, resp.ExpiresAt.Format(time.RFC3339))
	return nil
}
