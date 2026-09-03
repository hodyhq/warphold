//go:build linux

package gateway

import "golang.org/x/sys/unix"

// etagXattr holds the object's hex MD5, written once at Put time so HEAD does
// not have to read the object back to answer with an ETag.
const etagXattr = "user.warphold.md5"

func setETag(fd uintptr, etag string) error {
	return unix.Fsetxattr(int(fd), etagXattr, []byte(etag), 0)
}

func getETag(fd uintptr) (string, error) {
	buf := make([]byte, 64)

	n, err := unix.Fgetxattr(int(fd), etagXattr, buf)
	if err != nil {
		return "", err
	}

	return string(buf[:n]), nil
}
