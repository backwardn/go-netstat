package netstat

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
)

const (
	pathTCPTab = "/proc/net/tcp"
	pathUDPTab = "/proc/net/udp"

	ipv4StrLen = 8
	ipv6StrLen = 32
)

// SockAddr represents an ip:port pair
type SockAddr struct {
	IP   net.IP
	Port uint16
}

func (s *SockAddr) String() string {
	return fmt.Sprintf("%v:%d", s.IP, s.Port)
}

// SockTabEntry type represents each line of the /proc/net/[tcp|udp]
type SockTabEntry struct {
	ino        string
	LocalAddr  *SockAddr
	RemoteAddr *SockAddr
	State      SkState
	UID        uint32
	Process    *Process
}

// Process holds the PID and process name to which each socket belongs
type Process struct {
	pid  int
	name string
}

func (p *Process) String() string {
	return fmt.Sprintf("%d/%s", p.pid, p.name)
}

// SkState type represents socket connection state
type SkState uint8

func (s SkState) String() string {
	return skStates[s-1].s
}

var skStates = [...]struct {
	st uint8
	s  string
}{
	{0x01, "ESTABLISHED"},
	{0x02, "SYN_SENT"},
	{0x03, "SYN_RECV"},
	{0x04, "FIN_WAIT1"},
	{0x05, "FIN_WAIT2"},
	{0x06, "TIME_WAIT"},
	{0x07, ""}, // CLOSE
	{0x08, "CLOSE_WAIT"},
	{0x09, "LAST_ACK"},
	{0x0A, "LISTEN"},
	{0x0B, "CLOSING"},
}

// Errors returned by gonetstat
var (
	ErrNotEnoughFields = errors.New("gonetstat: not enough fields in the line")
)

func parseAddr(s string) (*SockAddr, error) {
	fields := strings.Split(s, ":")
	if len(fields) < 2 {
		return nil, fmt.Errorf("netstat: not enough fields: %v", s)
	}
	v, err := strconv.ParseUint(fields[0], 16, 32)
	if err != nil {
		return nil, err
	}
	ip := make(net.IP, net.IPv4len)
	binary.LittleEndian.PutUint32(ip[:], uint32(v))
	v, err = strconv.ParseUint(fields[1], 16, 16)
	if err != nil {
		return nil, err
	}
	return &SockAddr{IP: ip, Port: uint16(v)}, nil
}

func parseSocktab(r io.Reader) ([]SockTabEntry, error) {
	br := bufio.NewScanner(r)
	tab := make([]SockTabEntry, 0, 4)

	// Discard title
	if br.Scan() {
		_ = br.Text()
	}

	for br.Scan() {
		var e SockTabEntry
		line := br.Text()
		// Skip comments
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) < 12 {
			return nil, fmt.Errorf("netstat: not enough fields: %v, %v", len(fields), fields)
		}
		addr, err := parseAddr(fields[1])
		if err != nil {
			return nil, err
		}
		e.LocalAddr = addr
		addr, err = parseAddr(fields[2])
		if err != nil {
			return nil, err
		}
		e.RemoteAddr = addr
		u, err := strconv.ParseUint(fields[3], 16, 8)
		if err != nil {
			return nil, err
		}
		e.State = SkState(u)
		u, err = strconv.ParseUint(fields[7], 10, 32)
		if err != nil {
			return nil, err
		}
		e.UID = uint32(u)
		e.ino = fields[9]
		tab = append(tab, e)
	}
	return tab, br.Err()
}

type procFd struct {
	base  string
	pid   int
	sktab []SockTabEntry
	p     *Process
}

const sockPrefix = "socket:["

func getProcName(s []byte) string {
	i := bytes.Index(s, []byte("("))
	if i < 0 {
		return ""
	}
	j := bytes.LastIndex(s, []byte(")"))
	if i < 0 {
		return ""
	}
	if i > j {
		return ""
	}
	return string(s[i+1 : j])
}

func (p *procFd) iterFdDir() {
	// link name is of the form socket:[5860846]
	fddir := path.Join(p.base, "/fd")
	fi, err := ioutil.ReadDir(fddir)
	if err != nil {
		return
	}
	var buf [128]byte

	for _, file := range fi {
		fd := path.Join(fddir, file.Name())
		lname, err := os.Readlink(fd)
		if err != nil {
			continue
		}

		for i := range p.sktab {
			sk := &p.sktab[i]
			ss := sockPrefix + sk.ino + "]"
			if ss != lname {
				continue
			}
			if p.p == nil {
				stat, err := os.Open(path.Join(p.base, "stat"))
				if err != nil {
					return
				}
				n, err := stat.Read(buf[:])
				stat.Close()
				if err != nil {
					return
				}
				z := bytes.SplitN(buf[:n], []byte(" "), 3)
				name := getProcName(z[1])
				p.p = &Process{p.pid, name}
			}
			sk.Process = p.p
		}
	}
}

func extractProcInfo(sktab []SockTabEntry) {
	const basedir = "/proc"
	fi, err := ioutil.ReadDir(basedir)
	if err != nil {
		log.Fatal(err)
	}

	for _, file := range fi {
		if !file.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(file.Name())
		if err != nil {
			continue
		}
		base := path.Join(basedir, file.Name())
		proc := procFd{base: base, pid: pid, sktab: sktab}
		proc.iterFdDir()
	}
}

// NetStat - collect information about network port status
func NetStat() error {
	// to change the flags on the default logger
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	f, err := os.Open(pathTCPTab)
	if err != nil {
		return err
	}
	tabs, err := parseSocktab(f)
	if err != nil {
		return err
	}
	extractProcInfo(tabs)
	for _, t := range tabs {
		fmt.Println(t)
	}
	return nil
}

func doNetstat(path string) ([]SockTabEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	tabs, err := parseSocktab(f)
	f.Close()
	if err != nil {
		return nil, err
	}
	extractProcInfo(tabs)
	return tabs, nil
}

// TCPSocks returns active TCP sockets
func TCPSocks() ([]SockTabEntry, error) {
	return doNetstat(pathTCPTab)
}

// UDPSocks returns active UDP sockets
func UDPSocks() ([]SockTabEntry, error) {
	return doNetstat(pathUDPTab)
}
