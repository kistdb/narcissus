package sqlite

import (
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

// links returns how many names the file has.
func links(path string, _ fs.FileInfo) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return 0, err
	}
	return uint64(info.NumberOfLinks), nil
}
