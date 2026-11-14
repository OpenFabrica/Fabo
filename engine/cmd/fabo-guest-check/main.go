//go:build linux || darwin

// fabo-guest-check checks the startup exchange on the Linux host transport.
// Use a test agent and isolated engine state.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
	"github.com/OpenFabrica/FabriBay/engine/internal/hostagent"
)

func main() {
	dir := flag.String("agent-dir", "", "private directory shared with a manually started test agent")
	binary := flag.String("agent", "", "start this test agent in a private directory under /run")
	flag.Parse()
	var err error
	switch {
	case *dir != "" && *binary == "":
		err = check(*dir)
	case *binary != "" && *dir == "":
		err = checkProcess(*binary)
	default:
		err = fmt.Errorf("specify one of -agent-dir or -agent")
	}
	if err != nil {
		log.Fatal(err)
	}
}

func checkProcess(binary string) (err error) {
	p, err := hostagent.Start(context.Background(), binary, os.Stderr)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, p.Close()) }()
	return report(p.Conn, p.Hello)
}

func check(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("agent directory must be private (mode 0700)")
	}
	path := filepath.Join(dir, fmt.Sprintf("h%d.sock", guestwire.Port))
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return err
	}
	defer l.Close()
	if err := l.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return err
	}
	conn, err := l.AcceptUnix()
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	hello, err := guestwire.ReadHello(conn)
	if err != nil {
		return err
	}
	return report(conn, hello)
}

func report(conn *net.UnixConn, hello guestwire.Hello) error {
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	if err := guestwire.CheckPing(conn, uint64(time.Now().UnixNano())); err != nil {
		return err
	}
	stats, err := guestwire.ReadStats(conn)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		Hello guestwire.Hello
		Stats guestwire.Stats
	}{hello, stats})
}
