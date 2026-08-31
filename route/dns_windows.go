package route

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// DNSManager backs up and clears/restores physical adapter DNS servers and
// interface metrics on Windows so that all DNS queries are forced through the
// TUN interface.
type DNSManager struct {
	backupPath string
}

// InterfaceConfig holds the DNS and metric configuration we need to restore.
type InterfaceConfig struct {
	InterfaceIndex int        `json:"InterfaceIndex"`
	InterfaceAlias string     `json:"InterfaceAlias"`
	IPv4Servers    stringList `json:"IPv4Servers,omitempty"`
	IPv6Servers    stringList `json:"IPv6Servers,omitempty"`
	IPv4Metric     int        `json:"IPv4Metric"`
	IPv6Metric     int        `json:"IPv6Metric"`
	IsTUN          bool       `json:"IsTUN"`
}

// stringList accepts JSON strings, arrays of strings, null, or empty objects
// and normalizes them to a string slice. PowerShell's ConvertTo-Json collapses
// single-element arrays into plain strings and empty arrays into empty objects.
type stringList []string

func (s *stringList) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))

	switch {
	case trimmed == "null" || trimmed == "" || trimmed == "{}":
		*s = nil
		return nil
	case strings.HasPrefix(trimmed, `"`):
		var single string
		if err := json.Unmarshal(data, &single); err != nil {
			return err
		}
		*s = []string{single}
		return nil
	case strings.HasPrefix(trimmed, "["):
		var arr []string
		if err := json.Unmarshal(data, &arr); err != nil {
			return err
		}
		*s = arr
		return nil
	default:
		var obj map[string]interface{}
		if err := json.Unmarshal(data, &obj); err == nil && len(obj) == 0 {
			*s = nil
			return nil
		}
		return fmt.Errorf("invalid string list: %s", trimmed)
	}
}

func (s stringList) MarshalJSON() ([]byte, error) {
	if s == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]string(s))
}

// NewDNSManager creates a manager that persists backups to backupPath.
func NewDNSManager(backupPath string) *DNSManager {
	return &DNSManager{backupPath: backupPath}
}

// RecoverIfNeeded restores DNS settings if a backup file exists. The company
// network uses static IP/MAC binding, so the backed-up values are always the
// correct ones for these adapters: existing adapters get restored
// unconditionally, missing adapters are skipped (a different network will
// configure them via DHCP anyway). The backup file is kept permanently.
func (d *DNSManager) RecoverIfNeeded() (bool, error) {
	if _, err := os.Stat(d.backupPath); err != nil {
		return false, nil
	}
	if err := d.Restore(); err != nil {
		return true, err
	}
	return true, nil
}

// BackupAndClear saves the current DNS configuration of all non-tun adapters
// and then clears their DNS server addresses. It also lowers the TUN metric
// and raises physical adapter metrics so Windows prefers TUN. All operations
// are batched into a single PowerShell invocation to keep connect fast.
func (d *DNSManager) BackupAndClear() error {
	interfaces, err := d.listConfig()
	if err != nil {
		return fmt.Errorf("list config: %w", err)
	}

	if len(interfaces) == 0 {
		return nil
	}

	backup, err := json.MarshalIndent(interfaces, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal backup: %w", err)
	}
	if err := os.WriteFile(d.backupPath, backup, 0644); err != nil {
		return fmt.Errorf("write backup: %w", err)
	}

	var sb strings.Builder
	sb.WriteString("[Console]::OutputEncoding=[System.Text.Encoding]::UTF8\n")

	for _, iface := range interfaces {
		idx := iface.InterfaceIndex

		if iface.IsTUN {
			// Make TUN the most preferred interface.
			fmt.Fprintf(&sb, "try { Set-NetIPInterface -InterfaceIndex %d -AddressFamily IPv4 -InterfaceMetric 5 -ErrorAction Stop } catch {}\n", idx)
			fmt.Fprintf(&sb, "try { Set-NetIPInterface -InterfaceIndex %d -AddressFamily IPv6 -InterfaceMetric 5 -ErrorAction Stop } catch {}\n", idx)
			continue
		}

		// Loopback pseudo interface does not accept DNS/metric changes.
		if idx == 1 || strings.HasPrefix(strings.ToLower(iface.InterfaceAlias), "loopback") {
			continue
		}

		if len(iface.IPv4Servers) > 0 {
			// Setting an empty DNS list is a no-op on Windows; override with a
			// loopback blackhole instead so queries can never leave the machine.
			fmt.Fprintf(&sb, "try { Set-DnsClientServerAddress -InterfaceIndex %d -ServerAddresses '127.0.0.1' -ErrorAction Stop } catch { Write-Output \"blackhole dns ifindex=%d: $($_.Exception.Message)\" }\n", idx, idx)
		}
		if len(iface.IPv6Servers) > 0 {
			fmt.Fprintf(&sb, "try { Set-DnsClientServerAddress -InterfaceIndex %d -ServerAddresses '::1' -ErrorAction Stop } catch { Write-Output \"blackhole dns6 ifindex=%d: $($_.Exception.Message)\" }\n", idx, idx)
		}
		// Raise metric so Windows resolver prefers TUN over this adapter.
		fmt.Fprintf(&sb, "try { Set-NetIPInterface -InterfaceIndex %d -AddressFamily IPv4 -InterfaceMetric 50 -ErrorAction Stop } catch { Write-Output \"set metric ifindex=%d v4: $($_.Exception.Message)\" }\n", idx, idx)
		fmt.Fprintf(&sb, "try { Set-NetIPInterface -InterfaceIndex %d -AddressFamily IPv6 -InterfaceMetric 50 -ErrorAction Stop } catch { Write-Output \"set metric ifindex=%d v6: $($_.Exception.Message)\" }\n", idx, idx)
	}

	out, err := runPowerShell(sb.String())
	if err != nil {
		return fmt.Errorf("apply config: %w: %s", err, strings.TrimSpace(out))
	}
	if trimmed := strings.TrimSpace(out); trimmed != "" {
		fmt.Fprintln(os.Stderr, trimmed)
	}
	return nil
}

// Restore re-applies the previously saved DNS configuration.
func (d *DNSManager) Restore() error {
	data, err := os.ReadFile(d.backupPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read backup: %w", err)
	}

	var interfaces []InterfaceConfig
	if err := json.Unmarshal(data, &interfaces); err != nil {
		return fmt.Errorf("parse backup: %w", err)
	}

	var sb strings.Builder
	sb.WriteString("[Console]::OutputEncoding=[System.Text.Encoding]::UTF8\n")

	for _, iface := range interfaces {
		idx := iface.InterfaceIndex
		// Skip the TUN adapter: at disconnect time sing-box has already been
		// stopped and the adapter is gone, so there is nothing to restore.
		if iface.IsTUN || idx <= 0 || idx == 1 || strings.HasPrefix(strings.ToLower(iface.InterfaceAlias), "loopback") {
			continue
		}
		if len(iface.IPv4Servers) > 0 {
			fmt.Fprintf(&sb, "try { Set-DnsClientServerAddress -InterfaceIndex %d -ServerAddresses %s -ErrorAction Stop } catch { Write-Output \"restore dns ifindex=%d: $($_.Exception.Message)\" }\n",
				idx, psStringArray(iface.IPv4Servers), idx)
		}
		if len(iface.IPv6Servers) > 0 {
			fmt.Fprintf(&sb, "try { Set-DnsClientServerAddress -InterfaceIndex %d -ServerAddresses %s -ErrorAction Stop } catch { Write-Output \"restore dns6 ifindex=%d: $($_.Exception.Message)\" }\n",
				idx, psStringArray(iface.IPv6Servers), idx)
		}
		if iface.IPv4Metric > 0 {
			fmt.Fprintf(&sb, "try { Set-NetIPInterface -InterfaceIndex %d -AddressFamily IPv4 -InterfaceMetric %d -ErrorAction Stop } catch {}\n", idx, iface.IPv4Metric)
		}
		if iface.IPv6Metric > 0 {
			fmt.Fprintf(&sb, "try { Set-NetIPInterface -InterfaceIndex %d -AddressFamily IPv6 -InterfaceMetric %d -ErrorAction Stop } catch {}\n", idx, iface.IPv6Metric)
		}
	}

	out, err := runPowerShell(sb.String())
	if err != nil {
		fmt.Fprintf(os.Stderr, "restore config: %v: %s\n", err, strings.TrimSpace(out))
	} else if trimmed := strings.TrimSpace(out); trimmed != "" {
		fmt.Fprintln(os.Stderr, trimmed)
	}

	// The backup file is kept permanently: the company network uses static
	// IP/MAC binding, so the backed-up values stay correct for these adapters.
	return nil
}

// psStringArray renders a PowerShell string array literal.
func psStringArray(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return "@(" + strings.Join(quoted, ",") + ")"
}

// runPowerShell executes a PowerShell script and returns its combined output.
// Individual statements report their own failures via Write-Output, so the
// process is forced to exit 0 to keep the exit code meaningful.
func runPowerShell(script string) (string, error) {
	cmd := exec.Command("powershell", "-NoProfile", "-Command", script+"\nexit 0\n")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (d *DNSManager) listConfig() ([]InterfaceConfig, error) {
	ps := `
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
$v4 = Get-DnsClientServerAddress -AddressFamily IPv4 | Select-Object InterfaceIndex, InterfaceAlias, ServerAddresses
$v6 = Get-DnsClientServerAddress -AddressFamily IPv6 | Select-Object InterfaceIndex, InterfaceAlias, ServerAddresses
$metrics = Get-NetIPInterface | Select-Object InterfaceIndex, InterfaceAlias, AddressFamily, InterfaceMetric
$v4Dict = @{}
$v6Dict = @{}
foreach ($i in $v4) { if ($i.ServerAddresses) { $v4Dict[[int]$i.InterfaceIndex] = @($i.ServerAddresses) } }
foreach ($i in $v6) { if ($i.ServerAddresses) { $v6Dict[[int]$i.InterfaceIndex] = @($i.ServerAddresses) } }
$allIndexes = ($metrics.InterfaceIndex + $v4Dict.Keys + $v6Dict.Keys) | Sort-Object -Unique
$result = foreach ($idxRaw in $allIndexes) {
    $idx = [int]$idxRaw
    $alias = ($metrics | Where-Object { $_.InterfaceIndex -eq $idx } | Select-Object -First 1).InterfaceAlias
    if (-not $alias) { $alias = ($v4 | Where-Object { $_.InterfaceIndex -eq $idx } | Select-Object -First 1).InterfaceAlias }
    if (-not $alias) { $alias = ($v6 | Where-Object { $_.InterfaceIndex -eq $idx } | Select-Object -First 1).InterfaceAlias }
    $isTun = ($alias -eq 'tun0')
    $m4 = $metrics | Where-Object { $_.InterfaceIndex -eq $idx -and $_.AddressFamily -eq 'IPv4' } | Select-Object -First 1
    $m6 = $metrics | Where-Object { $_.InterfaceIndex -eq $idx -and $_.AddressFamily -eq 'IPv6' } | Select-Object -First 1
    [PSCustomObject]@{
        InterfaceIndex = $idx
        InterfaceAlias = $alias
        IPv4Servers = if ($v4Dict.ContainsKey($idx)) { $v4Dict[$idx] } else { @() }
        IPv6Servers = if ($v6Dict.ContainsKey($idx)) { $v6Dict[$idx] } else { @() }
        IPv4Metric = if ($m4) { $m4.InterfaceMetric } else { 0 }
        IPv6Metric = if ($m6) { $m6.InterfaceMetric } else { 0 }
        IsTUN = $isTun
    }
}
$result | ConvertTo-Json -Depth 3
`
	cmd := exec.Command("powershell", "-NoProfile", "-Command", ps)
	out, err := cmd.Output()
	if err != nil {
		if len(out) == 0 {
			return nil, fmt.Errorf("powershell: %w", err)
		}
	}

	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil, nil
	}

	var list []InterfaceConfig
	if err := json.Unmarshal(out, &list); err != nil {
		var single InterfaceConfig
		if err2 := json.Unmarshal(out, &single); err2 != nil {
			return nil, fmt.Errorf("parse config json: %w", err)
		}
		list = []InterfaceConfig{single}
	}
	return list, nil
}

func (d *DNSManager) setDNS(ifIndex int, family string, servers []string) error {
	var serverArg string
	if len(servers) == 0 {
		serverArg = "@()"
	} else {
		quoted := make([]string, len(servers))
		for i, s := range servers {
			quoted[i] = fmt.Sprintf("'%s'", strings.ReplaceAll(s, "'", "''"))
		}
		serverArg = "@" + strings.Join(quoted, ",")
	}

	// Set-DnsClientServerAddress has no -AddressFamily parameter; it infers the
	// family from the address format. Keep the family argument for logging only.
	_ = family
	ps := fmt.Sprintf(
		"[Console]::OutputEncoding=[System.Text.Encoding]::UTF8; "+
			"Set-DnsClientServerAddress -InterfaceIndex %d -ServerAddresses %s",
		ifIndex, serverArg)

	cmd := exec.Command("powershell", "-NoProfile", "-Command", ps)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (d *DNSManager) setMetric(ifIndex int, family string, metric int) error {
	ps := fmt.Sprintf(
		"[Console]::OutputEncoding=[System.Text.Encoding]::UTF8; "+
			"Set-NetIPInterface -InterfaceIndex %d -AddressFamily %s -InterfaceMetric %d",
		ifIndex, family, metric)
	cmd := exec.Command("powershell", "-NoProfile", "-Command", ps)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
