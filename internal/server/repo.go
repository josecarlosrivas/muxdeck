package server

import (
	"context"
	"strings"
	"sync"
	"time"
)

// repoState is the git half of a session's sidebar context: which branch the
// session's working directory sits on, and whether it has uncommitted work.
type repoState struct {
	Branch string `json:"branch,omitempty"`
	Dirty  bool   `json:"dirty,omitempty"`
}

// repoCache resolves repoState for session working directories.
//
// The session list is polled every couple of seconds and a status check on a
// large repository is not free, so two rules keep git off the hot path: a
// resolved state is reused for repoTTL, and a stale one is refreshed in the
// background while the poll returns the value already in hand. A chip that
// lags a few seconds behind the working tree is a fair trade for a list that
// never waits on git.
type repoCache struct {
	mu sync.Mutex
	m  map[string]repoEntry // session cwd -> entry
}

type repoEntry struct {
	state      repoState
	resolved   time.Time
	refreshing bool
}

const repoTTL = 5 * time.Second

// resolveRepo runs on a background goroutine whose completion is what clears
// the refreshing flag, so a git that never returns — a stalled network mount,
// a repository whose status walk does not finish — would strand that
// directory as permanently refreshing and freeze its chip for as long as a
// session sits there. Every call on this path is therefore bounded; a
// deadline yields the zero state and the entry retries on the next TTL.
const repoGitTimeout = 3 * time.Second

func newRepoCache() *repoCache { return &repoCache{m: map[string]repoEntry{}} }

// state returns what is known about cwd now, scheduling a refresh when that
// knowledge is missing or stale. A directory outside any repository resolves
// to the zero value and is cached like any other, so the miss costs one git
// call per TTL rather than one per poll.
func (c *repoCache) state(cwd string) repoState {
	if cwd == "" {
		return repoState{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[cwd]
	if (!ok || time.Since(e.resolved) > repoTTL) && !e.refreshing {
		e.refreshing = true
		c.m[cwd] = e
		go c.refresh(cwd)
	}
	return e.state
}

func (c *repoCache) refresh(cwd string) {
	state := resolveRepo(cwd)
	c.mu.Lock()
	c.m[cwd] = repoEntry{state: state, resolved: time.Now()}
	c.mu.Unlock()
}

// prune drops directories no live session is sitting in, so the map tracks
// the session list rather than growing with every directory ever visited.
func (c *repoCache) prune(live []string) {
	alive := make(map[string]bool, len(live))
	for _, cwd := range live {
		alive[cwd] = true
	}
	c.mu.Lock()
	for cwd := range c.m {
		if !alive[cwd] {
			delete(c.m, cwd)
		}
	}
	c.mu.Unlock()
}

func resolveRepo(cwd string) repoState {
	ctx, cancel := context.WithTimeout(context.Background(), repoGitTimeout)
	defer cancel()

	branch, err := gitContext(ctx, cwd, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		// Not a repository, no commits yet, or no git at all — all of which
		// mean the same thing to the sidebar: nothing to show.
		return repoState{}
	}
	state := repoState{Branch: strings.TrimSpace(branch)}
	if state.Branch == "HEAD" {
		// Detached: the short sha is the only name the head still has.
		if sha, err := gitContext(ctx, cwd, "rev-parse", "--short", "HEAD"); err == nil {
			state.Branch = strings.TrimSpace(sha)
		}
	}
	// --no-optional-locks so polling never contends with the git the user is
	// running in that same directory.
	if out, err := gitContext(ctx, cwd, "--no-optional-locks", "status", "--porcelain"); err == nil {
		state.Dirty = strings.TrimSpace(out) != ""
	}
	return state
}
