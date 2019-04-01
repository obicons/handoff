package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/checkpoint-restore/go-criu"
	"github.com/checkpoint-restore/go-criu/rpc"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"
	"github.com/shirou/gopsutil/process"
	"github.com/mholt/archiver"
	"net"
	"net/http"
	"os"
	//"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type MigrationClock struct {
	SourceTime      uint64 // time at the source node
	DestinationTime uint64 // time at the destination node
}

type SlaveStartMigrationMessage struct {
	Clock   MigrationClock
	Process Process
}

type ShadowTrafficMessage struct {
	Clock MigrationClock
	Frame []byte
	Pid   int32
}

var (
	MigrationClocks *sync.Map = new(sync.Map)
	Processes       *sync.Map = new(sync.Map)
)

type criuNotifier struct {
	targetAddr string
	imageDir string
	pid      int32
}

func (c criuNotifier) PreDump() error { return nil }
func (c criuNotifier) PreRestore() error { return nil }
func (c criuNotifier) PostRestore(p int32) error { return nil }
func (c criuNotifier) NetworkLock() error { return nil }
func (c criuNotifier) NetworkUnlock() error { return nil }
func (c criuNotifier) SetupNamespaces(p int32) error { return nil }
func (c criuNotifier) PostSetupNamespaces() error { return nil }

// after we finish a dump, we send it to the destination of the migration
func (c criuNotifier) PostDump() error {
	compressor := archiver.NewTarGz()
	if err := compressor.Archive([]string{c.imageDir}, c.imageDir + ".tar.gz"); err != nil {
		fmt.Println(err)
		return nil
	}

	file, err := os.Open(c.imageDir + ".tar.gz")
	if err != nil {
		fmt.Println(err)
		return nil
	}
	defer file.Close()

	fmt.Println(c.targetAddr)

	res, err := http.Post("http://" + c.targetAddr + "/Checkpoints?pid=" + strconv.Itoa(int(c.pid)), "binary/octet-stream", file)
	if err != nil {
		fmt.Println(err)
		return nil
	}
	defer res.Body.Close()

	return nil
}

func (c criuNotifier) PostResume() error {
	// TODO - we need to notify the caller that we are done with the restoration process
	
	
	return nil
}

func registerProcess(p Process) {
	exists, _ := process.PidExists(p.Pid)
	if !exists {
		fmt.Println("error: register request for non-existant process")
		return
	}

	Processes.Store(p.Pid, p)
	fmt.Println("registered process", p.Pid)
}

func doMigration(request StartMigrationRequest) {
	// step 1: check that we have a matching PID
	//  assumption: if the process exists it will continue to exist
	iprocess, exists := Processes.Load(request.Pid)
	if !exists {
		fmt.Println("error: migration request for non-registered process")
		return
	}

	process, ok := iprocess.(Process)
	if !ok {
		panic("error: pid not associated with a Process")
	}

	// step 2: initialize clocks and verify we're not already migrating
	iclock, loaded := MigrationClocks.LoadOrStore(request.Pid, &MigrationClock{})
	if loaded {
		// this means that we were already doing a migration
		fmt.Println("error: migration request for a process in-migration")
		return
	}

	clock, ok := iclock.(*MigrationClock)
	if !ok {
		panic("error: process not associated with *MigrationClock")
	}

	mutex := &sync.Mutex{}
	quitChan := make(chan bool)
	process.DoneChan = quitChan
	Processes.Store(request.Pid, process)

	// step 3: inform Destination that we are migrating the process
	if err := doInformDestination(request.Destination, process, clock); err != nil {
		fmt.Println(err)
		return
	}

	// step 4 a: shadow traffic
	go forwardProcessTraffic(process, request.Destination,
		                 clock, mutex, quitChan)

	// step 4 b: (i) checkpoint and (ii) send process
	// (i)
	checkpointer := criu.MakeCriu()
	leaveRunning := true
	shellJob := true
	outputDir := strconv.FormatInt(time.Now().Unix(), 10)
	orphanPtsMaster := true

	if err := os.Mkdir(outputDir, 0666); err != nil {
		fmt.Println(err)
		quitChan <- true
		return
	}

	file, err := os.Open(outputDir)
	if err != nil {
		fmt.Println(err)
		quitChan <- true
		return
	}
	defer file.Close()

	fd := int32(file.Fd())
	options := rpc.CriuOpts{
		LeaveRunning: &leaveRunning,
		ShellJob: &shellJob,
		Pid: &process.Pid,
		ImagesDirFd: &fd,
		External: []string{fmt.Sprintf("net[%d]:extRootNetNS", netnsInode(request.Pid))},
		OrphanPtsMaster: &orphanPtsMaster,
	}

	watcher := criuNotifier{
		imageDir: outputDir,
		targetAddr: request.Destination,
		pid: process.Pid,
	}

	if err := checkpointer.Dump(options, watcher); err != nil {
		fmt.Println(err)
		quitChan <- true
	}
}

func netnsInode(pid int32) uint64 {
	fstat := syscall.Stat_t{}

	if err := syscall.Stat("/proc/" + strconv.Itoa(int(pid)) + "/ns/net",
		&fstat); err != nil {
		panic(err)
	}

	return fstat.Ino 
}

func doInformDestination(target string, process Process, clock *MigrationClock) error {
	// increment the clock
	clock.SourceTime += 1

	// create + marshal request
	slaveMigrationRequest := SlaveStartMigrationMessage{*clock, process}
	jsonBytes, err := json.Marshal(slaveMigrationRequest)
	if err != nil {
		return errors.New("doInformDestination() unable to marhsal json ")
	}

	// send request
	_, err = http.Post("http://"+target+"/SlaveStartMigration", "application/json",
		bytes.NewBuffer(jsonBytes))
	if err != nil {
		return err
	}

	return nil
}

func forwardProcessTraffic(p Process, dst string,
	                   clck *MigrationClock, m *sync.Mutex,
	                   done chan bool) {
	// iface defined in main.go
	handle, err := pcap.OpenLive(iface, 1600, true, pcap.BlockForever)
	if err != nil {
		fmt.Println(err)
		return
	}

	// build the packet capture filter
	filterStr := ""
	for _, port := range p.TcpPorts {
		filterStr += " or tcp dst port " + strconv.FormatUint(uint64(port), 10)
	}

	for _, port := range p.UdpPorts {
		filterStr += " or udp dst port " + strconv.FormatUint(uint64(port), 10)
	}

	filterStr = strings.Trim(filterStr, " or")
	//fmt.Println("net filter:", filterStr)

	if err := handle.SetBPFFilter(filterStr); err != nil {
		fmt.Println(err)
		return
	}

	packetSrc := gopacket.NewPacketSource(handle, handle.LinkType())
	packetChan := packetSrc.Packets()

	for {
		select {
		case <-done:
			return

		case packet := <-packetChan:
			// update the clock
			m.Lock()
			clck.SourceTime += 1
			msgClock := *clck
			m.Unlock()

			//fmt.Println("HERE")
			
			// create the message
			msg := ShadowTrafficMessage{msgClock, packet.Data(), p.Pid}
			jsonBytes, err := json.Marshal(msg)
			if err != nil {
				fmt.Println(err)
				continue
			}

			// send the message
			_, err = http.Post("http://"+dst+"/ForwardTraffic",
				"application/json",
				bytes.NewBuffer(jsonBytes))
			if err != nil {
				fmt.Println(err)
				continue
			}

		}
	}
}

func restoreFromFile(path string) error {
	return execInNetNS("./restore.sh", []string{path})
}


func forwardTraffic(p int32) {
	iprocess, ok := Processes.Load(p)
	if !ok {
		fmt.Println("no process found with pid " + string(p))
		return
	}

	process, ok := iprocess.(Process)
	if !ok {
		panic("pid not associated with Process")
	}

	for packet := range process.Packets {
		decodedPacket := gopacket.NewPacket(packet, layers.LayerTypeEthernet, gopacket.Default)

		var ip net.IP
		if ipLayer := decodedPacket.Layer(layers.LayerTypeIPv4); ipLayer != nil {
			castedLayer, _ := ipLayer.(*layers.IPv4)
			ip = castedLayer.DstIP
		} else {
			return
		}

		var port uint16
		var payload []byte
		if udpLayer := decodedPacket.Layer(layers.LayerTypeUDP); udpLayer != nil {
			castedLayer, _ := udpLayer.(*layers.UDP)
			port = uint16(castedLayer.DstPort)
			payload = castedLayer.Payload
		} else {
			return
		}

		conn, err := net.Dial("udp",
			fmt.Sprintf("%s:%d", string(ip), port))
		if err != nil {
			fmt.Println("error sending packet")
			return
		}

		conn.Write(payload)
		conn.Close()
	}
}
