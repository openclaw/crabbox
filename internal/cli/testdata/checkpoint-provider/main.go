//go:build !windows

// This secretless executable models only the native CLI commands used by the
// killed-process contract test. Keeping it independent of the CLI test binary
// avoids initializing unrelated provider packages within the 300ms kill grace.
package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const captureFixtureEnv = "CRABBOX_CAPTURE_BINARY_FIXTURE"

func fail(format any, args ...any) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, format)
	} else {
		fmt.Fprintf(os.Stderr, fmt.Sprint(format)+"\n", args...)
	}
	os.Exit(1)
}

type checkpointCaptureFixtureState struct {
	Machine           map[string]any    `json:"machine"`
	Image             map[string]any    `json:"image"`
	Metadata          map[string]string `json:"metadata"`
	StartRunning      bool              `json:"startRunning"`
	Ready             bool              `json:"ready"`
	Failed            bool              `json:"failed"`
	Pause             string            `json:"pause"`
	ReplaceStopped    bool              `json:"replaceStopped"`
	ReplaceAfterFlush bool              `json:"replaceAfterFlush"`
	Saves             int               `json:"saves"`
	Creates           int               `json:"creates"`
	Stops             int               `json:"stops"`
	Starts            int               `json:"starts"`
	Removes           int               `json:"removes"`
	Version           int               `json:"version"`
}

type checkpointCaptureFixtureEvent struct {
	Kind  string `json:"kind"`
	PID   int    `json:"pid"`
	PGID  int    `json:"pgid"`
	Owner int    `json:"owner"`
}

func main() {
	root := os.Getenv(captureFixtureEnv)
	if root == "" {
		fail("missing fixture root")
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) < 3 {
		fail("missing fixture command")
	}
	kind, args := args[1], args[2:]
	statePath := filepath.Join(root, "provider.json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		fail(err)
	}
	var state checkpointCaptureFixtureState
	if err := json.Unmarshal(data, &state); err != nil {
		fail(err)
	}
	persist := func() {
		data, err := json.Marshal(state)
		if err != nil {
			fail(err)
		}
		if err := os.WriteFile(statePath, data, 0o600); err != nil {
			fail(err)
		}
	}
	event := func(kind string) {
		var gate *os.File
		if state.Pause == kind {
			path := filepath.Join(root, "withheld-response")
			if err := syscall.Mkfifo(path, 0o600); err != nil && err != syscall.EEXIST {
				fail(err)
			}
			gate, err = os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				fail(err)
			}
			defer gate.Close()
		}
		encoded, _ := json.Marshal(checkpointCaptureFixtureEvent{Kind: kind, PID: os.Getpid(), PGID: syscall.Getpgrp(), Owner: os.Getppid()})
		encoded = append(encoded, '\n')
		log, err := os.OpenFile(filepath.Join(root, "events.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			fail(err)
		}
		_, _ = log.Write(encoded)
		_ = log.Close()
		fifo, err := os.OpenFile(filepath.Join(root, "events.fifo"), os.O_WRONLY, 0)
		if err != nil {
			fail(err)
		}
		_, _ = fifo.Write(encoded)
		_ = fifo.Close()
		if gate != nil {
			// Deliberately withhold the native command response after its effect.
			// The test either releases this read or kills the exact process group.
			_, _ = gate.Read(make([]byte, 1))
		}
	}
	output := func(value any) { _ = json.NewEncoder(os.Stdout).Encode(value) }
	if kind == "ssh" {
		command := strings.Join(args, " ")
		action := "ssh-ready"
		if strings.Contains(command, "fixture-scrub") {
			action = "scrub"
		} else if strings.Contains(command, "cloud-init") {
			action = "flush"
			if state.ReplaceAfterFlush {
				state.Machine["id"] = "fixture-replacement-immutable-id"
				persist()
			}
		}
		event(action)
		os.Exit(0)
	}
	if args[0] == "get" || args[0] == "stop" || args[0] == "start" || args[0] == "rm" {
		if state.Machine != nil && (len(args) < 2 || args[1] != state.Machine["name"]) {
			fail("fixture only supports the native named mutation/read contract: %v", args)
		}
	}
	switch args[0] {
	case "get":
		if state.Machine == nil {
			fmt.Fprintln(os.Stderr, "fixture machine not found")
			os.Exit(1)
		}
		if state.ReplaceStopped && state.Machine["status"] == "STOPPED" {
			state.Machine["id"] = "fixture-replacement-immutable-id"
			persist()
		}
		event("get")
		if state.Pause == "get" {
			// Return the observation at release, after the competing owner ran.
			data, err := os.ReadFile(statePath)
			if err != nil {
				fail(err)
			}
			if err := json.Unmarshal(data, &state); err != nil {
				fail(err)
			}
		}
		output(state.Machine)
	case "ls":
		event("inventory")
		machines := []map[string]any{}
		if state.Machine != nil {
			summary := maps.Clone(state.Machine)
			delete(summary, "imageVersion") // Native list omits this detail-only field.
			machines = append(machines, summary)
		}
		output(machines)
	case "sizes":
		output([]map[string]any{{"size": "large", "vcpu": 2, "ramGb": 4, "diskGb": 80, "regions": []string{"eu"}}})
	case "keys":
		if len(args) != 3 || args[1] != "ls" || args[2] != "--json" {
			fail("unsupported key lookup: %v", args)
		}
		output([]any{})
	case "new":
		if state.Machine != nil || len(args) != 10 || args[2] != "--size" || args[4] != "--region" || args[6] != "--image" || args[8] != "--image-version" {
			fail("unsupported or duplicate fixture create: %v", args)
		}
		version, err := strconv.Atoi(args[9])
		if err != nil || version != 1 || state.Image == nil || args[7] != state.Image["name"] || !state.Ready {
			fail("create requires retained ready image version 1: %v", args)
		}
		state.Machine = map[string]any{"id": "fixture-fork-immutable-id", "name": args[1], "status": "RUNNING", "ip": "127.0.0.1", "size": args[3], "region": args[5], "image": args[7], "imageVersion": version, "defaultSSHUsername": "ubuntu"}
		state.Creates++
		persist()
		event("created")
	case "stop":
		if state.Machine["status"] != "RUNNING" {
			fmt.Fprintln(os.Stderr, "fixture source cannot stop while transitioning")
			os.Exit(1)
		}
		state.Machine["status"] = "STOPPED"
		state.Stops++
		persist()
		event("stopped")
	case "suspend":
		state.Machine["status"] = "SUSPENDED"
		persist()
		event("suspended")
	case "start":
		state.Starts++
		state.Machine["status"] = "STARTING"
		if state.StartRunning {
			state.Machine["status"] = "RUNNING"
		}
		persist()
		event("started")
	case "rm":
		if state.Machine["status"] == "STARTING" || state.Machine["status"] == "STOPPING" {
			fmt.Fprintln(os.Stderr, "fixture source cannot remove while transitioning")
			os.Exit(1)
		}
		state.Removes++
		state.Machine = nil
		persist()
		event("removed")
	case "images":
		switch args[1] {
		case "ls":
			images := []map[string]any{}
			if state.Image != nil {
				images = append(images, state.Image)
			}
			event("images-inventory")
			output(images)
		case "save":
			if state.Machine["status"] != "STOPPED" || args[2] != state.Machine["name"] {
				fail("save requires stopped fixture source")
			}
			state.Saves++
			state.Image = map[string]any{"id": "fixture-image-immutable-id", "name": args[3], "status": "DRAFT"}
			if len(args) != 6 || args[4] != "--metadata" {
				fail("unsupported native save: %v", args)
			}
			if err := json.Unmarshal([]byte(args[5]), &state.Metadata); err != nil {
				fail(err)
			}
			persist()
			event("saved")
		case "get":
			if state.Image == nil || args[2] != state.Image["name"] {
				fail("fixture image absent")
			}
			snapshot := "CREATING"
			if state.Ready {
				snapshot = "READY"
			}
			if state.Failed {
				snapshot = "FAILED"
			}
			event("image")
			version := state.Version
			if version == 0 {
				version = 1
			}
			output(map[string]any{"image": state.Image, "versions": []map[string]any{{"id": "fixture-version-id", "version": version, "status": "DRAFT", "snapshotStatus": snapshot, "metadata": state.Metadata}}})
		case "rm":
			if state.Image == nil || args[2] != state.Image["name"] {
				fail("fixture image absent")
			}
			state.Image = nil
			persist()
			event("image-removed")
		default:
			fail("unsupported native image command: %v", args)
		}
	default:
		fail("unsupported native command: %v", args)
	}
	os.Exit(0)
}
