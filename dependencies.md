go: github.com/shirou/gopsutil/process
go: github.com/google/gopacket/pcap
go: github.com/checkpoint-restore/go-criu
go: github.com/vishvananda/netlink
go: github.com/vishvananda/netns
go: github.com/coreos/go-iptables/iptables
go: github.com/mholt/archiver
go: github.com/docker/docker/pkg/mount
go: github.com/google/gopacket/layers
system: libpcap-dev
system: criu

# NOTE
Because systemd is a ~piece of shit~ *differently abled*, you must edit /etc/resolv.conf to avoid using the 127.0.0.53 nameserver (e.g. instead try 8.8.8.8). We may edit the software to specifically handle that rule, but don't count on it any time soon.
