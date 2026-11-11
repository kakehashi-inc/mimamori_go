package modules

import (
	"os"
	"sync"
	"time"
)

// SystemLocation returns the system time zone read from /etc/localtime
// (UTC when it is missing). The TZ environment variable is not used.
var SystemLocation = sync.OnceValue(func() *time.Location {
	data, err := os.ReadFile("/etc/localtime")
	if err != nil {
		return time.UTC
	}
	loc, err := time.LoadLocationFromTZData("Local", data)
	if err != nil {
		return time.UTC
	}
	return loc
})

// UseSystemLocation makes local times (log lines, displayed times) use the
// system time zone regardless of the environment.
func UseSystemLocation() {
	time.Local = SystemLocation()
}
