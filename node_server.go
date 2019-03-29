package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
)

type StartMigrationRequest struct {
	Pid         int32  // PID of process we're migration
	Destination string // Location we're migrating to
	Source      string // Location we're migrating from
}

type Process struct {
	Pid      int32    // PID of the process
	TcpPorts []uint16 // TCP ports the process listens on
	UdpPorts []uint16 // UDP ports the process listens on
}

var (
	mutex sync.Mutex = sync.Mutex{}
)

func RegisterProcessHandler(w http.ResponseWriter, r *http.Request) {
	// RegisterProcess() MUST be POST'd to!
	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	decoder := json.NewDecoder(r.Body)
	var request Process

	err := decoder.Decode(&request)
	if err != nil {
		fmt.Println("RegisterProcess(): poorly formatted request")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	go registerProcess(request)
}

func StartMigrationHandler(w http.ResponseWriter, r *http.Request) {
	// StartMigration() MUST be POST'd to!
	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	decoder := json.NewDecoder(r.Body)
	var request StartMigrationRequest

	err := decoder.Decode(&request)
	if err != nil {
		fmt.Println("StartMigration(): poorly formatted request")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	go doMigration(request)
}

func ForwardTrafficHandler(w http.ResponseWriter, r *http.Request) {
	// ForwardTraffic() MUST be POST'd to!
	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	decoder := json.NewDecoder(r.Body)
	var request ShadowTrafficMessage

	err := decoder.Decode(&request)
	if err != nil {
		fmt.Println("ForwardTraffic(): poorly formatted request")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// update the vector clock
	iclock, ok := MigrationClocks.Load(request.Pid)
	if !ok {
		fmt.Printf("ForwardTraffic(): no process %d for migration\n", request.Pid)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	clock, ok := iclock.(*MigrationClock)
	if !ok {
		fmt.Println("error: process not associated with *MigrationClock")
		return
	}

	mutex.Lock()
	clock.DestinationTime += 1
	clock.SourceTime = request.Clock.SourceTime
	mutex.Unlock()

	// TODO - actually forward the traffic!
	fmt.Println(request)
}

func SlaveStartMigrationHandler(w http.ResponseWriter, r *http.Request) {
	// SlaveStartMigration() MUST be POST'd to!
	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	decoder := json.NewDecoder(r.Body)
	var request SlaveStartMigrationMessage

	err := decoder.Decode(&request)
	if err != nil {
		fmt.Println("SlaveStartMigration(): poorly formatted request")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Processes and MigrationClocks are defined in migration.go
	Processes.Store(request.Process.Pid, request.Process)
	MigrationClocks.Store(request.Process.Pid, &request.Clock)

	fmt.Printf("Migration for %d started...\n", request.Process.Pid)

	// TODO - create a new network namespace.
	// Then, create a veth pair and connect to bridge. do not update route tables yet.
	// Write the raw ethernet frame to the veth pair
}

func ReceiveCheckpointHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		fmt.Println("ReceiveCheckpointHandler(): error parsing")
		return
	}

	spid, ok := r.Form["pid"]
	if !ok {
		fmt.Println("ReceiveCheckpointHandler(): no pid")
		return
	}

	pid, err := strconv.ParseInt(spid[0], 10, 32)
	if err != nil {
		fmt.Println("ReceiveCheckpointHandler(): non-numeric PID")
		return
	}

	file, err := os.Create(fmt.Sprintf("./%d.tar.gz", pid))
	if err != nil {
		fmt.Println("ReceiveCheckpointHandler(): can't create file")
		return
	}
	defer file.Close()

	io.Copy(file, r.Body)
}
