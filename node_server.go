package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"	
	"net/http"
	"os"
	"strconv"
	"sync"
	"github.com/mholt/archiver"	
)

type StartMigrationRequest struct {
	Pid         int32  // PID of process we're migration
	Destination string // Location we're migrating to
	Source      string // Location we're migrating from
}

type Process struct {
	Pid      int32     // PID of the process
	TcpPorts []uint16  // TCP ports the process listens on
	UdpPorts []uint16  // UDP ports the process listens on
	DoneChan chan bool   `json:"-"` // channel used to end traffic forwarding
	Packets  [][]byte    `json:"-"` // each []byte is a packet
	Lock     *sync.Mutex `json:"-"` // lock
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
		panic("error: process not associated with *MigrationClock")
	}

	mutex.Lock()
	clock.DestinationTime += 1
	clock.SourceTime = request.Clock.SourceTime
	mutex.Unlock()

	iprocess, ok := Processes.Load(request.Pid)
	if !ok {
		fmt.Println("ForwardTraffic(): no pid in Processes for migration")
		return
	}

	process, ok := iprocess.(Process)
	if !ok {
		panic("error: pid not associated with a Process in Processes")
	}

	// add the frames
	process.Lock.Lock()
	process.Packets = append(process.Packets, request.Frame)
	process.Lock.Unlock()

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

	// create a lock for this process
	mutex := sync.Mutex{}
	request.Process.Lock = &mutex

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

	filename := fmt.Sprintf("./%d.tar.gz", pid)
	file, err := os.Create(filename)
	if err != nil {
		fmt.Println("ReceiveCheckpointHandler(): can't create file")
		return
	}
	defer file.Close()

	io.Copy(file, r.Body)

	dst := fmt.Sprintf("./%d-restore/", pid)
	if err = archiver.Unarchive(filename, dst); err != nil {
		fmt.Println("ReceiveCheckpointHandler(): unable to unarchive file")
		return
	}

	// find where this was extracted to
	files, err := ioutil.ReadDir(dst)
	if err != nil {
		fmt.Println("ReceiveCheckpointHandler(): unable to read directory")
		return
	} else if len(files) != 1 {
		fmt.Println("ReceiveCheckpointHandler(): unexpected archive directory")
		return
	}

	dirName := fmt.Sprintf("./%s/%s", dst, files[0].Name())

	if err = restoreFromFile(dirName); err != nil {
		fmt.Println("ReceiveCheckpointHandler(): can't restore from file")
		return
	}
}

func FinishRestoreHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		fmt.Println("FinishCheckpointHandler(): error parsing")
	}

	spid, ok := r.Form["pid"]
	if !ok {
		fmt.Println("FinishCheckpointHandler(): no pid")
		return
	}

	pid, err := strconv.ParseInt(spid[0], 10, 32)
	if err != nil {
		fmt.Println("FinishCheckpointHandler(): non-numeric PID")
		return
	}

	process, err := os.FindProcess(int(pid))

	// TODO - we should see about garbage collecting all the system resources
	// (e.g. veth pairs) used by process

	if err != nil {
		fmt.Println("FinishRestoreHandler(): unable to locate pid")
		return
	}

	if err = process.Kill(); err != nil {
		fmt.Println("FinishRestoreHandler(): unable to kill pid")
		return
	}

	iinternalProcess, ok := Processes.Load(pid)
	if !ok {
		fmt.Println("FinishRestoreHandler(): unable to load pid")
		return
	}

	internalProcess, ok := iinternalProcess.(Process)
	if !ok {
		fmt.Println("FinishRestoreHandler(): pid not associated with process")
		return
	}

	internalProcess.DoneChan <- true
}
