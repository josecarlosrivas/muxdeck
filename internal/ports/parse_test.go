package ports

import (
	"reflect"
	"testing"
)

func TestStatPPID(t *testing.T) {
	for name, tc := range map[string]struct {
		stat string
		want int
		ok   bool
	}{
		"ordinary":            {"4242 (node) S 4200 4200 4200 0 -1 4194304 0", 4200, true},
		"spaces in comm":      {"7 (my server) R 3 3 3", 3, true},
		"parens in comm":      {"9 (weird (name)) S 5 5 5", 5, true},
		"separator in comm":   {"9 () ) S 5 5", 5, true},
		"no closing paren":    {"4242 (node S 4200 4200", 0, false},
		"truncated after nme": {"4242 (node) S", 0, false},
		"non-numeric ppid":    {"4242 (node) S x 1", 0, false},
		"empty":               {"", 0, false},
	} {
		got, ok := statPPID(tc.stat)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: got (%d, %v), want (%d, %v)", name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestSocketInode(t *testing.T) {
	for link, want := range map[string]uint64{
		"socket:[12345]": 12345,
		"socket:[0]":     0,
	} {
		got, ok := socketInode(link)
		if !ok || got != want {
			t.Errorf("%q: got (%d, %v), want (%d, true)", link, got, ok, want)
		}
	}
	for _, link := range []string{
		"/dev/pts/4", "pipe:[12345]", "anon_inode:[eventpoll]",
		"socket:[12345", "socket:[]", "socket:[-1]", "socket:[abc]", "",
	} {
		if _, ok := socketInode(link); ok {
			t.Errorf("%q: parsed as a socket inode, want rejected", link)
		}
	}
}

// The header row and every non-listening state must fall out, or a session
// would advertise ports belonging to its outbound connections.
func TestParseNetTCP(t *testing.T) {
	got := map[uint64]int{}
	parseNetTCP(`  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 41001 1 0000 100 0
   1: 0100007F:1F41 0100007F:B3A2 01 00000000:00000000 00:00000000 00000000  1000        0 41002 1 0000 100 0
   2: 0100007F:2076 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 41003 1 0000 100 0
`, got)
	parseNetTCP(`  sl  local_address                         rem_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:0BB8 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 41004 1 0000 100 0
`, got)

	want := map[uint64]int{41001: 8080, 41003: 8310, 41004: 3000}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseNetTCPRejectsMalformed(t *testing.T) {
	got := map[uint64]int{}
	parseNetTCP(`   0: 00000000:1F90 00000000:0000 0A 0 0 0 0 0
   1: 00000000 00000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 41010 1
   2: 00000000:ZZZZ 00000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 41011 1
   3: 00000000:0000 00000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 41012 1
   4: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 notanum 1
`, got)
	if len(got) != 0 {
		t.Errorf("got %v, want nothing parsed", got)
	}
}

func TestParsePS(t *testing.T) {
	got := parsePS(`  501     1
  4200   501
  4242  4200
 garbage
  4243
`)
	want := map[int]int{501: 1, 4200: 501, 4242: 4200}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseLsof(t *testing.T) {
	got := parseLsof(`p4242
n*:3000
n127.0.0.1:3000
p4300
n[::1]:8300
nlocalhost:50322->localhost:8300
`)
	want := map[int][]int{4242: {3000, 3000}, 4300: {8300}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// A process line that does not parse must orphan the sockets under it rather
// than hand them to the process above.
func TestParseLsofOrphansAfterBadProcessLine(t *testing.T) {
	got := parseLsof(`p4242
n*:3000
pnotanumber
n*:9999
`)
	want := map[int][]int{4242: {3000}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := parseLsof("f12\nn*:3000\n"); got != nil {
		t.Errorf("sockets before any process line: got %v, want nil", got)
	}
}

func TestLsofPort(t *testing.T) {
	for name, want := range map[string]int{
		"*:3000": 3000, "127.0.0.1:8300": 8300, "[::1]:8300": 8300, "*:1": 1,
	} {
		got, ok := lsofPort(name)
		if !ok || got != want {
			t.Errorf("%q: got (%d, %v), want (%d, true)", name, got, ok, want)
		}
	}
	for _, name := range []string{
		"localhost:50322->localhost:8300", "*:*", "3000", "", "*:0", "*:-1",
	} {
		if port, ok := lsofPort(name); ok {
			t.Errorf("%q: got port %d, want rejected", name, port)
		}
	}
}
