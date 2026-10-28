//go:build !unix

package geoip

import "os"

// mapFile 在不支持 mmap 的平台上直接按文件大小读入内存
func mapFile(path string) ([]byte, func() error, error) {
	data, err := os.ReadFile(path)
	return data, func() error { return nil }, err
}
