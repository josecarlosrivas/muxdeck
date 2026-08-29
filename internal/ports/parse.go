package ports

import (
	"strconv"
	"strings"
)

// The text formats this package joins on live here, untagged, so their tests
// run on every builder rather than only on the platform that produces them —
// a Linux CI job that never exercises the lsof parser is how a macOS-only
// regression ships.

// statPPID pulls the parent pid out of a /proc/<pid>/stat line.
//
// The second field is the executable name in parentheses and may itself
// contain spaces and parentheses (a process can rename itself to anything),
// so the scan resumes after the LAST ")" instead of splitting the whole line.
func statPPID(stat string) (int, bool) {
	i := strings.LastIndex(stat, ")")
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(stat[i+1:])
	if len(f) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(f[1])
	return ppid, err == nil
}

// socketInode reads the inode out of a /proc/<pid>/fd symlink target. Only
// sockets carry one; every other target is some other kind of file.
func socketInode(link string) (uint64, bool) {
	s, ok := strings.CutPrefix(link, "socket:[")
	if !ok {
		return 0, false
	}
	s, ok = strings.CutSuffix(s, "]")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 10, 64)
	return n, err == nil
}

// tcpListen is the st column value procfs uses for TCP_LISTEN.
const tcpListen = "0A"

// parseNetTCP collects inode -> port for the listening sockets in a
// /proc/net/tcp or /proc/net/tcp6 table, adding to into so both families
// share one map.
//
// Columns are sl, local_address, rem_address, st, … with the inode at index
// 9, and local_address is HEXADDR:HEXPORT. The header row falls out for free:
// its st column reads "st", which is not a listening state.
func parseNetTCP(table string, into map[uint64]int) {
	for _, line := range strings.Split(table, "\n") {
		f := strings.Fields(line)
		if len(f) < 10 || f[3] != tcpListen {
			continue
		}
		_, hexPort, ok := strings.Cut(f[1], ":")
		if !ok {
			continue
		}
		port, err := strconv.ParseUint(hexPort, 16, 32)
		if err != nil || port == 0 {
			continue
		}
		inode, err := strconv.ParseUint(f[9], 10, 64)
		if err != nil {
			continue
		}
		into[inode] = int(port)
	}
}

// parsePS reads pid -> ppid from `ps -axo pid=,ppid=` output.
func parsePS(out string) map[int]int {
	m := map[int]int{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		pid, errPID := strconv.Atoi(f[0])
		ppid, errPPID := strconv.Atoi(f[1])
		if errPID != nil || errPPID != nil {
			continue
		}
		m[pid] = ppid
	}
	return m
}

// parseLsof reads pid -> listening ports from `lsof -F` output, which emits
// one field per line tagged by its first byte — "p" opens a process block,
// "n" names a socket within it. The tagged form parses unambiguously; the
// human table does not, since a command name may contain spaces.
func parseLsof(out string) map[int][]int {
	byPID := map[int][]int{}
	pid := 0
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			n, err := strconv.Atoi(line[1:])
			if err != nil {
				// An unreadable process line would otherwise attribute the
				// sockets that follow it to the previous process.
				pid = 0
				continue
			}
			pid = n
		case 'n':
			if pid == 0 {
				continue
			}
			if port, ok := lsofPort(line[1:]); ok {
				byPID[pid] = append(byPID[pid], port)
			}
		}
	}
	if len(byPID) == 0 {
		return nil
	}
	return byPID
}

// lsofPort pulls the port out of an lsof name field: "*:3000",
// "127.0.0.1:8300", "[::1]:8300". "->" marks a connected socket rather than a
// listener, and the state filter is not to be trusted to have excluded it.
func lsofPort(name string) (int, bool) {
	if strings.Contains(name, "->") {
		return 0, false
	}
	i := strings.LastIndex(name, ":")
	if i < 0 {
		return 0, false
	}
	port, err := strconv.Atoi(name[i+1:])
	return port, err == nil && port > 0
}
