package deploy

import (
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"
)

// portAllocator hands out unique host ports for app containers. It tracks
// assignments in memory and skips any port a process is already listening on
// (a running container), which a plain bind check can miss because Go's
// net.Listen sets SO_REUSEADDR.
type portAllocator struct {
	mu    sync.Mutex
	start int
	end   int
	next  int
	used  map[int]bool
}

func newPortAllocator(start, end int) *portAllocator {
	return &portAllocator{start: start, end: end, next: start, used: map[int]bool{}}
}

func (a *portAllocator) allocate() (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	span := a.end - a.start + 1
	for i := 0; i < span; i++ {
		p := a.next
		a.next++
		if a.next > a.end {
			a.next = a.start
		}
		if a.used[p] {
			continue
		}
		if portListening(p) {
			a.used[p] = true // a container already holds it; remember to skip
			continue
		}
		a.used[p] = true
		return p, nil
	}
	return 0, fmt.Errorf("no free host ports in %d-%d", a.start, a.end)
}

func (a *portAllocator) release(p int) {
	if p == 0 {
		return
	}
	a.mu.Lock()
	delete(a.used, p)
	a.mu.Unlock()
}

// portListening reports whether something is accepting connections on the port.
func portListening(port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// waitListening polls a local TCP port until something accepts, or times out.
func waitListening(port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true
		}
		time.Sleep(150 * time.Millisecond) // poll fast so ready apps go live instantly
	}
	return false
}
