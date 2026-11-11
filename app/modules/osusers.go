package modules

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// PasswdEntry is one line of /etc/passwd.
type PasswdEntry struct {
	Name string
	UID  int
	GID  int
}

// passwdFile and groupFile can be replaced by tests.
var (
	passwdFile = "/etc/passwd"
	groupFile  = "/etc/group"
)

// ReadPasswd parses /etc/passwd into name and uid maps. It reads the file
// directly instead of using os/user so the lookup never depends on cgo/NSS.
func ReadPasswd() (byName map[string]PasswdEntry, byUID map[int]string) {
	byName = map[string]PasswdEntry{}
	byUID = map[int]string{}
	f, err := os.Open(passwdFile)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) < 4 {
			continue
		}
		uid, err1 := strconv.Atoi(parts[2])
		gid, err2 := strconv.Atoi(parts[3])
		if err1 != nil || err2 != nil {
			continue
		}
		e := PasswdEntry{Name: parts[0], UID: uid, GID: gid}
		byName[e.Name] = e
		if _, ok := byUID[uid]; !ok {
			byUID[uid] = e.Name
		}
	}
	return
}

// ReadGroups parses /etc/group into a gid to name map.
func ReadGroups() map[int]string {
	out := map[int]string{}
	f, err := os.Open(groupFile)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Split(sc.Text(), ":")
		if len(parts) < 3 {
			continue
		}
		if gid, err := strconv.Atoi(parts[2]); err == nil {
			if _, ok := out[gid]; !ok {
				out[gid] = parts[0]
			}
		}
	}
	return out
}

// NameOrID returns the user name for uid, or the number as text.
func NameOrID(m map[int]string, id int) string {
	if n, ok := m[id]; ok {
		return n
	}
	return strconv.Itoa(id)
}
