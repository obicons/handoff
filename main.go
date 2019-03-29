package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
)

var (
	iface string
)

func main() {
	ifacePtr := flag.String("iface", "", "public-facing network interface")
	port := flag.Int("port", 8080, "port to listen on")
	bridgeNetPtr := flag.String("network-cidr", "172.31.0.0/24",
		"CIDR block of virtual net ")

	flag.Parse()

	if *ifacePtr == "" {
		fmt.Println("error: no iface provided")
		os.Exit(1)
	}

	if 0 > *port || 65535 < *port {
		fmt.Println("error: invalid port provided")
		os.Exit(1)
	}

	iface = *ifacePtr

	// may as well add this check, since we need to be root to run
	if os.Geteuid() != 0 {
		fmt.Println("error: must be invoked as root")
		os.Exit(1)
	}

	// make sure the bridge exists
	err := verifyBridgePresence(*bridgeNetPtr)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	if err = execInNetNS("nc", []string{"-lk", "0.0.0.0", "5000"}); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	http.HandleFunc("/StartMigration", StartMigrationHandler)
	http.HandleFunc("/RegisterProcess", RegisterProcessHandler)
	http.HandleFunc("/ForwardTraffic", ForwardTrafficHandler)
	http.HandleFunc("/SlaveStartMigration", SlaveStartMigrationHandler)
	http.HandleFunc("/Checkpoints", ReceiveCheckpointHandler)
	http.ListenAndServe(":"+strconv.Itoa(*port), nil)
}
