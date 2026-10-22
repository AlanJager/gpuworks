package runveilcli

import (
	"io"
	"os"
)

// tailChunk is how much of a log is read per step backwards from its end.
const tailChunk = 64 << 10

// printTail writes the last lines of path to w (all of it when lines < 0)
// and returns the offset it read up to, where following continues. Only the
// part printed is read, so a multi-gigabyte log costs no more than its tail.
func printTail(w io.Writer, path string, lines int) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	end := info.Size()
	start := int64(0)
	if lines >= 0 {
		start, err = tailStart(file, end, lines)
		if err != nil {
			return 0, err
		}
	}
	if _, err := io.Copy(w, io.NewSectionReader(file, start, end-start)); err != nil {
		return 0, err
	}
	return end, nil
}

// tailStart finds where the last `lines` lines of the first `end` bytes
// begin. A final line without a newline counts as a line.
func tailStart(file *os.File, end int64, lines int) (int64, error) {
	if lines == 0 {
		return end, nil
	}
	buf := make([]byte, tailChunk)
	pos := end
	newlines := 0
	// A trailing newline ends the last line rather than starting a new one.
	skipLast := true
	for pos > 0 {
		n := int64(len(buf))
		if pos < n {
			n = pos
		}
		pos -= n
		chunk := buf[:n]
		if _, err := file.ReadAt(chunk, pos); err != nil && err != io.EOF {
			return 0, err
		}
		for i := len(chunk) - 1; i >= 0; i-- {
			if chunk[i] != '\n' {
				skipLast = false
				continue
			}
			if skipLast {
				skipLast = false
				continue
			}
			newlines++
			if newlines == lines {
				return pos + int64(i) + 1, nil
			}
		}
	}
	return 0, nil
}

// copyFrom copies what path holds past offset and returns how much it copied
// and the new offset. A file that shrank was truncated or replaced: it is
// read again from the start.
func copyFrom(w io.Writer, path string, offset int64) (int64, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, offset, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, offset, err
	}
	if info.Size() < offset {
		offset = 0
	}
	if info.Size() == offset {
		return 0, offset, nil
	}
	n, err := io.Copy(w, io.NewSectionReader(file, offset, info.Size()-offset))
	return n, offset + n, err
}
