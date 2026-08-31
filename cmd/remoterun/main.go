package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"time"

	"golang.org/x/crypto/ssh"
)

func main() {
	host := flag.String("host", "", "host:port")
	user := flag.String("user", "root", "ssh user")
	pass := flag.String("pass", os.Getenv("ROOTPASS"), "ssh password")
	flag.Parse()
	cmd := flag.Arg(0)
	if cmd == "" {
		fmt.Fprintln(os.Stderr, "usage: remoterun [flags] 'command'")
		os.Exit(2)
	}

	config := &ssh.ClientConfig{
		User:            *user,
		Auth:            []ssh.AuthMethod{ssh.Password(*pass)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}

	client, err := ssh.Dial("tcp", *host, config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ssh dial failed: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		fmt.Fprintf(os.Stderr, "new session failed: %v\n", err)
		os.Exit(1)
	}
	defer session.Close()

	var out bytes.Buffer
	var errbuf bytes.Buffer
	session.Stdout = &out
	session.Stderr = &errbuf

	if err := session.Run(cmd); err != nil {
		fmt.Fprintf(os.Stderr, "run failed: %v\n", err)
	}
	fmt.Print(out.String())
	if errbuf.Len() > 0 {
		fmt.Fprintln(os.Stderr, "--- stderr ---")
		fmt.Fprint(os.Stderr, errbuf.String())
	}
}
