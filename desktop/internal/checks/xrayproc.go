package checks

// xray-core as a child process (both cores), the desktop counterpart of
// xraylib.Start/Stop and XrayNext.kt. The config goes in over stdin
// ("run -config stdin:", same syntax in 26.3.27 and 26.10.31) and never
// touches the disk.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type xrayProc struct {
	cmd     *exec.Cmd
	done    chan struct{}
	waitErr error
	mu      sync.Mutex
	tail    []string
	kill    func(alive bool)
	logWg   sync.WaitGroup
}

// xrayEnv: the parent's environment minus anything that would make xray
// load other config files next to ours.
func xrayEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k := strings.ToUpper(strings.SplitN(kv, "=", 2)[0])
		if strings.HasPrefix(k, "XRAY_LOCATION_CONF") || strings.HasPrefix(k, "XRAY.LOCATION.CONF") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

var xrayLogPrefix = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}(\.\d+)? `)

func startXray(bin string, config []byte, logf func(string)) (*xrayProc, error) {
	if bin == "" {
		return nil, errors.New("xray binary not configured")
	}
	if _, err := os.Stat(bin); err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, "run", "-config", "stdin:")
	cmd.Stdin = bytes.NewReader(config)
	cmd.Env = xrayEnv()
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	cmd.WaitDelay = 3 * time.Second
	setProcAttrs(cmd)
	p := &xrayProc{cmd: cmd, done: make(chan struct{})}
	if err := cmd.Start(); err != nil {
		pw.Close()
		return nil, err
	}
	p.kill = attachKill(cmd)
	p.logWg.Add(1)
	go func() {
		defer p.logWg.Done()
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		n := 0
		for sc.Scan() {
			line := strings.TrimSpace(xrayLogPrefix.ReplaceAllString(sc.Text(), ""))
			if line == "" {
				continue
			}
			p.mu.Lock()
			p.tail = append(p.tail, truncRunes(line, 300))
			if len(p.tail) > 5 {
				p.tail = p.tail[1:]
			}
			p.mu.Unlock()
			if n++; n <= 200 && logf != nil {
				logf("[xray] " + truncRunes(line, 300))
			}
		}
		io.Copy(io.Discard, pr)
	}()
	go func() {
		p.waitErr = cmd.Wait()
		pw.Close()
		close(p.done)
	}()
	return p, nil
}

func (p *xrayProc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *xrayProc) exitCode() int {
	if p.cmd.ProcessState != nil {
		return p.cmd.ProcessState.ExitCode()
	}
	return -1
}

func (p *xrayProc) lastLine() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.tail) == 0 {
		return ""
	}
	return p.tail[len(p.tail)-1]
}

// stop kills the core (whole process group / job object) and waits.
func (p *xrayProc) stop() {
	if p == nil {
		return
	}
	p.kill(!p.exited())
	select {
	case <-p.done:
		p.logWg.Wait()
	case <-time.After(5 * time.Second):
	}
}

// waitReady: the inbound answers on 127.0.0.1:port (the loopback hop is
// not a check socket, so a plain dialer). Returns early if xray exits.
func (p *xrayProc) waitReady(ctx context.Context, port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if p.exited() {
			return false
		}
		c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err == nil {
			c.Close()
			return true
		}
		select {
		case <-p.done:
			return false
		case <-ctx.Done():
			return false
		case <-time.After(150 * time.Millisecond):
		}
	}
	return false
}

// freeLocalPort: a port nothing listens on right now (Android picked a
// random one in 20000..28999; a free one is the safer equivalent).
func freeLocalPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

var versionCache sync.Map

var xrayVersionRe = regexp.MustCompile(`Xray\s+v?(\d+\.\d+\.\d+)`)

// xrayVersion = XrayNext.version: "26.10.31" read from the binary once.
func xrayVersion(bin string) string {
	if bin == "" {
		return ""
	}
	if v, ok := versionCache.Load(bin); ok {
		return v.(string)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "version")
	cmd.Env = xrayEnv()
	setProcAttrs(cmd)
	out, _ := cmd.CombinedOutput()
	v := ""
	if m := xrayVersionRe.FindSubmatch(out); m != nil {
		v = string(m[1])
	}
	if v != "" {
		versionCache.Store(bin, v)
	}
	return v
}

func xrayVersionsImpl(env Env) (string, string) {
	return xrayVersion(env.XrayPath), xrayVersion(env.XrayNextPath)
}

func truncRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
