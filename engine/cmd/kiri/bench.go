//go:build linux || darwin

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const benchUsage = "usage: fabo bench [--runs N] [--image IMAGE] [--network none|bridge] [--only kiri|orbstack|docker]"

type benchOptions struct {
	runs                 int
	image, network, only string
}

func parseBench(args []string) (benchOptions, error) {
	o := benchOptions{50, "alpine:latest", "none", ""}
	for len(args) > 0 {
		if len(args) < 2 {
			return o, errors.New(benchUsage)
		}
		v := args[1]
		switch args[0] {
		case "--runs":
			n, e := strconv.Atoi(v)
			if e != nil || n <= 0 {
				return o, errors.New("runs must be a positive integer")
			}
			o.runs = n
		case "--image":
			if v == "" || strings.HasPrefix(v, "-") {
				return o, errors.New(benchUsage)
			}
			o.image = v
		case "--network":
			if v != "none" && v != "bridge" {
				return o, errors.New(benchUsage)
			}
			o.network = v
		case "--only":
			v = strings.ToLower(v)
			if v != "kiri" && v != "orbstack" && v != "docker" {
				return o, errors.New(benchUsage)
			}
			o.only = v
		default:
			return o, errors.New(benchUsage)
		}
		args = args[2:]
	}
	return o, nil
}

type benchRow struct {
	name, path string
	times      []time.Duration
}

func benchPercentile(times []time.Duration, percent int) time.Duration {
	if len(times) == 0 {
		return 0
	}
	sorted := slices.Clone(times)
	slices.Sort(sorted)
	return sorted[min(len(sorted)*percent/100, len(sorted)-1)]
}
func benchAPI(c *commandClient, o benchOptions) ([]time.Duration, error) {
	c.http.Timeout = 30 * time.Second
	times := []time.Duration{}
	for n := 0; n < o.runs+5; n++ {
		started := time.Now()
		var created struct{ ID string }
		body := map[string]any{"Image": o.image, "Cmd": []string{"true"}, "HostConfig": map[string]any{"AutoRemove": true, "NetworkMode": o.network}}
		if e := c.request("POST", "/containers/create", body, &created); e != nil {
			return nil, e
		}
		if created.ID == "" {
			return nil, errors.New("create returned no container ID")
		}
		e := func() error {
			path := "/containers/" + created.ID
			// Cleanup only this benchmark's container when a stage fails.
			completed := false
			defer func() {
				if !completed {
					_ = c.request("DELETE", path+"?force=1&v=1", nil, nil)
				}
			}()
			req, e := http.NewRequest("POST", "http://kiri"+path+"/wait?condition=removed", nil)
			if e != nil {
				return e
			}
			reply, e := c.http.Do(req)
			if e != nil {
				return e
			}
			defer reply.Body.Close()
			if reply.StatusCode != 200 {
				return commandResponseError(reply)
			}
			if e = c.request("POST", path+"/start", nil, nil); e != nil {
				return e
			}
			var result struct {
				StatusCode int
				Error      *struct{ Message string }
			}
			if e = json.NewDecoder(reply.Body).Decode(&result); e != nil {
				return e
			}
			if result.Error != nil && result.Error.Message != "" {
				return errors.New(result.Error.Message)
			}
			if result.StatusCode != 0 {
				return fmt.Errorf("benchmark container exited %d", result.StatusCode)
			}
			completed = true
			return nil
		}()
		if e != nil {
			return nil, e
		}
		if n >= 5 {
			times = append(times, time.Since(started))
		}
	}
	return times, nil
}
func benchCommand(args []string) error {
	o, e := parseBench(args)
	if e != nil {
		return e
	}
	in, e := locate()
	if e != nil {
		return e
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	fmt.Printf("Container lifecycle: %s → true → exit → removed, %d runs (+5 warm-up)\n\n", o.image, o.runs)
	rows := []benchRow{}
	targets := []struct{ name, key, socket string }{{"Kiri daemon", "kiri", in.socket()}, {"OrbStack", "orbstack", filepath.Join(in.home, ".orbstack/run/docker.sock")}, {"Docker Desktop", "docker", filepath.Join(in.home, ".docker/run/docker.sock")}}
	for _, target := range targets {
		if o.only != "" && o.only != target.key {
			continue
		}
		c := newCommandClient(target.socket)
		up := alive(c.http)
		if !up {
			c.http.CloseIdleConnections()
			if target.key == "kiri" && o.network == "none" {
				engine, launch, e := in.rawLaunch([]string{"bench", strconv.Itoa(o.runs), o.image})
				if e != nil {
					return e
				}
				cmd := exec.CommandContext(ctx, engine, launch...)
				cmd.Stderr = os.Stderr
				raw, e := cmd.Output()
				if e != nil {
					return e
				}
				var result struct {
					Nanoseconds    []int64
					SetupNS, RunNS uint64
				}
				if e = json.Unmarshal(raw, &result); e != nil {
					return e
				}
				if len(result.Nanoseconds) != o.runs {
					return errors.New("raw benchmark returned incomplete samples")
				}
				times := []time.Duration{}
				for _, ns := range result.Nanoseconds {
					times = append(times, time.Duration(ns))
				}
				rows = append(rows, benchRow{"Kiri — engine (guest protocol)", "API", times})
				fmt.Printf("  Kiri guest-side mean: setup %.2f ms, process %.2f ms\n", float64(result.SetupNS)/float64(o.runs)/1e6, float64(result.RunNS)/float64(o.runs)/1e6)
			} else {
				fmt.Printf("  %s: not running, skipped\n", target.name)
			}
			continue
		}
		fmt.Fprintf(os.Stderr, "  measuring %s…\n", target.name)
		times, e := benchAPI(c, o)
		c.http.CloseIdleConnections()
		if e != nil {
			return fmt.Errorf("%s: %w", target.name, e)
		}
		rows = append(rows, benchRow{target.name + " — Docker API, --network " + o.network, "API", times})
		docker := in.dockerCLI()
		if docker == "" {
			return errors.New("no docker CLI found")
		}
		times = nil
		for n := 0; n < o.runs+5; n++ {
			t := time.Now()
			runCtx, stopRun := context.WithTimeout(ctx, 30*time.Second)
			cmd := exec.CommandContext(runCtx, docker, "run", "--rm", "--network", o.network, o.image, "true")
			cmd.Env = in.dockerEnvironment()
			for k, v := range cmd.Env {
				if strings.HasPrefix(v, "DOCKER_HOST=") {
					cmd.Env[k] = "DOCKER_HOST=unix://" + target.socket
				}
			}
			cmd.Stdout = io.Discard
			cmd.Stderr = os.Stderr
			err := cmd.Run()
			stopRun()
			if e := err; e != nil {
				return fmt.Errorf("%s docker run: %w", target.name, e)
			}
			if n >= 5 {
				times = append(times, time.Since(t))
			}
		}
		rows = append(rows, benchRow{target.name + " — docker CLI, --network " + o.network, "CLI", times})
	}
	if len(rows) == 0 {
		return errors.New("no selected engine is running")
	}
	fmt.Printf("%-50s %10s %10s %10s\n", "", "p50", "p99", "mean")
	for _, row := range rows {
		var sum time.Duration
		for _, t := range row.times {
			sum += t
		}
		ratio := ""
		for _, base := range rows {
			if strings.HasPrefix(base.name, "Kiri") && base.path == row.path && !strings.HasPrefix(row.name, "Kiri") {
				if p := benchPercentile(base.times, 50); p > 0 {
					ratio = fmt.Sprintf("   %.0f× Kiri", float64(benchPercentile(row.times, 50))/float64(p))
				}
				break
			}
		}
		fmt.Printf("%-50s %7.2f ms %7.2f ms %7.2f ms%s\n", row.name, float64(benchPercentile(row.times, 50))/1e6, float64(benchPercentile(row.times, 99))/1e6, float64(sum)/float64(len(row.times))/1e6, ratio)
	}
	return nil
}
