//go:build !linux

package sandbox

// CgroupLimits are cgroup v2 resource caps; a zero field means "no limit".
type CgroupLimits struct {
	MemoryMaxBytes  int64
	MemoryHighBytes int64
	PIDsMax         int64
	CPUQuotaUS      int64
	CPUPeriodUS     int64
}

// DetectCgroupDelegation always returns (false) on non-Linux platforms.
func DetectCgroupDelegation() (dir string, controllers []string, ok bool) {
	return "", nil, false
}

// EnsureCgroupLeafHolder is a no-op on non-Linux platforms.
func EnsureCgroupLeafHolder() (parked bool, err error) {
	return false, nil
}

// Cgroup is a no-op cgroup handle on non-Linux platforms.
type Cgroup struct{}

// CreateCgroup returns (nil, nil) on non-Linux platforms (best-effort: no limits applied).
func CreateCgroup(name string, lim CgroupLimits) (*Cgroup, error) {
	return nil, nil
}

// Dir returns "" for a nil Cgroup.
func (c *Cgroup) Dir() string {
	if c == nil {
		return ""
	}
	return ""
}

// FD returns (-1, false) on non-Linux platforms.
func (c *Cgroup) FD() (int, bool) {
	return -1, false
}

// Stats always reports ok=false on non-Linux platforms.
func (c *Cgroup) Stats() (CgroupStats, bool) {
	return CgroupStats{}, false
}

// Add is a no-op on non-Linux platforms.
func (c *Cgroup) Add(pid int) error {
	return nil
}

// Close is a no-op on non-Linux platforms.
func (c *Cgroup) Close() error {
	return nil
}
