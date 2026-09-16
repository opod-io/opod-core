package fetch

import "syscall"

// freeGb is the space left on the volume holding dir, in whole GB. A prune
// stops as soon as the target is met rather than emptying a cache it can no
// longer refill cheaply.
func freeGb(dir string) (int, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int((uint64(st.Bavail) * uint64(st.Bsize)) / (1 << 30)), nil
}
