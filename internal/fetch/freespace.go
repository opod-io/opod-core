package fetch

import "syscall"

// freeGb is the space left on the volume holding dir, in whole GB. A prune
// stops as soon as the target is met rather than emptying a cache it can no
// longer refill cheaply.
func freeGb(dir string) (int, error) {
	_, free, err := volumeBytes(dir)
	if err != nil {
		return 0, err
	}
	return int(free / (1 << 30)), nil
}

// volumeBytes is the size and the free space of the volume holding dir. The
// prune reports both, because it is standing on the filesystem it was asked
// about: a manager that has to infer them from somewhere else reports 0 for a
// node whose own probe could not run — which is exactly the node that has run
// out of disk (measured on a design-partner node, 2026-09-21).
func volumeBytes(dir string) (total, free uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	return uint64(st.Blocks) * uint64(st.Bsize), uint64(st.Bavail) * uint64(st.Bsize), nil
}
