package diagnosis

// Tests for Phase A5 health prober. We reuse the mockExecutor / mockResult /
// newMockExecutor stub defined in log_collector_test.go (canned output keyed
// by command string), letting us exercise every parse path without a real
// remote target. Network and HTTP probes use localhost + httptest.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- NewHealthProber -------------------------------------------------------

func TestNewHealthProberDefaults(t *testing.T) {
	p := NewHealthProber(HealthProberConfig{})
	assert.Equal(t, RuntimeLinux, p.cfg.Runtime)
	assert.Equal(t, 80, p.cfg.PingPort)
	assert.Equal(t, 3*time.Second, p.cfg.DialTimeout)
	assert.Equal(t, 5*time.Second, p.cfg.HTTPTimeout)
	assert.Equal(t, 10*time.Second, p.cfg.CommandTimeout)
	assert.NotNil(t, p.cfg.HTTPClient)
	assert.Contains(t, p.cfg.DBCheckCommand, "mysql")
	assert.Contains(t, p.cfg.ReplicationCommand, "SHOW SLAVE STATUS")
	assert.Equal(t, 80.0, p.cfg.CPUWarnPercent)
	assert.Equal(t, 95.0, p.cfg.CPUCritPercent)
	assert.Equal(t, 80.0, p.cfg.MemWarnPercent)
	assert.Equal(t, 95.0, p.cfg.MemCritPercent)
	assert.Equal(t, 80.0, p.cfg.DiskWarnPercent)
	assert.Equal(t, 95.0, p.cfg.DiskCritPercent)
	assert.Equal(t, 30.0, p.cfg.ReplicationLagWarnSeconds)
	assert.Equal(t, 300.0, p.cfg.ReplicationLagCritSeconds)
}

func TestNewHealthProberCustom(t *testing.T) {
	p := NewHealthProber(HealthProberConfig{
		Runtime:        RuntimeWindows,
		PingPort:       443,
		DialTimeout:    2 * time.Second,
		HTTPTimeout:    3 * time.Second,
		CommandTimeout: 5 * time.Second,
		CPUWarnPercent: 70,
		CPUCritPercent: 90,
	})
	assert.Equal(t, RuntimeWindows, p.cfg.Runtime)
	assert.Equal(t, 443, p.cfg.PingPort)
	assert.Equal(t, 2*time.Second, p.cfg.DialTimeout)
	assert.Equal(t, 70.0, p.cfg.CPUWarnPercent)
	assert.Equal(t, 90.0, p.cfg.CPUCritPercent)
}

// --- aggregateStatus -------------------------------------------------------

func TestAggregateStatus(t *testing.T) {
	tests := []struct {
		name   string
		inputs []HealthStatus
		want   HealthStatus
	}{
		{"all healthy", []HealthStatus{StatusHealthy, StatusHealthy}, StatusHealthy},
		{"one degraded", []HealthStatus{StatusHealthy, StatusDegraded}, StatusDegraded},
		{"one unhealthy", []HealthStatus{StatusDegraded, StatusUnhealthy}, StatusUnhealthy},
		{"one unknown", []HealthStatus{StatusHealthy, StatusUnknown}, StatusUnknown},
		{"unhealthy beats degraded", []HealthStatus{StatusDegraded, StatusUnhealthy, StatusUnknown}, StatusUnhealthy},
		{"degraded beats unknown", []HealthStatus{StatusUnknown, StatusDegraded}, StatusDegraded},
		{"empty", nil, StatusHealthy},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, aggregateStatus(tc.inputs...))
		})
	}
}

// --- ProbeNetwork ----------------------------------------------------------

func TestProbeNetworkEmptyTarget(t *testing.T) {
	p := NewHealthProber(HealthProberConfig{})
	nh := p.ProbeNetwork(context.Background(), "", []int{80})
	assert.Equal(t, StatusUnknown, nh.Status)
	assert.Contains(t, nh.Err, "empty target")
}

func TestProbeNetworkLocalhost(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()

	addr := ln.Addr().(*net.TCPAddr)
	port := addr.Port

	p := NewHealthProber(HealthProberConfig{
		PingPort:    port,
		DialTimeout: time.Second,
	})
	nh := p.ProbeNetwork(context.Background(), "127.0.0.1", []int{port})
	assert.Equal(t, StatusHealthy, nh.Status)
	assert.True(t, nh.Ping.Reachable)
	assert.True(t, nh.DNS.Resolved)
	require.Len(t, nh.TCP, 1)
	assert.True(t, nh.TCP[0].Open)
	assert.Equal(t, port, nh.TCP[0].Port)
}

func TestProbeNetworkClosedPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().(*net.TCPAddr)
	port := addr.Port
	_ = ln.Close()

	p := NewHealthProber(HealthProberConfig{
		PingPort:    -1,
		DialTimeout: 200 * time.Millisecond,
	})
	nh := p.ProbeNetwork(context.Background(), "127.0.0.1", []int{port})
	assert.True(t, nh.DNS.Resolved)
	require.Len(t, nh.TCP, 1)
	assert.False(t, nh.TCP[0].Open)
	assert.Equal(t, StatusDegraded, nh.Status)
}

func TestProbeNetworkDNSFailure(t *testing.T) {
	p := NewHealthProber(HealthProberConfig{
		PingPort:    -1,
		DialTimeout: 200 * time.Millisecond,
	})
	nh := p.ProbeNetwork(context.Background(), "nonexistent.invalid.domain.local", nil)
	assert.False(t, nh.DNS.Resolved)
	assert.NotEmpty(t, nh.DNS.Err)
	assert.Equal(t, StatusUnhealthy, nh.Status)
}

func TestProbeNetworkCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p := NewHealthProber(HealthProberConfig{PingPort: -1})
	nh := p.ProbeNetwork(ctx, "127.0.0.1", nil)
	assert.False(t, nh.DNS.Resolved)
}

func TestProbeNetworkNoPorts(t *testing.T) {
	p := NewHealthProber(HealthProberConfig{PingPort: -1})
	nh := p.ProbeNetwork(context.Background(), "127.0.0.1", nil)
	assert.True(t, nh.DNS.Resolved)
	assert.Empty(t, nh.TCP)
	assert.Equal(t, StatusHealthy, nh.Status)
}

func TestProbeNetworkPingFailure(t *testing.T) {
	p := NewHealthProber(HealthProberConfig{
		PingPort:    1,
		DialTimeout: 200 * time.Millisecond,
	})
	nh := p.ProbeNetwork(context.Background(), "127.0.0.1", nil)
	assert.False(t, nh.Ping.Reachable)
	assert.NotEmpty(t, nh.Ping.Err)
	assert.Equal(t, StatusDegraded, nh.Status)
}

func TestProbeNetworkConcurrentPorts(t *testing.T) {
	ln1, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln1.Close() }()
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln2.Close() }()

	port1 := ln1.Addr().(*net.TCPAddr).Port
	port2 := ln2.Addr().(*net.TCPAddr).Port

	p := NewHealthProber(HealthProberConfig{
		PingPort:    -1,
		DialTimeout: time.Second,
	})
	nh := p.ProbeNetwork(context.Background(), "127.0.0.1", []int{port1, port2})
	require.Len(t, nh.TCP, 2)
	assert.True(t, nh.TCP[0].Open)
	assert.True(t, nh.TCP[1].Open)
	assert.Equal(t, port1, nh.TCP[0].Port)
	assert.Equal(t, port2, nh.TCP[1].Port)
}

func TestProbeNetworkMultiplePortMix(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	openPort := ln.Addr().(*net.TCPAddr).Port

	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	closedPort := ln2.Addr().(*net.TCPAddr).Port
	_ = ln2.Close()

	p := NewHealthProber(HealthProberConfig{
		PingPort:    -1,
		DialTimeout: 200 * time.Millisecond,
	})
	nh := p.ProbeNetwork(context.Background(), "127.0.0.1", []int{openPort, closedPort})
	require.Len(t, nh.TCP, 2)
	assert.True(t, nh.TCP[0].Open)
	assert.False(t, nh.TCP[1].Open)
	assert.Equal(t, StatusDegraded, nh.Status)
}

// --- ProbeNode (Linux) -----------------------------------------------------

func TestProbeNodeEmptyTarget(t *testing.T) {
	p := NewHealthProber(HealthProberConfig{Executor: newMockExecutor()})
	nh := p.ProbeNode(context.Background(), "")
	assert.Equal(t, StatusUnknown, nh.Status)
	assert.Contains(t, nh.Err, "empty target")
}

func TestProbeNodeNilExecutor(t *testing.T) {
	p := NewHealthProber(HealthProberConfig{})
	nh := p.ProbeNode(context.Background(), "host1")
	assert.Equal(t, StatusUnknown, nh.Status)
	assert.Contains(t, nh.Err, "nil executor")
}

func TestProbeNodeLinuxHealthy(t *testing.T) {
	exec := newMockExecutor()
	exec.set("top -bn1", mockResult{stdout: sampleTopOutput()})
	exec.set("nproc", mockResult{stdout: "4\n"})
	exec.set("free -b", mockResult{stdout: sampleFreeOutput()})
	exec.set("df -B1", mockResult{stdout: sampleDfOutput()})
	exec.set("cat /proc/loadavg", mockResult{stdout: "0.10 0.20 0.30 1/100 12345\n"})
	exec.set("cat /proc/uptime", mockResult{stdout: "3600.50 7200.00\n"})

	p := NewHealthProber(HealthProberConfig{Executor: exec})
	nh := p.ProbeNode(context.Background(), "host1")
	assert.Equal(t, StatusHealthy, nh.Status)
	assert.InDelta(t, 8.5, nh.CPU.UsagePercent, 0.1)
	assert.Equal(t, 4, nh.CPU.Cores)
	assert.Equal(t, uint64(8589934592), nh.Memory.TotalBytes)
	assert.Equal(t, uint64(1073741824), nh.Memory.UsedBytes)
	assert.InDelta(t, 12.5, nh.Memory.UsagePercent, 0.1)
	require.Len(t, nh.Disks, 2)
	assert.Equal(t, "/", nh.Disks[0].Mount)
	assert.InDelta(t, 50.0, nh.Disks[0].UsagePercent, 0.1)
	assert.InDelta(t, 0.10, nh.Load.Load1, 0.001)
	assert.Equal(t, time.Duration(3600.50*float64(time.Second)), nh.Uptime)
}

func TestProbeNodeLinuxDegradedCPU(t *testing.T) {
	exec := newMockExecutor()
	exec.set("top -bn1", mockResult{stdout: sampleTopHighCPU()})
	exec.set("nproc", mockResult{stdout: "2\n"})
	exec.set("free -b", mockResult{stdout: sampleFreeOutput()})
	exec.set("df -B1", mockResult{stdout: sampleDfOutput()})
	exec.set("cat /proc/loadavg", mockResult{stdout: "1.0 1.0 1.0 1/100 12345\n"})
	exec.set("cat /proc/uptime", mockResult{stdout: "100.0 200.0\n"})

	p := NewHealthProber(HealthProberConfig{Executor: exec})
	nh := p.ProbeNode(context.Background(), "host1")
	assert.Equal(t, StatusDegraded, nh.Status)
	assert.InDelta(t, 85.0, nh.CPU.UsagePercent, 0.1)
}

func TestProbeNodeLinuxUnhealthyCPU(t *testing.T) {
	exec := newMockExecutor()
	exec.set("top -bn1", mockResult{stdout: sampleTopCriticalCPU()})
	exec.set("nproc", mockResult{stdout: "2\n"})
	exec.set("free -b", mockResult{stdout: sampleFreeOutput()})
	exec.set("df -B1", mockResult{stdout: sampleDfOutput()})
	exec.set("cat /proc/loadavg", mockResult{stdout: "1.0 1.0 1.0 1/100 12345\n"})
	exec.set("cat /proc/uptime", mockResult{stdout: "100.0 200.0\n"})

	p := NewHealthProber(HealthProberConfig{Executor: exec})
	nh := p.ProbeNode(context.Background(), "host1")
	assert.Equal(t, StatusUnhealthy, nh.Status)
	assert.InDelta(t, 97.0, nh.CPU.UsagePercent, 0.1)
}

func TestProbeNodeLinuxUnhealthyMemory(t *testing.T) {
	exec := newMockExecutor()
	exec.set("top -bn1", mockResult{stdout: sampleTopOutput()})
	exec.set("nproc", mockResult{stdout: "4\n"})
	exec.set("free -b", mockResult{stdout: sampleFreeCriticalMem()})
	exec.set("df -B1", mockResult{stdout: sampleDfOutput()})
	exec.set("cat /proc/loadavg", mockResult{stdout: "0.1 0.1 0.1 1/100 12345\n"})
	exec.set("cat /proc/uptime", mockResult{stdout: "100.0 200.0\n"})

	p := NewHealthProber(HealthProberConfig{Executor: exec})
	nh := p.ProbeNode(context.Background(), "host1")
	assert.Equal(t, StatusUnhealthy, nh.Status)
	assert.InDelta(t, 96.0, nh.Memory.UsagePercent, 0.1)
}

func TestProbeNodeLinuxDegradedDisk(t *testing.T) {
	exec := newMockExecutor()
	exec.set("top -bn1", mockResult{stdout: sampleTopOutput()})
	exec.set("nproc", mockResult{stdout: "4\n"})
	exec.set("free -b", mockResult{stdout: sampleFreeOutput()})
	exec.set("df -B1", mockResult{stdout: sampleDfDegradedDisk()})
	exec.set("cat /proc/loadavg", mockResult{stdout: "0.1 0.1 0.1 1/100 12345\n"})
	exec.set("cat /proc/uptime", mockResult{stdout: "100.0 200.0\n"})

	p := NewHealthProber(HealthProberConfig{Executor: exec})
	nh := p.ProbeNode(context.Background(), "host1")
	assert.Equal(t, StatusDegraded, nh.Status)
}

func TestProbeNodeLinuxAllFail(t *testing.T) {
	exec := newMockExecutor()
	p := NewHealthProber(HealthProberConfig{Executor: exec})
	nh := p.ProbeNode(context.Background(), "host1")
	assert.Equal(t, StatusUnknown, nh.Status)
	assert.Contains(t, nh.Err, "all node probes failed")
}

func TestProbeNodeLinuxPartialFailure(t *testing.T) {
	exec := newMockExecutor()
	exec.set("top -bn1", mockResult{stdout: sampleTopOutput()})
	exec.set("nproc", mockResult{stdout: "4\n"})
	exec.set("free -b", mockResult{stdout: sampleFreeOutput()})

	p := NewHealthProber(HealthProberConfig{Executor: exec})
	nh := p.ProbeNode(context.Background(), "host1")
	assert.Equal(t, StatusHealthy, nh.Status)
	assert.InDelta(t, 8.5, nh.CPU.UsagePercent, 0.1)
	assert.Equal(t, uint64(8589934592), nh.Memory.TotalBytes)
}

func TestProbeNodeLinuxHighLoad(t *testing.T) {
	exec := newMockExecutor()
	exec.set("top -bn1", mockResult{stdout: sampleTopOutput()})
	exec.set("nproc", mockResult{stdout: "4\n"})
	exec.set("free -b", mockResult{stdout: sampleFreeOutput()})
	exec.set("df -B1", mockResult{stdout: sampleDfOutput()})
	exec.set("cat /proc/loadavg", mockResult{stdout: "8.0 8.0 8.0 1/100 12345\n"})
	exec.set("cat /proc/uptime", mockResult{stdout: "100.0 200.0\n"})

	p := NewHealthProber(HealthProberConfig{Executor: exec})
	nh := p.ProbeNode(context.Background(), "host1")
	assert.InDelta(t, 8.0, nh.Load.Load1, 0.001)
	assert.Equal(t, StatusHealthy, nh.Status)
}

func TestProbeNodeContextCancelled(t *testing.T) {
	exec := newMockExecutor()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p := NewHealthProber(HealthProberConfig{Executor: exec})
	nh := p.ProbeNode(ctx, "host1")
	assert.Equal(t, StatusUnknown, nh.Status)
}

// --- ProbeNode (Windows) ---------------------------------------------------

func TestProbeNodeWindowsHealthy(t *testing.T) {
	exec := newMockExecutor()
	exec.set("wmic cpu get loadpercentage /value", mockResult{stdout: "\nLoadPercentage=15\n\n"})
	exec.set("wmic cpu get numberofcores /value", mockResult{stdout: "\nNumberOfCores=4\n\n"})
	exec.set("wmic OS get TotalVisibleMemorySize,FreePhysicalMemory /value",
		mockResult{stdout: "\nFreePhysicalMemory=4000000\nTotalVisibleMemorySize=8000000\n\n"})
	exec.set("wmic logicaldisk get caption,freespace,size /value",
		mockResult{stdout: sampleWmicDiskOutput()})

	p := NewHealthProber(HealthProberConfig{Executor: exec, Runtime: RuntimeWindows})
	nh := p.ProbeNode(context.Background(), "host1")
	assert.Equal(t, StatusHealthy, nh.Status)
	assert.InDelta(t, 15.0, nh.CPU.UsagePercent, 0.1)
	assert.Equal(t, 4, nh.CPU.Cores)
	assert.Equal(t, uint64(8000000*1024), nh.Memory.TotalBytes)
	require.Len(t, nh.Disks, 1)
	assert.Equal(t, "C:", nh.Disks[0].Mount)
}

func TestProbeNodeWindowsHighCPU(t *testing.T) {
	exec := newMockExecutor()
	exec.set("wmic cpu get loadpercentage /value", mockResult{stdout: "\nLoadPercentage=90\n\n"})
	exec.set("wmic cpu get numberofcores /value", mockResult{stdout: "\nNumberOfCores=2\n\n"})
	exec.set("wmic OS get TotalVisibleMemorySize,FreePhysicalMemory /value",
		mockResult{stdout: "\nFreePhysicalMemory=4000000\nTotalVisibleMemorySize=8000000\n\n"})
	exec.set("wmic logicaldisk get caption,freespace,size /value",
		mockResult{stdout: sampleWmicDiskOutput()})

	p := NewHealthProber(HealthProberConfig{Executor: exec, Runtime: RuntimeWindows})
	nh := p.ProbeNode(context.Background(), "host1")
	assert.Equal(t, StatusDegraded, nh.Status)
}

func TestProbeNodeWindowsPartialFail(t *testing.T) {
	exec := newMockExecutor()
	exec.set("wmic cpu get loadpercentage /value", mockResult{stdout: "\nLoadPercentage=10\n\n"})
	exec.set("wmic cpu get numberofcores /value", mockResult{stdout: "\nNumberOfCores=2\n\n"})

	p := NewHealthProber(HealthProberConfig{Executor: exec, Runtime: RuntimeWindows})
	nh := p.ProbeNode(context.Background(), "host1")
	assert.Equal(t, StatusHealthy, nh.Status)
	assert.InDelta(t, 10.0, nh.CPU.UsagePercent, 0.1)
}

// --- ProbeService ----------------------------------------------------------

func TestProbeServiceEmptyTarget(t *testing.T) {
	p := NewHealthProber(HealthProberConfig{Executor: newMockExecutor()})
	sh := p.ProbeService(context.Background(), "", nil)
	assert.Equal(t, StatusUnknown, sh.Status)
	assert.Contains(t, sh.Err, "empty target")
}

func TestProbeServiceEmptyList(t *testing.T) {
	p := NewHealthProber(HealthProberConfig{Executor: newMockExecutor()})
	sh := p.ProbeService(context.Background(), "host1", nil)
	assert.Equal(t, StatusHealthy, sh.Status)
}

func TestProbeServiceRunningWithHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/healthz", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := srv.Listener.Addr().(*net.TCPAddr)
	port := addr.Port

	exec := newMockExecutor()
	exec.set(fmt.Sprintf("pgrep -x %q", "nginx"), mockResult{stdout: "1234\n"})

	p := NewHealthProber(HealthProberConfig{Executor: exec})
	sh := p.ProbeService(context.Background(), "127.0.0.1", []ServiceSpec{
		{Name: "nginx", Port: port},
	})
	require.Len(t, sh.Services, 1)
	svc := sh.Services[0]
	assert.True(t, svc.Running)
	assert.Equal(t, 1234, svc.PID)
	assert.Equal(t, StatusHealthy, svc.Status)
	assert.Equal(t, http.StatusOK, svc.HTTP.Code)
	assert.Equal(t, StatusHealthy, svc.HTTP.Status)
	assert.Equal(t, StatusHealthy, sh.Status)
}

func TestProbeServiceNotRunning(t *testing.T) {
	exec := newMockExecutor()

	p := NewHealthProber(HealthProberConfig{Executor: exec})
	sh := p.ProbeService(context.Background(), "host1", []ServiceSpec{
		{Name: "nginx"},
	})
	require.Len(t, sh.Services, 1)
	assert.False(t, sh.Services[0].Running)
	assert.Equal(t, StatusUnhealthy, sh.Services[0].Status)
	assert.Equal(t, StatusUnhealthy, sh.Status)
}

func TestProbeServiceHTTPUnhealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	addr := srv.Listener.Addr().(*net.TCPAddr)
	port := addr.Port

	exec := newMockExecutor()
	exec.set(fmt.Sprintf("pgrep -x %q", "api"), mockResult{stdout: "999\n"})

	p := NewHealthProber(HealthProberConfig{Executor: exec})
	sh := p.ProbeService(context.Background(), "127.0.0.1", []ServiceSpec{
		{Name: "api", Port: port},
	})
	require.Len(t, sh.Services, 1)
	assert.Equal(t, StatusUnhealthy, sh.Services[0].Status)
	assert.Equal(t, StatusUnhealthy, sh.Status)
}

func TestProbeServiceHTTPDegraded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	addr := srv.Listener.Addr().(*net.TCPAddr)
	port := addr.Port

	exec := newMockExecutor()
	exec.set(fmt.Sprintf("pgrep -x %q", "svc"), mockResult{stdout: "100\n"})

	p := NewHealthProber(HealthProberConfig{Executor: exec})
	sh := p.ProbeService(context.Background(), "127.0.0.1", []ServiceSpec{
		{Name: "svc", Port: port},
	})
	require.Len(t, sh.Services, 1)
	assert.Equal(t, StatusDegraded, sh.Services[0].Status)
}

func TestProbeServiceWindowsProcess(t *testing.T) {
	exec := newMockExecutor()
	exec.set(`tasklist /FI "IMAGENAME eq nginx.exe" /NH`, mockResult{
		stdout: "nginx.exe                   1234 Console                    1     12,000 K\n",
	})

	p := NewHealthProber(HealthProberConfig{Executor: exec, Runtime: RuntimeWindows})
	sh := p.ProbeService(context.Background(), "host1", []ServiceSpec{
		{Name: "nginx.exe"},
	})
	require.Len(t, sh.Services, 1)
	assert.True(t, sh.Services[0].Running)
	assert.Equal(t, StatusHealthy, sh.Status)
}

func TestProbeServiceMultipleMixed(t *testing.T) {
	exec := newMockExecutor()
	exec.set(fmt.Sprintf("pgrep -x %q", "good"), mockResult{stdout: "100\n"})

	p := NewHealthProber(HealthProberConfig{Executor: exec})
	sh := p.ProbeService(context.Background(), "host1", []ServiceSpec{
		{Name: "good"},
		{Name: "bad"},
	})
	require.Len(t, sh.Services, 2)
	assert.Equal(t, StatusUnhealthy, sh.Status)
}

func TestProbeServiceMultipleAllHealthy(t *testing.T) {
	exec := newMockExecutor()
	exec.set(fmt.Sprintf("pgrep -x %q", "svc1"), mockResult{stdout: "100\n"})
	exec.set(fmt.Sprintf("pgrep -x %q", "svc2"), mockResult{stdout: "200\n"})
	exec.set(fmt.Sprintf("pgrep -x %q", "svc3"), mockResult{stdout: "300\n"})

	p := NewHealthProber(HealthProberConfig{Executor: exec})
	sh := p.ProbeService(context.Background(), "host1", []ServiceSpec{
		{Name: "svc1"},
		{Name: "svc2"},
		{Name: "svc3"},
	})
	require.Len(t, sh.Services, 3)
	assert.Equal(t, StatusHealthy, sh.Status)
	for _, s := range sh.Services {
		assert.True(t, s.Running)
	}
}

func TestProbeServicePortClosedButProcessRunning(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	exec := newMockExecutor()
	exec.set(fmt.Sprintf("pgrep -x %q", "svc"), mockResult{stdout: "100\n"})

	p := NewHealthProber(HealthProberConfig{
		Executor:    exec,
		DialTimeout: 200 * time.Millisecond,
		HTTPTimeout: 200 * time.Millisecond,
	})
	sh := p.ProbeService(context.Background(), "127.0.0.1", []ServiceSpec{
		{Name: "svc", Port: port},
	})
	require.Len(t, sh.Services, 1)
	assert.Equal(t, StatusUnhealthy, sh.Services[0].Status)
}

func TestProbeServiceNoPort(t *testing.T) {
	exec := newMockExecutor()
	exec.set(fmt.Sprintf("pgrep -x %q", "svc"), mockResult{stdout: "100\n"})

	p := NewHealthProber(HealthProberConfig{Executor: exec})
	sh := p.ProbeService(context.Background(), "host1", []ServiceSpec{
		{Name: "svc"},
	})
	require.Len(t, sh.Services, 1)
	assert.True(t, sh.Services[0].Running)
	assert.Equal(t, StatusHealthy, sh.Services[0].Status)
	assert.Empty(t, sh.Services[0].HTTP.URL)
}

func TestProbeServiceContextCancelled(t *testing.T) {
	exec := newMockExecutor()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p := NewHealthProber(HealthProberConfig{Executor: exec})
	sh := p.ProbeService(ctx, "host1", []ServiceSpec{{Name: "svc"}})
	require.Len(t, sh.Services, 1)
	assert.False(t, sh.Services[0].Running)
}

func TestProbeServiceNilExecutor(t *testing.T) {
	p := NewHealthProber(HealthProberConfig{})
	sh := p.ProbeService(context.Background(), "host1", []ServiceSpec{{Name: "svc"}})
	require.Len(t, sh.Services, 1)
	assert.False(t, sh.Services[0].Running)
	assert.Equal(t, StatusUnhealthy, sh.Status)
}

func TestProbeProcessWindowsNotFound(t *testing.T) {
	exec := newMockExecutor()

	p := NewHealthProber(HealthProberConfig{Executor: exec, Runtime: RuntimeWindows})
	sh := p.ProbeService(context.Background(), "host1", []ServiceSpec{
		{Name: "missing.exe"},
	})
	require.Len(t, sh.Services, 1)
	assert.False(t, sh.Services[0].Running)
	assert.Equal(t, StatusUnhealthy, sh.Status)
}

func TestProbeProcessWindowsInfoMessage(t *testing.T) {
	exec := newMockExecutor()
	exec.set(`tasklist /FI "IMAGENAME eq missing.exe" /NH`, mockResult{
		stdout:   "INFO: No tasks are running which match the specified criteria.\n",
		exitCode: 0,
	})

	p := NewHealthProber(HealthProberConfig{Executor: exec, Runtime: RuntimeWindows})
	sh := p.ProbeService(context.Background(), "host1", []ServiceSpec{
		{Name: "missing.exe"},
	})
	require.Len(t, sh.Services, 1)
	assert.False(t, sh.Services[0].Running)
}

// --- ProbeData -------------------------------------------------------------

func TestProbeDataEmptyTarget(t *testing.T) {
	p := NewHealthProber(HealthProberConfig{Executor: newMockExecutor()})
	dh := p.ProbeData(context.Background(), "")
	assert.Equal(t, StatusUnknown, dh.Status)
	assert.Contains(t, dh.Err, "empty target")
}

func TestProbeDataNilExecutor(t *testing.T) {
	p := NewHealthProber(HealthProberConfig{})
	dh := p.ProbeData(context.Background(), "host1")
	assert.Equal(t, StatusUnknown, dh.Status)
	assert.Contains(t, dh.Err, "nil executor")
}

func TestProbeDataHealthy(t *testing.T) {
	exec := newMockExecutor()
	exec.set(`mysql -e "SELECT 1"`, mockResult{stdout: "1\n"})
	exec.set(`mysql -e "SHOW SLAVE STATUS\G"`, mockResult{stdout: sampleSlaveStatus(0)})
	exec.set("df -B1", mockResult{stdout: sampleDfOutput()})

	p := NewHealthProber(HealthProberConfig{Executor: exec})
	dh := p.ProbeData(context.Background(), "host1")
	assert.Equal(t, StatusHealthy, dh.Status)
	assert.True(t, dh.DBConnection.Connected)
	assert.True(t, dh.ReplicationLag.Healthy)
	assert.InDelta(t, 0, dh.ReplicationLag.LagSeconds, 0.001)
	require.Len(t, dh.Disks, 2)
}

func TestProbeDataDBConnectionFailed(t *testing.T) {
	exec := newMockExecutor()
	exec.set(`mysql -e "SELECT 1"`, mockResult{exitCode: 1, stderr: "connection refused"})
	exec.set(`mysql -e "SHOW SLAVE STATUS\G"`, mockResult{stdout: sampleSlaveStatus(0)})
	exec.set("df -B1", mockResult{stdout: sampleDfOutput()})

	p := NewHealthProber(HealthProberConfig{Executor: exec})
	dh := p.ProbeData(context.Background(), "host1")
	assert.Equal(t, StatusUnhealthy, dh.Status)
	assert.False(t, dh.DBConnection.Connected)
	assert.NotEmpty(t, dh.DBConnection.Err)
}

func TestProbeDataDBTransportError(t *testing.T) {
	exec := newMockExecutor()
	exec.set(`mysql -e "SELECT 1"`, mockResult{err: errors.New("ssh: connection dropped")})
	exec.set(`mysql -e "SHOW SLAVE STATUS\G"`, mockResult{stdout: sampleSlaveStatus(0)})
	exec.set("df -B1", mockResult{stdout: sampleDfOutput()})

	p := NewHealthProber(HealthProberConfig{Executor: exec})
	dh := p.ProbeData(context.Background(), "host1")
	assert.False(t, dh.DBConnection.Connected)
	assert.Contains(t, dh.DBConnection.Err, "connection dropped")
}

func TestProbeDataReplicationDegraded(t *testing.T) {
	exec := newMockExecutor()
	exec.set(`mysql -e "SELECT 1"`, mockResult{stdout: "1\n"})
	exec.set(`mysql -e "SHOW SLAVE STATUS\G"`, mockResult{stdout: sampleSlaveStatus(45)})
	exec.set("df -B1", mockResult{stdout: sampleDfOutput()})

	p := NewHealthProber(HealthProberConfig{Executor: exec})
	dh := p.ProbeData(context.Background(), "host1")
	assert.Equal(t, StatusDegraded, dh.Status)
	assert.InDelta(t, 45, dh.ReplicationLag.LagSeconds, 0.001)
}

func TestProbeDataReplicationCritical(t *testing.T) {
	exec := newMockExecutor()
	exec.set(`mysql -e "SELECT 1"`, mockResult{stdout: "1\n"})
	exec.set(`mysql -e "SHOW SLAVE STATUS\G"`, mockResult{stdout: sampleSlaveStatus(500)})
	exec.set("df -B1", mockResult{stdout: sampleDfOutput()})

	p := NewHealthProber(HealthProberConfig{Executor: exec})
	dh := p.ProbeData(context.Background(), "host1")
	assert.Equal(t, StatusUnhealthy, dh.Status)
	assert.InDelta(t, 500, dh.ReplicationLag.LagSeconds, 0.001)
}

func TestProbeDataReplicationNull(t *testing.T) {
	exec := newMockExecutor()
	exec.set(`mysql -e "SELECT 1"`, mockResult{stdout: "1\n"})
	exec.set(`mysql -e "SHOW SLAVE STATUS\G"`, mockResult{stdout: sampleSlaveStatusNull()})
	exec.set("df -B1", mockResult{stdout: sampleDfOutput()})

	p := NewHealthProber(HealthProberConfig{Executor: exec})
	dh := p.ProbeData(context.Background(), "host1")
	assert.False(t, dh.ReplicationLag.Healthy)
	assert.Equal(t, -1.0, dh.ReplicationLag.LagSeconds)
	assert.Equal(t, StatusDegraded, dh.Status)
}

func TestProbeDataReplicationNotFound(t *testing.T) {
	exec := newMockExecutor()
	exec.set(`mysql -e "SELECT 1"`, mockResult{stdout: "1\n"})
	exec.set(`mysql -e "SHOW SLAVE STATUS\G"`, mockResult{stdout: "Empty set\n"})
	exec.set("df -B1", mockResult{stdout: sampleDfOutput()})

	p := NewHealthProber(HealthProberConfig{Executor: exec})
	dh := p.ProbeData(context.Background(), "host1")
	assert.Equal(t, StatusDegraded, dh.Status)
}

func TestProbeDataReplicationExitError(t *testing.T) {
	exec := newMockExecutor()
	exec.set(`mysql -e "SELECT 1"`, mockResult{stdout: "1\n"})
	exec.set(`mysql -e "SHOW SLAVE STATUS\G"`, mockResult{exitCode: 1, stderr: "error"})
	exec.set("df -B1", mockResult{stdout: sampleDfOutput()})

	p := NewHealthProber(HealthProberConfig{Executor: exec})
	dh := p.ProbeData(context.Background(), "host1")
	assert.NotEmpty(t, dh.ReplicationLag.Err)
	assert.Contains(t, dh.ReplicationLag.Err, "exit code 1")
}

func TestProbeDataReplicationTransportError(t *testing.T) {
	exec := newMockExecutor()
	exec.set(`mysql -e "SELECT 1"`, mockResult{stdout: "1\n"})
	exec.set(`mysql -e "SHOW SLAVE STATUS\G"`, mockResult{err: errors.New("connection lost")})
	exec.set("df -B1", mockResult{stdout: sampleDfOutput()})

	p := NewHealthProber(HealthProberConfig{Executor: exec})
	dh := p.ProbeData(context.Background(), "host1")
	assert.Contains(t, dh.ReplicationLag.Err, "connection lost")
	assert.Equal(t, StatusDegraded, dh.Status)
}

func TestProbeDataAllFail(t *testing.T) {
	exec := newMockExecutor()
	p := NewHealthProber(HealthProberConfig{Executor: exec})
	dh := p.ProbeData(context.Background(), "host1")
	assert.Equal(t, StatusUnknown, dh.Status)
	assert.Contains(t, dh.Err, "all data probes failed")
}

func TestProbeDataPartialFailure(t *testing.T) {
	exec := newMockExecutor()
	exec.set(`mysql -e "SELECT 1"`, mockResult{stdout: "1\n"})

	p := NewHealthProber(HealthProberConfig{Executor: exec})
	dh := p.ProbeData(context.Background(), "host1")
	assert.True(t, dh.DBConnection.Connected)
	assert.Equal(t, StatusDegraded, dh.Status)
}

func TestProbeDataWindowsDisk(t *testing.T) {
	exec := newMockExecutor()
	exec.set(`mysql -e "SELECT 1"`, mockResult{stdout: "1\n"})
	exec.set(`mysql -e "SHOW SLAVE STATUS\G"`, mockResult{stdout: sampleSlaveStatus(0)})
	exec.set("wmic logicaldisk get caption,freespace,size /value",
		mockResult{stdout: sampleWmicDiskOutput()})

	p := NewHealthProber(HealthProberConfig{Executor: exec, Runtime: RuntimeWindows})
	dh := p.ProbeData(context.Background(), "host1")
	assert.True(t, dh.DBConnection.Connected)
	require.Len(t, dh.Disks, 1)
	assert.Equal(t, "C:", dh.Disks[0].Mount)
}

func TestProbeDataWindowsAllFail(t *testing.T) {
	exec := newMockExecutor()
	p := NewHealthProber(HealthProberConfig{Executor: exec, Runtime: RuntimeWindows})
	dh := p.ProbeData(context.Background(), "host1")
	assert.Equal(t, StatusUnknown, dh.Status)
}

func TestProbeDataCustomCommands(t *testing.T) {
	exec := newMockExecutor()
	exec.set("pg_isready", mockResult{stdout: "accepting connections\n"})
	exec.set("psql -c 'SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn) FROM pg_stat_replication'",
		mockResult{stdout: "Seconds_Behind_Master: 5\n"})
	exec.set("df -B1", mockResult{stdout: sampleDfOutput()})

	p := NewHealthProber(HealthProberConfig{
		Executor:           exec,
		DBCheckCommand:     "pg_isready",
		ReplicationCommand: "psql -c 'SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn) FROM pg_stat_replication'",
	})
	dh := p.ProbeData(context.Background(), "host1")
	assert.True(t, dh.DBConnection.Connected)
	assert.True(t, dh.ReplicationLag.Healthy)
	assert.InDelta(t, 5, dh.ReplicationLag.LagSeconds, 0.001)
}

// --- ProbeAll --------------------------------------------------------------

func TestProbeAllAggregation(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	srvPort := srv.Listener.Addr().(*net.TCPAddr).Port

	exec := newMockExecutor()
	exec.set("top -bn1", mockResult{stdout: sampleTopOutput()})
	exec.set("nproc", mockResult{stdout: "4\n"})
	exec.set("free -b", mockResult{stdout: sampleFreeOutput()})
	exec.set("df -B1", mockResult{stdout: sampleDfOutput()})
	exec.set("cat /proc/loadavg", mockResult{stdout: "0.1 0.1 0.1 1/100 12345\n"})
	exec.set("cat /proc/uptime", mockResult{stdout: "3600.0 7200.0\n"})
	exec.set(fmt.Sprintf("pgrep -x %q", "web"), mockResult{stdout: "200\n"})
	exec.set(`mysql -e "SELECT 1"`, mockResult{stdout: "1\n"})
	exec.set(`mysql -e "SHOW SLAVE STATUS\G"`, mockResult{stdout: sampleSlaveStatus(0)})

	p := NewHealthProber(HealthProberConfig{
		Executor:        exec,
		PingPort:        port,
		DefaultPorts:    []int{port},
		DefaultServices: []ServiceSpec{{Name: "web", Port: srvPort}},
		DialTimeout:     time.Second,
	})
	report := p.ProbeAll(context.Background(), "127.0.0.1")
	assert.Equal(t, "127.0.0.1", report.Target)
	assert.Equal(t, StatusHealthy, report.Status)
	assert.Equal(t, StatusHealthy, report.Network.Status)
	assert.Equal(t, StatusHealthy, report.Node.Status)
	assert.Equal(t, StatusHealthy, report.Service.Status)
	assert.Equal(t, StatusHealthy, report.Data.Status)
	assert.Contains(t, report.Summary, "network=healthy")
	assert.False(t, report.ProbedAt.IsZero())
	assert.Positive(t, report.Duration)
}

func TestProbeAllWithUnhealthyNode(t *testing.T) {
	exec := newMockExecutor()
	exec.set("top -bn1", mockResult{stdout: sampleTopCriticalCPU()})
	exec.set("nproc", mockResult{stdout: "2\n"})
	exec.set("free -b", mockResult{stdout: sampleFreeOutput()})
	exec.set("df -B1", mockResult{stdout: sampleDfOutput()})
	exec.set("cat /proc/loadavg", mockResult{stdout: "1.0 1.0 1.0 1/100 12345\n"})
	exec.set("cat /proc/uptime", mockResult{stdout: "100.0 200.0\n"})
	exec.set(`mysql -e "SELECT 1"`, mockResult{stdout: "1\n"})
	exec.set(`mysql -e "SHOW SLAVE STATUS\G"`, mockResult{stdout: sampleSlaveStatus(0)})

	p := NewHealthProber(HealthProberConfig{
		Executor:     exec,
		PingPort:     -1,
		DefaultPorts: nil,
	})
	report := p.ProbeAll(context.Background(), "127.0.0.1")
	assert.Equal(t, StatusUnhealthy, report.Status)
	assert.Equal(t, StatusUnhealthy, report.Node.Status)
}

func TestProbeAllCancelledContext(t *testing.T) {
	exec := newMockExecutor()
	exec.set("top -bn1", mockResult{stdout: sampleTopOutput()})
	exec.set("nproc", mockResult{stdout: "4\n"})
	exec.set("free -b", mockResult{stdout: sampleFreeOutput()})
	exec.set("df -B1", mockResult{stdout: sampleDfOutput()})
	exec.set("cat /proc/loadavg", mockResult{stdout: "0.1 0.1 0.1 1/100 12345\n"})
	exec.set("cat /proc/uptime", mockResult{stdout: "100.0 200.0\n"})
	exec.set(`mysql -e "SELECT 1"`, mockResult{stdout: "1\n"})
	exec.set(`mysql -e "SHOW SLAVE STATUS\G"`, mockResult{stdout: sampleSlaveStatus(0)})

	ctx, cancel := context.WithTimeout(context.Background(), 0)
	cancel()

	p := NewHealthProber(HealthProberConfig{Executor: exec, PingPort: -1})
	report := p.ProbeAll(ctx, "127.0.0.1")
	assert.NotEmpty(t, report.Target)
}

func TestProbeAllEmptyTarget(t *testing.T) {
	p := NewHealthProber(HealthProberConfig{Executor: newMockExecutor(), PingPort: -1})
	report := p.ProbeAll(context.Background(), "")
	assert.Equal(t, "", report.Target)
	assert.Equal(t, StatusUnknown, report.Network.Status)
	assert.Equal(t, StatusUnknown, report.Node.Status)
	assert.Equal(t, StatusUnknown, report.Service.Status)
	assert.Equal(t, StatusUnknown, report.Data.Status)
	assert.Equal(t, StatusUnknown, report.Status)
}

func TestProbeAllWithNilExecutor(t *testing.T) {
	p := NewHealthProber(HealthProberConfig{
		PingPort:     -1,
		DefaultPorts: nil,
	})
	report := p.ProbeAll(context.Background(), "127.0.0.1")
	assert.Equal(t, StatusHealthy, report.Network.Status)
	assert.Equal(t, StatusUnknown, report.Node.Status)
	assert.Equal(t, StatusUnknown, report.Data.Status)
}

func TestProbeAllSummary(t *testing.T) {
	exec := newMockExecutor()
	exec.set("top -bn1", mockResult{stdout: sampleTopOutput()})
	exec.set("nproc", mockResult{stdout: "4\n"})
	exec.set("free -b", mockResult{stdout: sampleFreeOutput()})
	exec.set("df -B1", mockResult{stdout: sampleDfOutput()})
	exec.set("cat /proc/loadavg", mockResult{stdout: "0.1 0.1 0.1 1/100 12345\n"})
	exec.set("cat /proc/uptime", mockResult{stdout: "100.0 200.0\n"})
	exec.set(`mysql -e "SELECT 1"`, mockResult{stdout: "1\n"})
	exec.set(`mysql -e "SHOW SLAVE STATUS\G"`, mockResult{stdout: sampleSlaveStatus(0)})

	p := NewHealthProber(HealthProberConfig{Executor: exec, PingPort: -1})
	report := p.ProbeAll(context.Background(), "127.0.0.1")
	assert.Contains(t, report.Summary, "network=")
	assert.Contains(t, report.Summary, "node=")
	assert.Contains(t, report.Summary, "service=")
	assert.Contains(t, report.Summary, "data=")
	assert.True(t, strings.Contains(report.Summary, "node=healthy"))
}

// --- Parser unit tests -----------------------------------------------------

func TestParseLinuxCPU(t *testing.T) {
	cpu, err := parseLinuxCPU(sampleTopOutput())
	require.NoError(t, err)
	assert.InDelta(t, 8.5, cpu.UsagePercent, 0.1)
}

func TestParseLinuxCPUNotFound(t *testing.T) {
	_, err := parseLinuxCPU("no cpu info here")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "Cpu(s) line not found")
}

func TestParseLinuxCPUPercentFormat(t *testing.T) {
	out := `%Cpu(s):  5.0%us,  3.0%sy,  0.0%ni, 91.5%id,  0.5%wa,  0.0%hi,  0.0%si,  0.0%st`
	cpu, err := parseLinuxCPU(out)
	require.NoError(t, err)
	assert.InDelta(t, 8.5, cpu.UsagePercent, 0.1)
}

func TestParseLinuxCPUMultipleLines(t *testing.T) {
	out := `%Cpu(s):  5.0 us,  3.0 sy,  0.0 ni, 91.5 id,  0.0 wa
%Cpu(s): 50.0 us, 30.0 sy,  0.0 ni, 20.0 id,  0.0 wa
`
	cpu, err := parseLinuxCPU(out)
	require.NoError(t, err)
	assert.InDelta(t, 8.5, cpu.UsagePercent, 0.1)
}

func TestParseLinuxMemory(t *testing.T) {
	mem, err := parseLinuxMemory(sampleFreeOutput())
	require.NoError(t, err)
	assert.Equal(t, uint64(8589934592), mem.TotalBytes)
	assert.Equal(t, uint64(1073741824), mem.UsedBytes)
	assert.Equal(t, uint64(7516192768), mem.FreeBytes)
	assert.InDelta(t, 12.5, mem.UsagePercent, 0.1)
}

func TestParseLinuxMemoryNotFound(t *testing.T) {
	_, err := parseLinuxMemory("no memory info")
	assert.Error(t, err)
}

func TestParseLinuxMemoryMalformed(t *testing.T) {
	_, err := parseLinuxMemory("Mem: 123")
	assert.Error(t, err)
}

func TestParseLinuxDisk(t *testing.T) {
	disks, err := parseLinuxDisk(sampleDfOutput())
	require.NoError(t, err)
	require.Len(t, disks, 2)
	assert.Equal(t, "/dev/sda1", disks[0].Filesystem)
	assert.Equal(t, "/", disks[0].Mount)
	assert.Equal(t, uint64(10000000000), disks[0].TotalBytes)
	assert.InDelta(t, 50.0, disks[0].UsagePercent, 0.1)
	assert.Equal(t, "/dev/sda2", disks[1].Filesystem)
	assert.Equal(t, "/home", disks[1].Mount)
}

func TestParseLinuxDiskEmpty(t *testing.T) {
	_, err := parseLinuxDisk("Filesystem     1B-blocks      Used Available Use% Mounted on\n")
	assert.Error(t, err)
}

func TestParseLinuxDiskSkipsShortLines(t *testing.T) {
	out := `Filesystem     1B-blocks      Used Available Use% Mounted on
short line
/dev/sda1      10000000000 5000000000  4000000000  50% /
`
	disks, err := parseLinuxDisk(out)
	require.NoError(t, err)
	require.Len(t, disks, 1)
	assert.Equal(t, "/", disks[0].Mount)
}

func TestParseLinuxLoad(t *testing.T) {
	load, err := parseLinuxLoad("0.52 0.58 0.59 1/123 4567\n")
	require.NoError(t, err)
	assert.InDelta(t, 0.52, load.Load1, 0.001)
	assert.InDelta(t, 0.58, load.Load5, 0.001)
	assert.InDelta(t, 0.59, load.Load15, 0.001)
}

func TestParseLinuxLoadMalformed(t *testing.T) {
	_, err := parseLinuxLoad("two fields")
	assert.Error(t, err)
}

func TestParseLinuxUptime(t *testing.T) {
	up, err := parseLinuxUptime("3600.50 7200.00\n")
	require.NoError(t, err)
	assert.Equal(t, time.Duration(3600.50*float64(time.Second)), up)
}

func TestParseLinuxUptimeMalformed(t *testing.T) {
	_, err := parseLinuxUptime("")
	assert.Error(t, err)
}

func TestParseWindowsCPU(t *testing.T) {
	cpu, err := parseWindowsCPU("\nLoadPercentage=42\n\n")
	require.NoError(t, err)
	assert.InDelta(t, 42.0, cpu.UsagePercent, 0.1)
}

func TestParseWindowsCPUNotFound(t *testing.T) {
	_, err := parseWindowsCPU("no data")
	assert.Error(t, err)
}

func TestParseWindowsCores(t *testing.T) {
	assert.Equal(t, 4, parseWindowsCores("\nNumberOfCores=4\n\n"))
	assert.Equal(t, 8, parseWindowsCores("\nNumberOfCores=4\n\nNumberOfCores=4\n\n"))
	assert.Equal(t, 0, parseWindowsCores("no data"))
}

func TestParseWindowsMemory(t *testing.T) {
	mem, err := parseWindowsMemory("\nFreePhysicalMemory=4000000\nTotalVisibleMemorySize=8000000\n\n")
	require.NoError(t, err)
	assert.Equal(t, uint64(8000000*1024), mem.TotalBytes)
	assert.Equal(t, uint64(4000000*1024), mem.FreeBytes)
	assert.Equal(t, uint64(4000000*1024), mem.UsedBytes)
	assert.InDelta(t, 50.0, mem.UsagePercent, 0.1)
}

func TestParseWindowsMemoryNotFound(t *testing.T) {
	_, err := parseWindowsMemory("no data")
	assert.Error(t, err)
}

func TestParseWindowsDisk(t *testing.T) {
	disks, err := parseWindowsDisk(sampleWmicDiskOutput())
	require.NoError(t, err)
	require.Len(t, disks, 1)
	assert.Equal(t, "C:", disks[0].Filesystem)
	assert.Equal(t, uint64(100000000000), disks[0].TotalBytes)
	assert.Equal(t, uint64(50000000000), disks[0].FreeBytes)
	assert.InDelta(t, 50.0, disks[0].UsagePercent, 0.1)
}

func TestParseWindowsDiskEmpty(t *testing.T) {
	_, err := parseWindowsDisk("no disks")
	assert.Error(t, err)
}

func TestParseReplicationLag(t *testing.T) {
	info := parseReplicationLag(sampleSlaveStatus(42))
	assert.True(t, info.Healthy)
	assert.InDelta(t, 42, info.LagSeconds, 0.001)
	assert.Empty(t, info.Err)
}

func TestParseReplicationLagNull(t *testing.T) {
	info := parseReplicationLag(sampleSlaveStatusNull())
	assert.False(t, info.Healthy)
	assert.Equal(t, -1.0, info.LagSeconds)
	assert.Contains(t, info.Err, "not running")
}

func TestParseReplicationLagNotFound(t *testing.T) {
	info := parseReplicationLag("Empty set")
	assert.NotEmpty(t, info.Err)
	assert.Contains(t, info.Err, "not found")
}

// --- withTimeout -----------------------------------------------------------

func TestWithTimeoutExistingDeadline(t *testing.T) {
	p := NewHealthProber(HealthProberConfig{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	derived, derivedCancel := p.withTimeout(ctx, 10*time.Second)
	defer derivedCancel()
	dl1, _ := ctx.Deadline()
	dl2, _ := derived.Deadline()
	assert.Equal(t, dl1, dl2, "should preserve existing deadline")
}

func TestWithTimeoutNoDeadline(t *testing.T) {
	p := NewHealthProber(HealthProberConfig{})
	derived, cancel := p.withTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, ok := derived.Deadline()
	assert.True(t, ok, "should add a deadline")
}

// --- Sample data -----------------------------------------------------------

func sampleTopOutput() string {
	return `top - 12:34:56 up  1:23,  2 users,  load average: 0.10, 0.20, 0.30
Tasks: 123 total,   1 running, 122 sleeping,   0 stopped,   0 zombie
%Cpu(s):  5.0 us,  3.0 sy,  0.5 ni, 91.5 id,  0.0 wa,  0.0 hi,  0.0 si,  0.0 st
MiB Mem :   8192.0 total,   7168.0 free,   1024.0 used,    0.0 buff/cache
MiB Swap:   1024.0 total,   1024.0 free,      0.0 used,   7168.0 avail Mem

  PID USER      PR  NI    VIRT    RES    SHR S  %CPU  %MEM     TIME+ COMMAND
    1 root      20   0   10240   1024    512 S   0.0   0.0   0:00.10 systemd
`
}

func sampleTopHighCPU() string {
	return `top - 12:34:56 up  1:23,  2 users,  load average: 1.0, 1.0, 1.0
Tasks: 123 total,   1 running, 122 sleeping,   0 stopped,   0 zombie
%Cpu(s): 80.0 us,  5.0 sy,  0.0 ni, 15.0 id,  0.0 wa,  0.0 hi,  0.0 si,  0.0 st
`
}

func sampleTopCriticalCPU() string {
	return `top - 12:34:56 up  1:23,  2 users,  load average: 2.0, 2.0, 2.0
Tasks: 123 total,   1 running, 122 sleeping,   0 stopped,   0 zombie
%Cpu(s): 90.0 us,  7.0 sy,  0.0 ni,  3.0 id,  0.0 wa,  0.0 hi,  0.0 si,  0.0 st
`
}

func sampleFreeOutput() string {
	return `              total        used        free      shared  buff/cache   available
Mem:   8589934592  1073741824  7516192768    1048576   1073741824  6442450944
Swap:  1073741824           0  1073741824
`
}

func sampleFreeCriticalMem() string {
	return `              total        used        free      shared  buff/cache   available
Mem:   8589934592  8246337208   343597384    1048576    104857600   343597384
Swap:  1073741824           0  1073741824
`
}

func sampleDfOutput() string {
	return `Filesystem     1B-blocks      Used Available Use% Mounted on
/dev/sda1      10000000000 5000000000  4000000000  50% /
/dev/sda2      20000000000 10000000000 9000000000  53% /home
`
}

func sampleDfDegradedDisk() string {
	return `Filesystem     1B-blocks      Used Available Use% Mounted on
/dev/sda1      10000000000 8500000000  1000000000  85% /
`
}

func sampleWmicDiskOutput() string {
	return "\nCaption=C:\nFreeSpace=50000000000\nSize=100000000000\n\n"
}

func sampleSlaveStatus(lag int) string {
	return fmt.Sprintf(`*************************** 1. row ***************************
             Slave_IO_State: Waiting for master to send event
                Master_Host: 10.0.0.1
                Master_User: repl
                  Master_Log_File: mysql-bin.000123
             Read_Master_Log_Pos: 123456
              Relay_Log_File: relay-bin.000456
               Relay_Log_Pos: 123456
       Relay_Master_Log_File: mysql-bin.000123
            Slave_IO_Running: Yes
           Slave_SQL_Running: Yes
        Seconds_Behind_Master: %d
Master_SSL_Allowed: No
`, lag)
}

func sampleSlaveStatusNull() string {
	return `*************************** 1. row ***************************
             Slave_IO_State: Connecting to master
            Slave_IO_Running: Connecting
           Slave_SQL_Running: Yes
        Seconds_Behind_Master: NULL
`
}
