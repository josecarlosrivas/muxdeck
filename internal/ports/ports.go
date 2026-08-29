// Package ports reports which TCP ports a tmux session's processes are
// listening on, so a row in the sidebar can say what a session is serving
// without the operator going hunting for it.
package ports

import "sort"

// maxPerSession caps what one session reports. A process tree holding dozens
// of listeners has nothing more to tell a sidebar chip than its first few,
// and the cap keeps a runaway tree from bloating every poll response.
const maxPerSession = 8

// Listening maps session name -> sorted listening TCP ports.
//
// panes gives the pane process of every session — the roots of the process
// trees that session owns. Everything descended from those roots counts as
// the session's, which is what makes `npm run dev` under a shell under a pane
// attributable at all.
//
// Any failure to read the process table yields no ports rather than an error:
// this decorates a list that must keep working on a machine where /proc is
// masked, lsof is absent, or the platform is neither Linux nor macOS.
func Listening(panes map[string][]int) map[string][]int {
	if len(panes) == 0 {
		return nil
	}
	parent, err := parents()
	if err != nil {
		return nil
	}
	owner := attribute(panes, parent)
	if len(owner) == 0 {
		return nil
	}
	pids := make([]int, 0, len(owner))
	for pid := range owner {
		pids = append(pids, pid)
	}
	byPID, err := listening(pids)
	if err != nil {
		return nil
	}
	return assemble(owner, byPID)
}

// attribute walks down from each session's pane processes and claims every
// descendant for that session.
//
// Down from the panes rather than up from the listeners because the platform
// half is asked for a pid set: on Linux the join between a socket and the
// process holding it costs one directory walk per process, so the set has to
// be the session's few dozen descendants and not every process on the box.
//
// Sessions are visited in name order and a pid keeps its first claim, so a
// process table caught mid-reparent resolves the same way twice rather than
// making a chip flicker between two rows. Marking before descending also
// bounds the walk if a torn read of the table produces a cycle.
func attribute(panes map[string][]int, parent map[int]int) map[int]string {
	children := make(map[int][]int, len(parent))
	for pid, ppid := range parent {
		children[ppid] = append(children[ppid], pid)
	}
	names := make([]string, 0, len(panes))
	for name := range panes {
		names = append(names, name)
	}
	sort.Strings(names)

	owner := map[int]string{}
	for _, name := range names {
		queue := append([]int(nil), panes[name]...)
		for len(queue) > 0 {
			pid := queue[0]
			queue = queue[1:]
			if _, claimed := owner[pid]; claimed {
				continue
			}
			owner[pid] = name
			queue = append(queue, children[pid]...)
		}
	}
	return owner
}

// assemble folds per-process ports up into per-session ports, deduplicated
// (a pre-forking server holds one listening socket open in every worker) and
// ordered so a chip does not reshuffle between polls.
func assemble(owner map[int]string, byPID map[int][]int) map[string][]int {
	seen := map[string]map[int]bool{}
	out := map[string][]int{}
	for pid, ports := range byPID {
		name, ok := owner[pid]
		if !ok {
			continue
		}
		if seen[name] == nil {
			seen[name] = map[int]bool{}
		}
		for _, port := range ports {
			if seen[name][port] {
				continue
			}
			seen[name][port] = true
			out[name] = append(out[name], port)
		}
	}
	for name, ports := range out {
		sort.Ints(ports)
		if len(ports) > maxPerSession {
			ports = ports[:maxPerSession]
		}
		out[name] = ports
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
