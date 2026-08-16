
package recommend

// defaults.go provides the built-in catalogue of historical incidents,
// runbooks and fix patterns that ship with LEVEE. They are loaded by
// NewKnowledgeBaseWithDefaults so that a fresh install has a useful
// recommendation set without requiring the operator to curate one.
//
// The catalogue covers the five most common operational emergencies:
//
//   1. Java OOM            -> restart the Java service
//   2. Disk full           -> clean logs + expand capacity
//   3. DB pool exhausted   -> tune the connection pool
//   4. Network partition   -> check network config + restart networking
//   5. Config drift        -> roll back config + redeploy
//
// The workflows are illustrative LEVEELang snippets; they are not executed
// by this package but are surfaced to the operator as suggestions.

import "time"

// defaultIncidents is the built-in set of historical incidents. The IDs are
// stable so that operator-supplied overrides can reference them.
var defaultIncidents = []HistoricalIncident{
	{
		ID:        "INC-JAVA-OOM-001",
		Title:     "Java service out-of-memory",
		Symptoms:  []string{"java.lang.OutOfMemoryError", "heap space", "GC overhead limit exceeded", "high RSS"},
		RootCause: "Java heap exhaustion due to memory leak or insufficient -Xmx",
		Resolution: "Restart the Java service with a larger -Xmx and capture a heap dump " +
			"on the next OOM for offline analysis.",
		Workflow:  "workflow java-restart { service = \"java-app\" }",
		Tags:      []string{"java", "oom", "memory", "jvm"},
		Severity:  "critical",
		CreatedAt: time.Date(2024, 1, 15, 9, 30, 0, 0, time.UTC),
		Occurrences: 12,
	},
	{
		ID:        "INC-DISK-FULL-001",
		Title:     "Disk full on log volume",
		Symptoms:  []string{"no space left on device", "disk usage above 90%", "write errors"},
		RootCause: "Log volume filled by unbounded application logs or rotated logs not being purged",
		Resolution: "Clean old rotated logs, raise log retention threshold, and provision additional " +
			"capacity on the log volume.",
		Workflow:  "workflow disk-cleanup { path = \"/var/log/app\" }",
		Tags:      []string{"disk", "storage", "logs", "capacity"},
		Severity:  "critical",
		CreatedAt: time.Date(2024, 2, 3, 14, 12, 0, 0, time.UTC),
		Occurrences: 8,
	},
	{
		ID:        "INC-DB-POOL-001",
		Title:     "Database connection pool exhausted",
		Symptoms:  []string{"connection pool exhausted", "unable to acquire connection", "request timeout"},
		RootCause: "Connection pool size too small for peak load or connections leaked by long-running transactions",
		Resolution: "Increase the pool size, verify connection release in all code paths, and review " +
			"long-running transactions.",
		Workflow:  "workflow db-pool-tune { pool = \"primary\" }",
		Tags:      []string{"database", "pool", "connection", "postgresql"},
		Severity:  "warning",
		CreatedAt: time.Date(2024, 3, 21, 8, 5, 0, 0, time.UTC),
		Occurrences: 5,
	},
	{
		ID:        "INC-NET-PARTITION-001",
		Title:     "Network partition between service tiers",
		Symptoms:  []string{"connection refused", "request timeout", "upstream unreachable"},
		RootCause: "Network partition or misconfigured firewall blocking traffic between service tiers",
		Resolution: "Verify network configuration and firewall rules, then restart the affected network " +
			"interfaces or services.",
		Workflow:  "workflow network-restart { tier = \"edge\" }",
		Tags:      []string{"network", "partition", "firewall", "connectivity"},
		Severity:  "critical",
		CreatedAt: time.Date(2024, 4, 10, 22, 45, 0, 0, time.UTC),
		Occurrences: 3,
	},
	{
		ID:        "INC-CONFIG-DRIFT-001",
		Title:     "Configuration drift detected",
		Symptoms:  []string{"config mismatch", "unexpected behaviour after deploy", "diff non-empty"},
		RootCause: "Manual config change on the host diverged from the declared baseline",
		Resolution: "Roll back the host configuration to the last known good baseline and redeploy.",
		Workflow:  "workflow config-rollback { baseline = \"last-good\" }",
		Tags:      []string{"config", "drift", "baseline", "deploy"},
		Severity:  "warning",
		CreatedAt: time.Date(2024, 5, 2, 11, 20, 0, 0, time.UTC),
		Occurrences: 4,
	},
}

// defaultRunbooks is the built-in set of operational runbooks. They are
// intentionally generic so they apply across services.
var defaultRunbooks = []Runbook{
	{
		ID:          "RB-DISK-FULL-001",
		Name:        "Disk Full Recovery",
		Description: "Recover a host whose root or log volume has filled up.",
		Trigger:     "disk usage above 90 percent or no space left on device",
		Steps: []RunbookStep{
			{Order: 1, Action: "identify-large-files", Command: "du -ah /var/log | sort -rh | head -20",
				Description: "Find the largest files on the log volume.", RiskLevel: "low"},
			{Order: 2, Action: "clean-old-logs", Command: "find /var/log -name '*.gz' -mtime +7 -delete",
				Description: "Delete rotated logs older than 7 days.", RiskLevel: "medium"},
			{Order: 3, Action: "restart-services", Command: "systemctl restart rsyslog",
				Description: "Restart the logging daemon so it reopens file handles.", RiskLevel: "medium"},
		},
		Tags: []string{"disk", "storage", "logs"},
	},
	{
		ID:          "RB-SERVICE-RESTART-001",
		Name:        "Service Restart",
		Description: "Restart a misbehaving service after capturing diagnostics.",
		Trigger:     "service unhealthy or high error rate",
		Steps: []RunbookStep{
			{Order: 1, Action: "capture-threads", Command: "jstack $(pidof java) > /tmp/threads.txt",
				Description: "Capture a thread dump for post-mortem analysis.", RiskLevel: "low"},
			{Order: 2, Action: "capture-heap", Command: "jmap -dump:format=b,file=/tmp/heap.bin $(pidof java)",
				Description: "Capture a heap dump for OOM analysis.", RiskLevel: "low"},
			{Order: 3, Action: "restart", Command: "systemctl restart ${service}",
				Description: "Restart the service. This causes a brief outage.", RiskLevel: "high"},
		},
		Tags: []string{"service", "restart", "java", "jvm"},
	},
	{
		ID:          "RB-NETWORK-CHECK-001",
		Name:        "Network Connectivity Check",
		Description: "Diagnose and recover from network partition between service tiers.",
		Trigger:     "connection refused or upstream unreachable or network partition",
		Steps: []RunbookStep{
			{Order: 1, Action: "ping-peers", Command: "ping -c 3 ${peer}",
				Description: "Verify L3 connectivity to the peer tier.", RiskLevel: "low"},
			{Order: 2, Action: "check-firewall", Command: "iptables -L -n",
				Description: "Inspect the local firewall rules.", RiskLevel: "low"},
			{Order: 3, Action: "restart-interface", Command: "systemctl restart networking",
				Description: "Restart networking. This drops all in-flight connections.", RiskLevel: "high"},
		},
		Tags: []string{"network", "partition", "firewall"},
	},
}

// defaultPatterns is the built-in set of fix patterns. The conditions are
// regular expressions matched against the root cause and symptoms.
var defaultPatterns = []FixPattern{
	{
		ID:        "FP-OOM-RESTART-001",
		Name:      "Restart on OOM",
		Condition: "(?i)outofmemory|oom|heap space",
		Fix:       "Restart the JVM service with -XX:+HeapDumpOnOutOfMemoryError",
		Workflow:  "workflow oom-restart { service = \"java-app\" }",
		RiskLevel: RiskHigh,
		Tags:      []string{"java", "oom", "memory"},
	},
	{
		ID:        "FP-DISK-CLEAN-001",
		Name:      "Clean disk and expand",
		Condition: "(?i)no space left|disk full|disk usage",
		Fix:       "Purge old logs and request capacity expansion",
		Workflow:  "workflow disk-clean { path = \"/var/log\" }",
		RiskLevel: RiskMedium,
		Tags:      []string{"disk", "storage", "logs"},
	},
	{
		ID:        "FP-DB-POOL-001",
		Name:      "Tune DB connection pool",
		Condition: "(?i)connection pool|pool exhausted|unable to acquire",
		Fix:       "Increase pool size and review connection leaks",
		Workflow:  "workflow db-pool-tune { pool = \"primary\" }",
		RiskLevel: RiskMedium,
		Tags:      []string{"database", "pool", "connection"},
	},
}