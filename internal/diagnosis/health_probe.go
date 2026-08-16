// This file implements Phase A5 of LEVEE's automated diagnosis subsystem:
// the health prober. It runs four families of probes — network connectivity,
// node resources, service status and data consistency — concurrently against
// a single target and aggregates them into one HealthReport.
//
// Network probes (ping, DNS, TCP dial) use the net standard library directly.
// Node, service and data probes execute remote shell commands through the
// CommandExecutor interface defined in log_collector.go, keeping the prober
// transport-agnostic. Linux targets use top/free/df/procfs; Windows targets
// use wmic.
//
// The prober is safe for concurrent use: ProbeAll (and each individual probe)
// may be called from multiple goroutines simultaneously. All probes respect
// the caller's context deadline. The package never panics; failures are
// reported through the Status field of the returned report.
package diagnosis

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nexus/levee/internal/log"
)

// --- Health status ---------------------------------------------------------

// HealthStatus enumerates the possible outcomes of a probe.
type HealthStatus string

const (
	// StatusHealthy means the probe succeeded with no warnings.
	StatusHealthy HealthStatus = "healthy"
	// StatusDegraded means the probe succeeded with warnings (e.g. high
	// resource usage, slow replication). The target is usable but needs
	// attention.
	StatusDegraded HealthStatus = "degraded"
	// StatusUnhealthy means the probe failed. The target should not receive
	// traffic until the issue is resolved.
	StatusUnhealthy HealthStatus = "unhealthy"
	// StatusUnknown means the probe could not run or could not interpret the
	// result. It is treated as a failure by the orchestrator.
	StatusUnknown HealthStatus = "unknown"
)

// --- Report types ----------------------------------------------------------

// HealthReport is the aggregated outcome of ProbeAll. It always contains all
// four sub-reports; a sub-report whose probe failed has Status == StatusUnknown
// and a non-empty Err field.
type HealthReport struct {
	Target   string        `json:"target"`
	Status   HealthStatus  `json:"status"`
	Network  NetworkHealth `json:"network"`
	Node     NodeHealth    `json:"node"`
	Service  ServiceHealth `json:"service"`
	Data     DataHealth    `json:"data"`
	Summary  string        `json:"summary"`
	ProbedAt time.Time     `json:"probed_at"`
	Duration time.Duration `json:"duration_ms"`
}

// NetworkHealth describes network connectivity to the target.
type NetworkHealth struct {
	Status HealthStatus `json:"status"`
	Ping   PingResult   `json:"ping"`
	DNS    DNSResult    `json:"dns"`
	TCP    []TCPResult  `json:"tcp"`
	Err    string       `json:"err,omitempty"`
}

// PingResult is the outcome of a TCP-based reachability probe. A TCP dial to
// PingPort is used as a ping surrogate because raw ICMP sockets require
// elevated privileges on most operating systems.
type PingResult struct {
	Reachable bool          `json:"reachable"`
	RTT       time.Duration `json:"rtt_ms"`
	Err       string        `json:"err,omitempty"`
}

// DNSResult is the outcome of a forward DNS lookup.
type DNSResult struct {
	Resolved bool     `json:"resolved"`
	Addrs    []string `json:"addrs,omitempty"`
	Err      string   `json:"err,omitempty"`
}

// TCPResult is the outcome of a single TCP dial probe.
type TCPResult struct {
	Port    int           `json:"port"`
	Open    bool          `json:"open"`
	Latency time.Duration `json:"latency_ms"`
	Err     string        `json:"err,omitempty"`
}

// NodeHealth describes the target's resource utilization.
type NodeHealth struct {
	Status HealthStatus  `json:"status"`
	CPU    CPUInfo       `json:"cpu"`
	Memory MemoryInfo    `json:"memory"`
	Disks  []DiskInfo    `json:"disks"`
	Load   LoadInfo      `json:"load"`
	Uptime time.Duration `json:"uptime_ms"`
	Err    string        `json:"err,omitempty"`
}

// CPUInfo holds CPU utilization metrics.
type CPUInfo struct {
	UsagePercent float64 `json:"usage_percent"`
	Cores        int     `json:"cores"`
}

// MemoryInfo holds memory utilization metrics in bytes.
type MemoryInfo struct {
	TotalBytes   uint64  `json:"total_bytes"`
	UsedBytes    uint64  `json:"used_bytes"`
	FreeBytes    uint64  `json:"free_bytes"`
	UsagePercent float64 `json:"usage_percent"`
}

// DiskInfo holds disk utilization for a single filesystem.
type DiskInfo struct {
	Filesystem   string  `json:"filesystem"`
	Mount        string  `json:"mount"`
	TotalBytes   uint64  `json:"total_bytes"`
	UsedBytes    uint64  `json:"used_bytes"`
	FreeBytes    uint64  `json:"free_bytes"`
	UsagePercent float64 `json:"usage_percent"`
}

// LoadInfo holds the 1/5/15-minute load averages.
type LoadInfo struct {
	Load1  float64 `json:"load1"`
	Load5  float64 `json:"load5"`
	Load15 float64 `json:"load15"`
}

// ServiceHealth describes the status of named services on the target.
type ServiceHealth struct {
	Status   HealthStatus  `json:"status"`
	Services []ServiceInfo `json:"services"`
	Err      string        `json:"err,omitempty"`
}

// ServiceSpec describes a service to probe.
type ServiceSpec struct {
	// Name is the process or service name to check. On Linux it is passed to
	// pgrep -x; on Windows it is passed to tasklist /FI "IMAGENAME eq Name".
	Name string `json:"name"`
	// Port is the TCP port to dial and the port used for the HTTP /healthz
	// probe. Zero skips both port and HTTP probes.
	Port int `json:"port,omitempty"`
}

// ServiceInfo is the per-service probe result.
type ServiceInfo struct {
	Name    string       `json:"name"`
	Status  HealthStatus `json:"status"`
	Running bool         `json:"running"`
	PID     int          `json:"pid,omitempty"`
	Port    int          `json:"port,omitempty"`
	HTTP    HTTPProbe    `json:"http"`
	Message string       `json:"message,omitempty"`
}

// HTTPProbe is the outcome of a GET /healthz request.
type HTTPProbe struct {
	URL    string        `json:"url,omitempty"`
	Status HealthStatus  `json:"status"`
	Code   int           `json:"code,omitempty"`
	RTT    time.Duration `json:"rtt_ms"`
	Err    string        `json:"err,omitempty"`
}

// DataHealth describes data-layer consistency.
type DataHealth struct {
	Status         HealthStatus    `json:"status"`
	DBConnection   DBConnInfo      `json:"db_connection"`
	ReplicationLag ReplicationInfo `json:"replication_lag"`
	Disks          []DiskInfo      `json:"disks"`
	Err            string          `json:"err,omitempty"`
}

// DBConnInfo is the database connectivity probe result.
type DBConnInfo struct {
	Connected bool          `json:"connected"`
	Latency   time.Duration `json:"latency_ms"`
	Err       string        `json:"err,omitempty"`
}

// ReplicationInfo is the replication lag probe result. LagSeconds is -1 when
// replication is not running (Seconds_Behind_Master is NULL).
type ReplicationInfo struct {
	LagSeconds float64 `json:"lag_seconds"`
	Healthy    bool    `json:"healthy"`
	Err        string  `json:"err,omitempty"`
}

// --- HealthProber ----------------------------------------------------------

// HealthProberConfig configures a HealthProber. All fields are optional; zero
// values are replaced with sensible defaults by NewHealthProber.
type HealthProberConfig struct {
	// Executor runs remote commands for node/service/data probes. It may be
	// nil, in which case those probes return StatusUnknown. Network probes
	// do not need an executor.
	Executor CommandExecutor

	// Runtime is the target OS family: RuntimeLinux or RuntimeWindows.
	// Defaults to RuntimeLinux when empty.
	Runtime Runtime

	// PingPort is the TCP port used for the ping reachability probe. A
	// negative value skips the ping probe. Zero defaults to 80.
	PingPort int

	// DialTimeout is the per-attempt timeout for net.DialTimeout probes.
	// Defaults to 3s.
	DialTimeout time.Duration

	// HTTPTimeout is the timeout for HTTP /healthz probes. Defaults to 5s.
	HTTPTimeout time.Duration

	// CommandTimeout is the per-command timeout applied when the caller's
	// ctx has no deadline. Defaults to 10s.
	CommandTimeout time.Duration

	// HTTPClient is the client used for /healthz probes. If nil a default
	// client with HTTPTimeout is used. Exposed so tests can inject a stub.
	HTTPClient *http.Client

	// DefaultPorts are the TCP ports probed by ProbeAll. ProbeNetwork callers
	// pass their own port list.
	DefaultPorts []int

	// DefaultServices are the services probed by ProbeAll. ProbeService
	// callers pass their own service list.
	DefaultServices []ServiceSpec

	// DBCheckCommand is the command run to verify database connectivity. A
	// zero exit code means connected. Defaults to a MySQL SELECT 1.
	DBCheckCommand string

	// ReplicationCommand is the command run to measure replication lag. Its
	// stdout is parsed for Seconds_Behind_Master. Defaults to MySQL SHOW
	// SLAVE STATUS.
	ReplicationCommand string

	// Thresholds for degraded/unhealthy classification. Zero values are
	// replaced with defaults.
	CPUWarnPercent            float64
	CPUCritPercent            float64
	MemWarnPercent            float64
	MemCritPercent            float64
	DiskWarnPercent           float64
	DiskCritPercent           float64
	ReplicationLagWarnSeconds float64
	ReplicationLagCritSeconds float64
}

// HealthProber runs the four probe families and aggregates their results.
// The zero value is not usable; callers must use NewHealthProber.
//
// A HealthProber is immutable after construction and therefore safe for
// concurrent use by any number of goroutines.
type HealthProber struct {
	cfg HealthProberConfig
}

// NewHealthProber returns a HealthProber with the given config, applying
// defaults for any zero-valued fields. It never panics.
func NewHealthProber(cfg HealthProberConfig) *HealthProber {
	if cfg.Runtime == "" {
		cfg.Runtime = RuntimeLinux
	}
	// PingPort == 0 means use the default of 80; a negative value skips
	// the ping probe entirely.
	if cfg.PingPort == 0 {
		cfg.PingPort = 80
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 3 * time.Second
	}
	if cfg.HTTPTimeout == 0 {
		cfg.HTTPTimeout = 5 * time.Second
	}
	if cfg.CommandTimeout == 0 {
		cfg.CommandTimeout = 10 * time.Second
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: cfg.HTTPTimeout}
	}
	if cfg.DBCheckCommand == "" {
		cfg.DBCheckCommand = `mysql -e "SELECT 1"`
	}
	if cfg.ReplicationCommand == "" {
		cfg.ReplicationCommand = `mysql -e "SHOW SLAVE STATUS\G"`
	}
	if cfg.CPUWarnPercent == 0 {
		cfg.CPUWarnPercent = 80
	}
	if cfg.CPUCritPercent == 0 {
		cfg.CPUCritPercent = 95
	}
	if cfg.MemWarnPercent == 0 {
		cfg.MemWarnPercent = 80
	}
	if cfg.MemCritPercent == 0 {
		cfg.MemCritPercent = 95
	}
	if cfg.DiskWarnPercent == 0 {
		cfg.DiskWarnPercent = 80
	}
	if cfg.DiskCritPercent == 0 {
		cfg.DiskCritPercent = 95
	}
	if cfg.ReplicationLagWarnSeconds == 0 {
		cfg.ReplicationLagWarnSeconds = 30
	}
	if cfg.ReplicationLagCritSeconds == 0 {
		cfg.ReplicationLagCritSeconds = 300
	}
	return &HealthProber{cfg: cfg}
}

// --- ProbeAll --------------------------------------------------------------

// ProbeAll runs all four probe families concurrently and returns the aggregated
// report. Each sub-probe respects the caller's context deadline; when ctx has
// no deadline CommandTimeout is applied.
func (p *HealthProber) ProbeAll(ctx context.Context, target string) HealthReport {
	start := time.Now()
	ctx, cancel := p.withTimeout(ctx, p.cfg.CommandTimeout)
	defer cancel()

	var (
		network NetworkHealth
		node    NodeHealth
		service ServiceHealth
		data    DataHealth
	)

	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); network = p.ProbeNetwork(ctx, target, p.cfg.DefaultPorts) }()
	go func() { defer wg.Done(); node = p.ProbeNode(ctx, target) }()
	go func() { defer wg.Done(); service = p.ProbeService(ctx, target, p.cfg.DefaultServices) }()
	go func() { defer wg.Done(); data = p.ProbeData(ctx, target) }()
	wg.Wait()

	status := aggregateStatus(network.Status, node.Status, service.Status, data.Status)
	summary := fmt.Sprintf("network=%s node=%s service=%s data=%s",
		network.Status, node.Status, service.Status, data.Status)

	log.Info("diagnosis: health probe complete",
		"target", target,
		"status", status,
		"duration_ms", time.Since(start).Milliseconds())

	return HealthReport{
		Target:   target,
		Status:   status,
		Network:  network,
		Node:     node,
		Service:  service,
		Data:     data,
		Summary:  summary,
		ProbedAt: time.Now(),
		Duration: time.Since(start),
	}
}

// --- ProbeNetwork ----------------------------------------------------------

// ProbeNetwork checks network connectivity to target via ping (TCP dial to
// PingPort), DNS forward lookup and TCP dial to each port in ports. The TCP
// port probes run concurrently. This probe uses only the net standard library
// and does not require a CommandExecutor.
func (p *HealthProber) ProbeNetwork(ctx context.Context, target string, ports []int) NetworkHealth {
	if target == "" {
		return NetworkHealth{Status: StatusUnknown, Err: "empty target"}
	}
	ctx, cancel := p.withTimeout(ctx, p.cfg.DialTimeout)
	defer cancel()

	var nh NetworkHealth

	// Ping (TCP dial to PingPort).
	if p.cfg.PingPort > 0 {
		nh.Ping = p.probePing(ctx, target)
	}

	// DNS lookup.
	nh.DNS = p.probeDNS(ctx, target)

	// TCP port probes (concurrent).
	if len(ports) > 0 {
		nh.TCP = make([]TCPResult, len(ports))
		var wg sync.WaitGroup
		wg.Add(len(ports))
		for i, port := range ports {
			go func(idx, pr int) {
				defer wg.Done()
				nh.TCP[idx] = p.probeTCP(ctx, target, pr)
			}(i, port)
		}
		wg.Wait()
	}

	nh.Status = p.aggregateNetwork(nh)
	return nh
}

// probePing measures TCP connect RTT to target:PingPort.
func (p *HealthProber) probePing(ctx context.Context, target string) PingResult {
	addr := net.JoinHostPort(target, strconv.Itoa(p.cfg.PingPort))
	start := time.Now()
	d := net.Dialer{Timeout: p.cfg.DialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	rtt := time.Since(start)
	if err != nil {
		return PingResult{Reachable: false, RTT: rtt, Err: err.Error()}
	}
	_ = conn.Close()
	return PingResult{Reachable: true, RTT: rtt}
}

// probeDNS performs a forward DNS lookup on target.
func (p *HealthProber) probeDNS(ctx context.Context, target string) DNSResult {
	// net.LookupHost does not accept a context in the standard library
	// before Go 1.20; we use the context only to check for cancellation
	// before starting the lookup. The resolver respects the DialTimeout
	// through the implicit deadline.
	if err := ctx.Err(); err != nil {
		return DNSResult{Err: err.Error()}
	}
	addrs, err := net.LookupHost(target)
	if err != nil {
		return DNSResult{Err: err.Error()}
	}
	return DNSResult{Resolved: true, Addrs: addrs}
}

// probeTCP dials target:port and reports whether the port is open.
func (p *HealthProber) probeTCP(ctx context.Context, target string, port int) TCPResult {
	addr := net.JoinHostPort(target, strconv.Itoa(port))
	start := time.Now()
	d := net.Dialer{Timeout: p.cfg.DialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	rtt := time.Since(start)
	if err != nil {
		return TCPResult{Port: port, Open: false, Latency: rtt, Err: err.Error()}
	}
	_ = conn.Close()
	return TCPResult{Port: port, Open: true, Latency: rtt}
}

// aggregateNetwork derives the overall network status from individual results.
// DNS failure is Unhealthy; ping failure is Degraded (may be firewall); any
// closed required TCP port is Degraded.
func (p *HealthProber) aggregateNetwork(nh NetworkHealth) HealthStatus {
	if !nh.DNS.Resolved && nh.DNS.Err != "" {
		return StatusUnhealthy
	}
	status := StatusHealthy
	if !nh.Ping.Reachable && nh.Ping.Err != "" {
		status = StatusDegraded
	}
	for _, t := range nh.TCP {
		if !t.Open && t.Err != "" {
			status = StatusDegraded
		}
	}
	return status
}

// --- ProbeNode -------------------------------------------------------------

// ProbeNode collects CPU, memory, disk, load-average and uptime metrics from
// the target by executing OS-specific commands through CommandExecutor. On
// Linux it uses top/free/df/procfs; on Windows it uses wmic.
func (p *HealthProber) ProbeNode(ctx context.Context, target string) NodeHealth {
	if target == "" {
		return NodeHealth{Status: StatusUnknown, Err: "empty target"}
	}
	if p.cfg.Executor == nil {
		return NodeHealth{Status: StatusUnknown, Err: "nil executor"}
	}
	ctx, cancel := p.withTimeout(ctx, p.cfg.CommandTimeout)
	defer cancel()

	if p.cfg.Runtime == RuntimeWindows {
		return p.probeNodeWindows(ctx, target)
	}
	return p.probeNodeLinux(ctx, target)
}

// probeNodeLinux collects node metrics from a Linux target.
func (p *HealthProber) probeNodeLinux(ctx context.Context, target string) NodeHealth {
	var nh NodeHealth
	var errCount int

	// CPU usage + cores.
	if out, _, code, err := p.cfg.Executor.Execute(ctx, target, "top -bn1"); err == nil && code == 0 {
		if cpu, pErr := parseLinuxCPU(out); pErr == nil {
			nh.CPU = cpu
		} else {
			errCount++
		}
	} else {
		errCount++
	}
	if out, _, code, err := p.cfg.Executor.Execute(ctx, target, "nproc"); err == nil && code == 0 {
		if cores, cErr := strconv.Atoi(strings.TrimSpace(out)); cErr == nil && cores > 0 {
			nh.CPU.Cores = cores
		}
	}

	// Memory.
	if out, _, code, err := p.cfg.Executor.Execute(ctx, target, "free -b"); err == nil && code == 0 {
		if mem, mErr := parseLinuxMemory(out); mErr == nil {
			nh.Memory = mem
		} else {
			errCount++
		}
	} else {
		errCount++
	}

	// Disk.
	if out, _, code, err := p.cfg.Executor.Execute(ctx, target, "df -B1"); err == nil && code == 0 {
		if disks, dErr := parseLinuxDisk(out); dErr == nil {
			nh.Disks = disks
		} else {
			errCount++
		}
	} else {
		errCount++
	}

	// Load average.
	if out, _, code, err := p.cfg.Executor.Execute(ctx, target, "cat /proc/loadavg"); err == nil && code == 0 {
		if load, lErr := parseLinuxLoad(out); lErr == nil {
			nh.Load = load
		} else {
			errCount++
		}
	} else {
		errCount++
	}

	// Uptime.
	if out, _, code, err := p.cfg.Executor.Execute(ctx, target, "cat /proc/uptime"); err == nil && code == 0 {
		if up, uErr := parseLinuxUptime(out); uErr == nil {
			nh.Uptime = up
		} else {
			errCount++
		}
	} else {
		errCount++
	}

	if errCount >= 5 {
		nh.Status = StatusUnknown
		nh.Err = "all node probes failed"
		return nh
	}
	nh.Status = p.classifyNode(nh)
	return nh
}

// probeNodeWindows collects node metrics from a Windows target via wmic.
func (p *HealthProber) probeNodeWindows(ctx context.Context, target string) NodeHealth {
	var nh NodeHealth
	var errCount int

	if out, _, code, err := p.cfg.Executor.Execute(ctx, target,
		"wmic cpu get loadpercentage /value"); err == nil && code == 0 {
		if cpu, pErr := parseWindowsCPU(out); pErr == nil {
			nh.CPU = cpu
		} else {
			errCount++
		}
	} else {
		errCount++
	}
	if out, _, code, err := p.cfg.Executor.Execute(ctx, target,
		"wmic cpu get numberofcores /value"); err == nil && code == 0 {
		nh.CPU.Cores = parseWindowsCores(out)
	}

	if out, _, code, err := p.cfg.Executor.Execute(ctx, target,
		"wmic OS get TotalVisibleMemorySize,FreePhysicalMemory /value"); err == nil && code == 0 {
		if mem, mErr := parseWindowsMemory(out); mErr == nil {
			nh.Memory = mem
		} else {
			errCount++
		}
	} else {
		errCount++
	}

	if out, _, code, err := p.cfg.Executor.Execute(ctx, target,
		"wmic logicaldisk get caption,freespace,size /value"); err == nil && code == 0 {
		if disks, dErr := parseWindowsDisk(out); dErr == nil {
			nh.Disks = disks
		} else {
			errCount++
		}
	} else {
		errCount++
	}

	if errCount >= 3 {
		nh.Status = StatusUnknown
		nh.Err = "most node probes failed"
		return nh
	}
	nh.Status = p.classifyNode(nh)
	return nh
}

// classifyNode derives the node status from resource utilization against
// configured thresholds.
func (p *HealthProber) classifyNode(nh NodeHealth) HealthStatus {
	status := StatusHealthy
	if nh.CPU.UsagePercent >= p.cfg.CPUCritPercent {
		return StatusUnhealthy
	}
	if nh.CPU.UsagePercent >= p.cfg.CPUWarnPercent {
		status = StatusDegraded
	}
	if nh.Memory.TotalBytes > 0 {
		memPct := float64(nh.Memory.UsedBytes) / float64(nh.Memory.TotalBytes) * 100
		if memPct >= p.cfg.MemCritPercent {
			return StatusUnhealthy
		}
		if memPct >= p.cfg.MemWarnPercent {
			status = StatusDegraded
		}
	}
	for _, d := range nh.Disks {
		if d.UsagePercent >= p.cfg.DiskCritPercent {
			return StatusUnhealthy
		}
		if d.UsagePercent >= p.cfg.DiskWarnPercent {
			status = StatusDegraded
		}
	}
	return status
}

// --- ProbeService ----------------------------------------------------------

// ProbeService checks each service in services on target: process liveness
// (via pgrep/tasklist), optional TCP port dial and optional HTTP /healthz
// probe. Services are probed concurrently.
func (p *HealthProber) ProbeService(ctx context.Context, target string, services []ServiceSpec) ServiceHealth {
	if target == "" {
		return ServiceHealth{Status: StatusUnknown, Err: "empty target"}
	}
	ctx, cancel := p.withTimeout(ctx, p.cfg.CommandTimeout)
	defer cancel()

	if len(services) == 0 {
		return ServiceHealth{Status: StatusHealthy}
	}

	infos := make([]ServiceInfo, len(services))
	var wg sync.WaitGroup
	wg.Add(len(services))
	for i, svc := range services {
		go func(idx int, s ServiceSpec) {
			defer wg.Done()
			infos[idx] = p.probeOneService(ctx, target, s)
		}(i, svc)
	}
	wg.Wait()

	sh := ServiceHealth{Services: infos}
	sh.Status = StatusHealthy
	for _, info := range infos {
		if info.Status == StatusUnhealthy {
			sh.Status = StatusUnhealthy
			break
		}
		if info.Status == StatusDegraded && sh.Status != StatusUnhealthy {
			sh.Status = StatusDegraded
		}
		if info.Status == StatusUnknown && sh.Status == StatusHealthy {
			sh.Status = StatusDegraded
		}
	}
	return sh
}

// probeOneService probes a single service.
func (p *HealthProber) probeOneService(ctx context.Context, target string, svc ServiceSpec) ServiceInfo {
	info := ServiceInfo{Name: svc.Name, Port: svc.Port, Status: StatusUnknown}

	// Process liveness.
	running, pid := p.probeProcess(ctx, target, svc.Name)
	info.Running = running
	info.PID = pid

	if !running {
		info.Status = StatusUnhealthy
		info.Message = "process not running"
	} else {
		info.Status = StatusHealthy
		info.Message = "process running"
	}

	// Port dial and HTTP probe.
	if svc.Port > 0 {
		tcp := p.probeTCP(ctx, target, svc.Port)
		if !tcp.Open {
			if info.Status == StatusHealthy {
				info.Status = StatusDegraded
			}
			info.Message = fmt.Sprintf("%s; port %d closed", info.Message, svc.Port)
		}
		info.HTTP = p.probeHTTP(ctx, target, svc.Port)
		if info.HTTP.Status == StatusUnhealthy {
			info.Status = StatusUnhealthy
		} else if info.HTTP.Status == StatusDegraded && info.Status == StatusHealthy {
			info.Status = StatusDegraded
		}
	}

	return info
}

// probeProcess checks whether a named process is running on target. Returns
// (running, pid); pid is 0 when not running or on Windows.
func (p *HealthProber) probeProcess(ctx context.Context, target, name string) (bool, int) {
	if p.cfg.Executor == nil {
		return false, 0
	}
	var cmd string
	if p.cfg.Runtime == RuntimeWindows {
		cmd = fmt.Sprintf(`tasklist /FI "IMAGENAME eq %s" /NH`, name)
	} else {
		cmd = fmt.Sprintf("pgrep -x %s", strconv.Quote(name))
	}
	out, _, code, err := p.cfg.Executor.Execute(ctx, target, cmd)
	if err != nil || code != 0 {
		return false, 0
	}
	if p.cfg.Runtime == RuntimeWindows {
		return strings.TrimSpace(out) != "" && !strings.Contains(out, "INFO:"), 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(strings.SplitN(out, "\n", 2)[0]))
	return pid > 0, pid
}

// probeHTTP performs a GET /healthz request to target:port.
func (p *HealthProber) probeHTTP(ctx context.Context, target string, port int) HTTPProbe {
	url := fmt.Sprintf("http://%s:%d/healthz", target, port)
	hp := HTTPProbe{URL: url, Status: StatusUnknown}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		hp.Err = err.Error()
		return hp
	}
	start := time.Now()
	resp, err := p.cfg.HTTPClient.Do(req)
	hp.RTT = time.Since(start)
	if err != nil {
		hp.Err = err.Error()
		hp.Status = StatusUnhealthy
		return hp
	}
	defer func() { _ = resp.Body.Close() }()

	hp.Code = resp.StatusCode
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		hp.Status = StatusHealthy
	case resp.StatusCode >= 500:
		hp.Status = StatusUnhealthy
	default:
		hp.Status = StatusDegraded
	}
	return hp
}

// --- ProbeData -------------------------------------------------------------

// ProbeData checks data-layer health: database connectivity, replication lag
// and disk space. Commands are executed through CommandExecutor.
func (p *HealthProber) ProbeData(ctx context.Context, target string) DataHealth {
	if target == "" {
		return DataHealth{Status: StatusUnknown, Err: "empty target"}
	}
	if p.cfg.Executor == nil {
		return DataHealth{Status: StatusUnknown, Err: "nil executor"}
	}
	ctx, cancel := p.withTimeout(ctx, p.cfg.CommandTimeout)
	defer cancel()

	var dh DataHealth
	var errCount int

	// DB connectivity.
	dh.DBConnection = p.probeDBConnection(ctx, target)
	if !dh.DBConnection.Connected {
		errCount++
	}

	// Replication lag.
	dh.ReplicationLag = p.probeReplication(ctx, target)
	if dh.ReplicationLag.Err != "" {
		errCount++
	}

	// Disk space (reuse node disk probe).
	diskCmd := "df -B1"
	if p.cfg.Runtime == RuntimeWindows {
		diskCmd = "wmic logicaldisk get caption,freespace,size /value"
	}
	if out, _, code, err := p.cfg.Executor.Execute(ctx, target, diskCmd); err == nil && code == 0 {
		var disks []DiskInfo
		var dErr error
		if p.cfg.Runtime == RuntimeWindows {
			disks, dErr = parseWindowsDisk(out)
		} else {
			disks, dErr = parseLinuxDisk(out)
		}
		if dErr == nil {
			dh.Disks = disks
		} else {
			errCount++
		}
	} else {
		errCount++
	}

	if errCount >= 3 {
		dh.Status = StatusUnknown
		dh.Err = "all data probes failed"
		return dh
	}
	dh.Status = p.classifyData(dh)
	return dh
}

// probeDBConnection runs the DB check command and measures latency.
func (p *HealthProber) probeDBConnection(ctx context.Context, target string) DBConnInfo {
	start := time.Now()
	_, stderr, code, err := p.cfg.Executor.Execute(ctx, target, p.cfg.DBCheckCommand)
	latency := time.Since(start)
	if err != nil {
		return DBConnInfo{Latency: latency, Err: err.Error()}
	}
	if code != 0 {
		return DBConnInfo{Latency: latency, Err: fmt.Sprintf("exit code %d: %s", code, stderr)}
	}
	return DBConnInfo{Connected: true, Latency: latency}
}

// probeReplication runs the replication status command and parses lag.
func (p *HealthProber) probeReplication(ctx context.Context, target string) ReplicationInfo {
	out, _, code, err := p.cfg.Executor.Execute(ctx, target, p.cfg.ReplicationCommand)
	if err != nil {
		return ReplicationInfo{Err: err.Error()}
	}
	if code != 0 {
		return ReplicationInfo{Err: fmt.Sprintf("exit code %d", code)}
	}
	return parseReplicationLag(out)
}

// classifyData derives the data health status from sub-probe results.
func (p *HealthProber) classifyData(dh DataHealth) HealthStatus {
	if !dh.DBConnection.Connected {
		return StatusUnhealthy
	}
	status := StatusHealthy
	if dh.ReplicationLag.Err == "" && dh.ReplicationLag.Healthy {
		if dh.ReplicationLag.LagSeconds >= p.cfg.ReplicationLagCritSeconds {
			return StatusUnhealthy
		}
		if dh.ReplicationLag.LagSeconds >= p.cfg.ReplicationLagWarnSeconds {
			status = StatusDegraded
		}
	} else if dh.ReplicationLag.Err != "" {
		status = StatusDegraded
	}
	for _, d := range dh.Disks {
		if d.UsagePercent >= p.cfg.DiskCritPercent {
			return StatusUnhealthy
		}
		if d.UsagePercent >= p.cfg.DiskWarnPercent {
			status = StatusDegraded
		}
	}
	return status
}

// --- Helpers ---------------------------------------------------------------

// withTimeout returns ctx unchanged when it already has a deadline; otherwise
// it derives a child context with the given timeout. The returned cancel
// function must be called by the caller.
func (p *HealthProber) withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

// aggregateStatus reduces multiple statuses to a single one. Unhealthy wins,
// then Degraded, then Unknown, then Healthy.
func aggregateStatus(statuses ...HealthStatus) HealthStatus {
	var hasUnhealthy, hasDegraded, hasUnknown bool
	for _, s := range statuses {
		switch s {
		case StatusUnhealthy:
			hasUnhealthy = true
		case StatusDegraded:
			hasDegraded = true
		case StatusUnknown:
			hasUnknown = true
		}
	}
	switch {
	case hasUnhealthy:
		return StatusUnhealthy
	case hasDegraded:
		return StatusDegraded
	case hasUnknown:
		return StatusUnknown
	default:
		return StatusHealthy
	}
}

// --- Linux parsers ---------------------------------------------------------

// cpuIdleRe matches the idle percentage in a top Cpu(s) line, e.g. "91.5 id"
// or "91.5%id".
var cpuIdleRe = regexp.MustCompile(`(\d+\.?\d*)\s*%?id`)

// parseLinuxCPU extracts CPU usage from top -bn1 output. Usage is computed as
// 100 - idle.
func parseLinuxCPU(stdout string) (CPUInfo, error) {
	for _, line := range strings.Split(stdout, "\n") {
		if !strings.Contains(line, "Cpu(s)") {
			continue
		}
		m := cpuIdleRe.FindStringSubmatch(line)
		if len(m) < 2 {
			return CPUInfo{}, fmt.Errorf("parse cpu: idle value not found in %q", line)
		}
		idle, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			return CPUInfo{}, fmt.Errorf("parse cpu: %w", err)
		}
		return CPUInfo{UsagePercent: 100 - idle}, nil
	}
	return CPUInfo{}, fmt.Errorf("parse cpu: Cpu(s) line not found")
}

// parseLinuxMemory extracts memory info from free -b output.
func parseLinuxMemory(stdout string) (MemoryInfo, error) {
	for _, line := range strings.Split(stdout, "\n") {
		if !strings.HasPrefix(line, "Mem:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			return MemoryInfo{}, fmt.Errorf("parse memory: unexpected free output: %q", line)
		}
		total, _ := strconv.ParseUint(fields[1], 10, 64)
		used, _ := strconv.ParseUint(fields[2], 10, 64)
		free, _ := strconv.ParseUint(fields[3], 10, 64)
		mem := MemoryInfo{TotalBytes: total, UsedBytes: used, FreeBytes: free}
		if total > 0 {
			mem.UsagePercent = float64(used) / float64(total) * 100
		}
		return mem, nil
	}
	return MemoryInfo{}, fmt.Errorf("parse memory: Mem line not found")
}

// parseLinuxDisk extracts disk info from df -B1 output.
func parseLinuxDisk(stdout string) ([]DiskInfo, error) {
	lines := strings.Split(stdout, "\n")
	var disks []DiskInfo
	for i, line := range lines {
		if i == 0 {
			continue // header
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		total, _ := strconv.ParseUint(fields[1], 10, 64)
		used, _ := strconv.ParseUint(fields[2], 10, 64)
		free, _ := strconv.ParseUint(fields[3], 10, 64)
		pctStr := strings.TrimSuffix(fields[4], "%")
		pct, _ := strconv.ParseFloat(pctStr, 64)
		disks = append(disks, DiskInfo{
			Filesystem:   fields[0],
			Mount:        fields[5],
			TotalBytes:   total,
			UsedBytes:    used,
			FreeBytes:    free,
			UsagePercent: pct,
		})
	}
	if len(disks) == 0 {
		return nil, fmt.Errorf("parse disk: no filesystems parsed")
	}
	return disks, nil
}

// parseLinuxLoad extracts load averages from /proc/loadavg output.
func parseLinuxLoad(stdout string) (LoadInfo, error) {
	fields := strings.Fields(strings.TrimSpace(stdout))
	if len(fields) < 3 {
		return LoadInfo{}, fmt.Errorf("parse load: unexpected /proc/loadavg output: %q", stdout)
	}
	load := LoadInfo{}
	load.Load1, _ = strconv.ParseFloat(fields[0], 64)
	load.Load5, _ = strconv.ParseFloat(fields[1], 64)
	load.Load15, _ = strconv.ParseFloat(fields[2], 64)
	return load, nil
}

// parseLinuxUptime extracts uptime from /proc/uptime output.
func parseLinuxUptime(stdout string) (time.Duration, error) {
	fields := strings.Fields(strings.TrimSpace(stdout))
	if len(fields) < 1 {
		return 0, fmt.Errorf("parse uptime: unexpected /proc/uptime output: %q", stdout)
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("parse uptime: %w", err)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

// --- Windows parsers -------------------------------------------------------

// wmiLoadRe matches LoadPercentage=N in wmic cpu output.
var wmiLoadRe = regexp.MustCompile(`LoadPercentage=(\d+)`)

// wmiCoresRe matches NumberOfCores=N in wmic cpu output.
var wmiCoresRe = regexp.MustCompile(`NumberOfCores=(\d+)`)

// wmiTotalMemRe matches TotalVisibleMemorySize=N.
var wmiTotalMemRe = regexp.MustCompile(`TotalVisibleMemorySize=(\d+)`)

// wmiFreeMemRe matches FreePhysicalMemory=N.
var wmiFreeMemRe = regexp.MustCompile(`FreePhysicalMemory=(\d+)`)

// wmiCaptionRe matches Caption=X:.
var wmiCaptionRe = regexp.MustCompile(`Caption=(\S+)`)

// wmiFreeSpaceRe matches FreeSpace=N.
var wmiFreeSpaceRe = regexp.MustCompile(`FreeSpace=(\d+)`)

// wmiSizeRe matches Size=N.
var wmiSizeRe = regexp.MustCompile(`Size=(\d+)`)

// parseWindowsCPU extracts CPU usage from wmic cpu get loadpercentage output.
func parseWindowsCPU(stdout string) (CPUInfo, error) {
	m := wmiLoadRe.FindStringSubmatch(stdout)
	if len(m) < 2 {
		return CPUInfo{}, fmt.Errorf("parse windows cpu: LoadPercentage not found")
	}
	load, err := strconv.Atoi(m[1])
	if err != nil {
		return CPUInfo{}, fmt.Errorf("parse windows cpu: %w", err)
	}
	return CPUInfo{UsagePercent: float64(load)}, nil
}

// parseWindowsCores sums NumberOfCores values in wmic output.
func parseWindowsCores(stdout string) int {
	matches := wmiCoresRe.FindAllStringSubmatch(stdout, -1)
	var total int
	for _, m := range matches {
		if len(m) >= 2 {
			if v, err := strconv.Atoi(m[1]); err == nil {
				total += v
			}
		}
	}
	return total
}

// parseWindowsMemory extracts memory info from wmic OS output. Values are in
// KB and converted to bytes.
func parseWindowsMemory(stdout string) (MemoryInfo, error) {
	var totalKB, freeKB uint64
	if m := wmiTotalMemRe.FindStringSubmatch(stdout); len(m) >= 2 {
		totalKB, _ = strconv.ParseUint(m[1], 10, 64)
	}
	if m := wmiFreeMemRe.FindStringSubmatch(stdout); len(m) >= 2 {
		freeKB, _ = strconv.ParseUint(m[1], 10, 64)
	}
	if totalKB == 0 {
		return MemoryInfo{}, fmt.Errorf("parse windows memory: TotalVisibleMemorySize not found")
	}
	mem := MemoryInfo{
		TotalBytes: totalKB * 1024,
		FreeBytes:  freeKB * 1024,
	}
	mem.UsedBytes = mem.TotalBytes - mem.FreeBytes
	mem.UsagePercent = float64(mem.UsedBytes) / float64(mem.TotalBytes) * 100
	return mem, nil
}

// parseWindowsDisk extracts disk info from wmic logicaldisk output. Each
// drive is a block separated by a blank line.
func parseWindowsDisk(stdout string) ([]DiskInfo, error) {
	var disks []DiskInfo
	// Normalize line endings and split into blocks.
	normalized := strings.ReplaceAll(stdout, "\r\n", "\n")
	blocks := strings.Split(normalized, "\n\n")
	for _, block := range blocks {
		if !strings.Contains(block, "Caption=") {
			continue
		}
		var d DiskInfo
		if m := wmiCaptionRe.FindStringSubmatch(block); len(m) >= 2 {
			d.Filesystem = m[1]
			d.Mount = m[1]
		} else {
			continue
		}
		if m := wmiSizeRe.FindStringSubmatch(block); len(m) >= 2 {
			d.TotalBytes, _ = strconv.ParseUint(m[1], 10, 64)
		}
		if m := wmiFreeSpaceRe.FindStringSubmatch(block); len(m) >= 2 {
			d.FreeBytes, _ = strconv.ParseUint(m[1], 10, 64)
		}
		d.UsedBytes = d.TotalBytes - d.FreeBytes
		if d.TotalBytes > 0 {
			d.UsagePercent = float64(d.UsedBytes) / float64(d.TotalBytes) * 100
		}
		disks = append(disks, d)
	}
	if len(disks) == 0 {
		return nil, fmt.Errorf("parse windows disk: no logical disks parsed")
	}
	return disks, nil
}

// --- Replication parser ----------------------------------------------------

// replLagRe matches Seconds_Behind_Master: N or Seconds_Behind_Master: NULL.
var replLagRe = regexp.MustCompile(`Seconds_Behind_Master:\s*(\d+|NULL)`)

// parseReplicationLag extracts replication lag from SHOW SLAVE STATUS output.
// LagSeconds is -1 when the value is NULL (replication not running).
func parseReplicationLag(stdout string) ReplicationInfo {
	m := replLagRe.FindStringSubmatch(stdout)
	if len(m) < 2 {
		return ReplicationInfo{Err: "Seconds_Behind_Master not found"}
	}
	if m[1] == "NULL" {
		return ReplicationInfo{LagSeconds: -1, Healthy: false, Err: "replication not running"}
	}
	lag, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return ReplicationInfo{Err: fmt.Sprintf("parse lag: %v", err)}
	}
	return ReplicationInfo{LagSeconds: lag, Healthy: true}
}
