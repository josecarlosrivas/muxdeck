//go:build linux

package ports

import (
	"os"
	"path/filepath"
	"strconv"
)

// parents reads the pid -> ppid edge of every live process from procfs.
func parents() (map[int]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	out := make(map[int]int, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // /proc's non-numeric entries are not processes
		}
		stat, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue // exited between the listing and the read
		}
		if ppid, ok := statPPID(string(stat)); ok {
			out[pid] = ppid
		}
	}
	return out, nil
}

// listening returns the TCP ports each of pids holds a listening socket on.
//
// procfs splits that fact in two: /proc/net/tcp knows every socket's state
// and port but not who holds it, and /proc/<pid>/fd knows which sockets a
// process holds but not what they are. The inode is the join key. Reading the
// tables first means the per-process walk only has to recognise inodes, and
// an empty listening set skips the walk entirely.
func listening(pids []int) (map[int][]int, error) {
	byInode := map[uint64]int{}
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(table)
		if err != nil {
			continue // tcp6 is simply absent when IPv6 is off
		}
		parseNetTCP(string(data), byInode)
	}
	if len(byInode) == 0 {
		return nil, nil
	}
	out := map[int][]int{}
	for _, pid := range pids {
		dir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
		fds, err := os.ReadDir(dir)
		if err != nil {
			continue // exited, or running as another user
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(dir, fd.Name()))
			if err != nil {
				continue
			}
			inode, ok := socketInode(link)
			if !ok {
				continue
			}
			if port, ok := byInode[inode]; ok {
				out[pid] = append(out[pid], port)
			}
		}
	}
	return out, nil
}
