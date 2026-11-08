package daemon

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/haasele/prox/internal/policy"
)

// listeningPorts reads /proc/<pid>/net/tcp{,6} from the container's network
// namespace to find LISTEN sockets. This is the last-resort target detection
// step before giving up.
func listeningPorts(pid int) []int {
	if pid <= 0 {
		return nil
	}
	seen := map[int]bool{}
	var out []int
	for _, name := range []string{"tcp", "tcp6"} {
		f, err := os.Open(fmt.Sprintf("/proc/%d/net/%s", pid, name))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		first := true
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if first {
				first = false
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 4 || fields[3] != "0A" {
				continue
			}
			local := fields[1]
			i := strings.LastIndex(local, ":")
			if i < 0 {
				continue
			}
			port64, err := strconv.ParseInt(local[i+1:], 16, 32)
			if err != nil || port64 <= 0 || port64 > 65535 {
				continue
			}
			p := int(port64)
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
		f.Close()
	}
	return out
}

func detectProbePort(proto string, pid int) int {
	ports := listeningPorts(pid)
	if len(ports) == 0 {
		return 0
	}
	if proto == "http" {
		return policy.DetectHTTPPort(ports)
	}
	lowest := ports[0]
	for _, p := range ports {
		if p < lowest {
			lowest = p
		}
	}
	return lowest
}
