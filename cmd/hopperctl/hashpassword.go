package main

import (
	"flag"
	"fmt"
	"os"

	"golang.org/x/crypto/bcrypt"
)

// cmdHashPassword reads a password from stdin (never from an argument or
// flag, to avoid it ending up in shell history or `ps`) and prints its
// bcrypt hash, for pasting into hopper.yaml's server.admin_password_hash.
func cmdHashPassword(args []string) error {
	fs := flag.NewFlagSet("hash-password", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "%s\n", `Usage: printf '%s' 'your-password' | hopperctl hash-password`)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	pw, err := readStdinTrimmed()
	if err != nil {
		return fmt.Errorf("read password from stdin: %w", err)
	}
	if pw == "" {
		return fmt.Errorf("no password provided on stdin (usage: printf '%%s' 'pw' | hopperctl hash-password)")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	fmt.Println(string(hash))
	return nil
}
