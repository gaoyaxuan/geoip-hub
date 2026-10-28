//go:build unix

package geoip

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// mapFile 以只读方式 mmap 整个文件：只有访问到的页才占用内存，内存紧张时可被系统回收
func mapFile(path string) ([]byte, func() error, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if fi.Size() == 0 {
		return nil, nil, fmt.Errorf("%s: 文件为空", path)
	}
	data, err := unix.Mmap(int(f.Fd()), 0, int(fi.Size()), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return nil, nil, fmt.Errorf("mmap %s: %w", path, err)
	}
	return data, func() error { return unix.Munmap(data) }, nil
}
